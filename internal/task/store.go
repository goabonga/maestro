// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package task

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/worktree"
)

// MaxDescription bounds the description of a task, in bytes.
const MaxDescription = 64 << 10

// ErrInvalid reports a task request missing its project or carrying an
// empty, oversized or non-UTF-8 description.
var ErrInvalid = errors.New("invalid task")

// Created is the event of a task's creation, recorded from the empty
// state to NEW.
const Created Event = "create"

// Record is one recorded event of a task: a transition, or a stale
// response logged without changing the state.
type Record struct {
	ID       int64
	TaskID   string
	Event    Event
	From     State
	To       State
	Revision string
	Stale    bool
	Reason   string
	At       time.Time
}

// Store persists tasks and their events.
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

// Create stores a NEW task of the project projectID, described by
// description, on the stored configuration snapshot configID, starting
// from the commit baseSHA on its branch maestro/task-<id>, with
// DefaultMaxFixCycles. The description is kept without its surrounding
// blanks.
func (s Store) Create(projectID, description, configID, baseSHA string) (Task, error) {
	description = strings.TrimSpace(description)
	switch {
	case projectID == "":
		return Task{}, fmt.Errorf("%w: no project", ErrInvalid)
	case description == "":
		return Task{}, fmt.Errorf("%w: empty description", ErrInvalid)
	case len(description) > MaxDescription:
		return Task{}, fmt.Errorf("%w: description exceeds %d bytes", ErrInvalid, MaxDescription)
	case !utf8.ValidString(description):
		return Task{}, fmt.Errorf("%w: description is not UTF-8 text", ErrInvalid)
	}
	if !objectID.MatchString(baseSHA) {
		return Task{}, fmt.Errorf("task base %q is not a commit", baseSHA)
	}
	if _, err := config.LoadSnapshot(s.DB, configID); err != nil {
		return Task{}, err
	}
	at := s.now()
	id := newID()
	created := Task{
		ID: id, ProjectID: projectID, Description: description, ConfigID: configID, BaseSHA: baseSHA, Branch: worktree.TaskBranch(id),
		State: New, Version: 1, MaxFixCycles: DefaultMaxFixCycles, CreatedAt: at, UpdatedAt: at,
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO tasks
		(task_id, project_id, description, config_id, task_base_sha, branch, state, version, max_fix_cycles,
		created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, projectID, description, configID, baseSHA, created.Branch, string(New), created.Version, created.MaxFixCycles, stamp(at), stamp(at))
	if err != nil {
		return Task{}, fmt.Errorf("create task: %w", err)
	}
	if err := record(tx, Record{TaskID: id, Event: Created, To: New, At: at}); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return created, nil
}

// Transition applies one event to a task. The new state is stored with
// its event in one transaction, and only if the task is still at the
// state and version that were read: a concurrent transition makes this
// one fail with ErrTransition rather than overwrite it. A refused event
// leaves no trace; a stale one is recorded without changing the task
// and fails with ErrStale.
func (s Store) Transition(taskID string, in Input) (Task, error) {
	current, err := s.Get(taskID)
	if err != nil {
		return Task{}, err
	}
	at := s.now()
	next, err := Apply(current, in, at)
	if errors.Is(err, ErrStale) {
		if logErr := s.logStale(current, in, at); logErr != nil {
			return Task{}, logErr
		}
		return current, err
	}
	if err != nil {
		return current, err
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE tasks SET state = ?, version = ?, resume_state = ?, blocked_reason = ?,
		head_sha = ?, approved_sha = ?, result_sha = ?, fix_cycles = ?, conflict_base = ?,
		conflict_failures = ?, resolution_approved_sha = ?, updated_at = ?, reason = ?
		WHERE task_id = ? AND state = ? AND version = ?`,
		string(next.State), next.Version, string(next.ResumeState), next.BlockedReason,
		next.HeadSHA, next.ApprovedSHA, next.ResultSHA, next.FixCycles, next.ConflictBase,
		next.ConflictFailures, next.ResolutionApprovedSHA, stamp(at), next.Reason,
		taskID, string(current.State), current.Version)
	if err != nil {
		return Task{}, fmt.Errorf("transition task %s: %w", taskID, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return Task{}, err
	} else if changed != 1 {
		return Task{}, fmt.Errorf("%w: %s changed concurrently", ErrTransition, taskID)
	}
	if err := record(tx, Record{
		TaskID: taskID, Event: in.Event, From: current.State, To: next.State,
		Revision: in.Revision, Reason: in.Reason, At: at,
	}); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return next, nil
}

// logStale records a stale response without touching the task.
func (s Store) logStale(current Task, in Input, at time.Time) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := record(tx, Record{
		TaskID: current.ID, Event: in.Event, From: current.State, To: current.State,
		Revision: in.Revision, Stale: true, Reason: in.Reason, At: at,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// columns are the stored fields of a task, in scan order.
const columns = `task_id, project_id, description, config_id, task_base_sha, branch, state, version,
	resume_state, blocked_reason, head_sha, approved_sha, result_sha, fix_cycles, max_fix_cycles,
	conflict_base, conflict_failures, resolution_approved_sha, created_at, updated_at, reason`

// scan reads one task row selected with columns.
func scan(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var current, resume, created, updated string
	err := row.Scan(
		&t.ID, &t.ProjectID, &t.Description, &t.ConfigID, &t.BaseSHA, &t.Branch, &current, &t.Version,
		&resume, &t.BlockedReason, &t.HeadSHA, &t.ApprovedSHA, &t.ResultSHA, &t.FixCycles, &t.MaxFixCycles,
		&t.ConflictBase, &t.ConflictFailures, &t.ResolutionApprovedSHA, &created, &updated, &t.Reason)
	if err != nil {
		return Task{}, err
	}
	t.State, t.ResumeState = State(current), State(resume)
	if t.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Task{}, err
	}
	if t.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return Task{}, err
	}
	return t, nil
}

// Get returns a task by id.
func (s Store) Get(taskID string) (Task, error) {
	t, err := scan(s.DB.QueryRow(`SELECT `+columns+` FROM tasks WHERE task_id = ?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("%w: %s", ErrNotFound, taskID)
	}
	return t, err
}

// List returns the tasks of a project, oldest first.
func (s Store) List(projectID string) ([]Task, error) {
	rows, err := s.DB.Query(`SELECT `+columns+` FROM tasks WHERE project_id = ?
		ORDER BY created_at, task_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tasks []Task
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// Events returns the recorded events of a task, oldest first.
func (s Store) Events(taskID string) ([]Record, error) {
	rows, err := s.DB.Query(`SELECT event_id, event, from_state, to_state, revision, stale, reason, at
		FROM task_events WHERE task_id = ? ORDER BY event_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []Record
	for rows.Next() {
		r := Record{TaskID: taskID}
		var event, from, to, at string
		if err := rows.Scan(&r.ID, &event, &from, &to, &r.Revision, &r.Stale, &r.Reason, &at); err != nil {
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
	_, err := tx.Exec(`INSERT INTO task_events (task_id, event, from_state, to_state, revision, stale, reason, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TaskID, string(r.Event), string(r.From), string(r.To), r.Revision, r.Stale, r.Reason, stamp(r.At))
	if err != nil {
		return fmt.Errorf("record task event: %w", err)
	}
	return nil
}

// stamp renders an instant for storage.
func stamp(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}

// newID returns a random UUID version 4.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
