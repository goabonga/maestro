// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/worktree"
)

// Registered is the event of a worker's registration, recorded from the
// empty state to STOPPED.
const Registered Event = "register"

// MaxName bounds the name of a worker, in bytes.
const MaxName = 64

// validName constrains worker names to safe identifiers, usable as path
// components of the worker's private repository.
var validName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Spec describes a worker to register.
type Spec struct {
	Name      string
	Agent     string
	AgentKind string
	Driver    string
}

// Record is one recorded event of a worker.
type Record struct {
	ID        int64
	ProjectID string
	Name      string
	Event     Event
	From      State
	To        State
	// TaskID and TurnID name the assignment the event is about, when
	// the worker held or took one.
	TaskID string
	TurnID string
	Reason string
	At     time.Time
}

// Store persists workers and their events.
type Store struct {
	DB *state.DB
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// now reads the store's clock in UTC.
func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Register stores a STOPPED worker of the project, with the path of its
// private repository in that project. A name already registered in the
// project fails with ErrExists.
func (s Store) Register(project worktree.Project, spec Spec) (Worker, error) {
	switch {
	case project.ID == "":
		return Worker{}, fmt.Errorf("%w: no project", ErrInvalid)
	case len(spec.Name) > MaxName || !validName.MatchString(spec.Name):
		return Worker{}, fmt.Errorf("%w: name %q is not a safe identifier", ErrInvalid, spec.Name)
	case spec.Agent == "" || spec.AgentKind == "" || spec.Driver == "":
		return Worker{}, fmt.Errorf("%w: a worker needs an agent, its kind and a driver", ErrInvalid)
	}
	at := s.now()
	registered := Worker{
		ProjectID: project.ID, Name: spec.Name, Agent: spec.Agent, AgentKind: spec.AgentKind, Driver: spec.Driver,
		Repository: project.WorkerRepository(spec.Name), State: Stopped, Version: 1, CreatedAt: at, UpdatedAt: at,
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return Worker{}, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO workers
		(project_id, name, agent, agent_kind, driver, repository, state, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		registered.ProjectID, registered.Name, registered.Agent, registered.AgentKind, registered.Driver,
		registered.Repository, string(Stopped), registered.Version, stamp(at), stamp(at))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: workers.project_id, workers.name") {
			return Worker{}, fmt.Errorf("%w: %s in project %s", ErrExists, spec.Name, project.ID)
		}
		return Worker{}, fmt.Errorf("register worker: %w", err)
	}
	if err := record(tx, Record{ProjectID: project.ID, Name: spec.Name, Event: Registered, To: Stopped, At: at}); err != nil {
		return Worker{}, err
	}
	if err := tx.Commit(); err != nil {
		return Worker{}, err
	}
	return registered, nil
}

// unregister removes a worker a refused start registered, with its
// events: only a STOPPED or STARTING worker without assignment.
func (s Store) unregister(projectID, workerName string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM worker_events WHERE project_id = ? AND name = ?`, projectID, workerName); err != nil {
		return err
	}
	result, err := tx.Exec(`DELETE FROM workers WHERE project_id = ? AND name = ?
		AND state IN ('STOPPED', 'STARTING') AND turn_id IS NULL`, projectID, workerName)
	if err != nil {
		return err
	}
	if removed, err := result.RowsAffected(); err != nil {
		return err
	} else if removed != 1 {
		return fmt.Errorf("%w: %s cannot be unregistered", ErrTransition, workerName)
	}
	return tx.Commit()
}

// Transition applies one event to a worker. The new state and the
// assignment are stored with the event in one transaction, and only if
// the worker is still at the state and version that were read: a
// concurrent transition makes this one fail with ErrTransition rather
// than overwrite it. An assignment must name a stored turn of its task
// for the worker's agent, and a turn is assigned to one worker at most:
// another worker holding it fails with ErrAssigned. A refused event
// leaves no trace.
func (s Store) Transition(projectID, workerName string, in Input) (Worker, error) {
	current, err := s.Get(projectID, workerName)
	if err != nil {
		return Worker{}, err
	}
	return s.transitionFrom(current, in)
}

// transitionFrom applies one event to a worker as it was read: the
// transition is stored only if the worker is still at current's state
// and version, and fails with ErrTransition otherwise.
func (s Store) transitionFrom(current Worker, in Input) (Worker, error) {
	at := s.now()
	next, err := Apply(current, in, at)
	if err != nil {
		return current, err
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return Worker{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if in.Event == Assign {
		if err := checkTurn(tx, next); err != nil {
			return current, err
		}
	}
	if err := write(tx, current, next, in, at); err != nil {
		return current, err
	}
	if err := tx.Commit(); err != nil {
		return Worker{}, err
	}
	return next, nil
}

// checkTurn verifies that the assigned turn is stored, belongs to the
// assigned task and runs the worker's agent.
func checkTurn(tx *sql.Tx, w Worker) error {
	var taskID, agent string
	err := tx.QueryRow(`SELECT task_id, agent FROM turns WHERE turn_id = ?`, w.Assignment.TurnID).Scan(&taskID, &agent)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: unknown turn %s", ErrInvalid, w.Assignment.TurnID)
	}
	if err != nil {
		return err
	}
	if taskID != w.Assignment.TaskID {
		return fmt.Errorf("%w: turn %s belongs to task %s, not %s", ErrInvalid, w.Assignment.TurnID, taskID, w.Assignment.TaskID)
	}
	if agent != w.Agent {
		return fmt.Errorf("%w: turn %s runs agent %s, not %s", ErrInvalid, w.Assignment.TurnID, agent, w.Agent)
	}
	return nil
}

// write stores the transition of a worker from current to next with its
// event, inside tx, only if the worker is still at current's state and
// version: a concurrent transition fails with ErrTransition.
func write(tx *sql.Tx, current, next Worker, in Input, at time.Time) error {
	var taskID, turnID any
	role := ""
	if next.Assignment != nil {
		taskID, role, turnID = next.Assignment.TaskID, string(next.Assignment.Role), next.Assignment.TurnID
	}
	result, err := tx.Exec(`UPDATE workers SET state = ?, version = ?, task_id = ?, role = ?, turn_id = ?,
		updated_at = ?, reason = ?
		WHERE project_id = ? AND name = ? AND state = ? AND version = ?`,
		string(next.State), next.Version, taskID, role, turnID, stamp(at), next.Reason,
		current.ProjectID, current.Name, string(current.State), current.Version)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: workers.turn_id") {
			return fmt.Errorf("%w: %s", ErrAssigned, next.Assignment.TurnID)
		}
		return fmt.Errorf("transition worker %s: %w", current.Name, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return fmt.Errorf("%w: %s changed concurrently", ErrTransition, current.Name)
	}
	r := Record{ProjectID: current.ProjectID, Name: current.Name, Event: in.Event, From: current.State, To: next.State,
		Reason: next.Reason, At: at}
	switch {
	case next.Assignment != nil:
		r.TaskID, r.TurnID = next.Assignment.TaskID, next.Assignment.TurnID
	case current.Assignment != nil:
		r.TaskID, r.TurnID = current.Assignment.TaskID, current.Assignment.TurnID
	}
	return record(tx, r)
}

// columns are the stored fields of a worker, in scan order.
const columns = `project_id, name, agent, agent_kind, driver, repository, state, version,
	task_id, role, turn_id, created_at, updated_at, reason`

// scan reads one worker row selected with columns.
func scan(row interface{ Scan(...any) error }) (Worker, error) {
	var w Worker
	var current, role, created, updated string
	var taskID, turnID sql.NullString
	err := row.Scan(&w.ProjectID, &w.Name, &w.Agent, &w.AgentKind, &w.Driver, &w.Repository, &current, &w.Version,
		&taskID, &role, &turnID, &created, &updated, &w.Reason)
	if err != nil {
		return Worker{}, err
	}
	w.State = State(current)
	if taskID.Valid {
		w.Assignment = &Assignment{TaskID: taskID.String, Role: Role(role), TurnID: turnID.String}
	}
	if w.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Worker{}, err
	}
	if w.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return Worker{}, err
	}
	return w, nil
}

// Get returns a worker of a project by name.
func (s Store) Get(projectID, workerName string) (Worker, error) {
	w, err := scan(s.DB.QueryRow(`SELECT `+columns+` FROM workers WHERE project_id = ? AND name = ?`,
		projectID, workerName))
	if errors.Is(err, sql.ErrNoRows) {
		return Worker{}, fmt.Errorf("%w: %s in project %s", ErrNotFound, workerName, projectID)
	}
	return w, err
}

// List returns the workers of a project, by name.
func (s Store) List(projectID string) ([]Worker, error) {
	rows, err := s.DB.Query(`SELECT `+columns+` FROM workers WHERE project_id = ? ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var workers []Worker
	for rows.Next() {
		w, err := scan(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, rows.Err()
}

// Events returns the recorded events of a worker, oldest first.
func (s Store) Events(projectID, workerName string) ([]Record, error) {
	rows, err := s.DB.Query(`SELECT event_id, event, from_state, to_state, task_id, turn_id, reason, at
		FROM worker_events WHERE project_id = ? AND name = ? ORDER BY event_id`, projectID, workerName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []Record
	for rows.Next() {
		r := Record{ProjectID: projectID, Name: workerName}
		var event, from, to, at string
		if err := rows.Scan(&r.ID, &event, &from, &to, &r.TaskID, &r.TurnID, &r.Reason, &at); err != nil {
			return nil, err
		}
		r.Event, r.From, r.To = Event(event), State(from), State(to)
		if r.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// record appends one event inside the caller's transaction.
func record(tx *sql.Tx, r Record) error {
	_, err := tx.Exec(`INSERT INTO worker_events (project_id, name, event, from_state, to_state, task_id, turn_id, reason, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ProjectID, r.Name, string(r.Event), string(r.From), string(r.To), r.TaskID, r.TurnID, r.Reason, stamp(r.At))
	if err != nil {
		return fmt.Errorf("record worker event: %w", err)
	}
	return nil
}

// stamp renders an instant for storage.
func stamp(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}
