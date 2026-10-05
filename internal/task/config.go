// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package task

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/turn"
)

// UpdateConfig is the event of a task adopting a new configuration
// snapshot. It is not a row of the transition table: it is applied by
// Store.UpdateConfig, which checks quiescence in the database.
const UpdateConfig Event = "config-update"

// ConfigUpdate asks a task to adopt another stored snapshot.
type ConfigUpdate struct {
	// ConfigID is the stored snapshot to adopt.
	ConfigID string
	// Published: the atomic publication of the task succeeded. An
	// update is refused once it holds.
	Published bool
	// OperationSettled: no integration operation is pending for the
	// task.
	OperationSettled bool
}

// ConfigUpdated is the outcome of an update.
type ConfigUpdated struct {
	Task Task
	// Previous is the config_id the task ran on before the update.
	Previous string
	// Changes are the differences between the two snapshots; empty when
	// the task already runs on the requested snapshot, in which case
	// nothing is stored.
	Changes config.Changes
}

// ConfigRecord is one journaled update of a task's configuration.
type ConfigRecord struct {
	EventID  int64
	TaskID   string
	Previous string
	ConfigID string
	Impact   config.Impact
	Changes  config.Changes
}

// progress orders the states that carry work: an update moves a task
// back to a state of lower progress, never forward. BLOCKED and the
// terminal states are absent.
var progress = map[State]int{
	New: 0, Planning: 1, Implementing: 2, Testing: 3, Fixing: 4, Reviewing: 4, ReadyToIntegrate: 5,
	Integrating: 6, MergeConflict: 6, Validating: 6,
}

// restart is the state an update of the given impact continues from:
// PLANNING at the latest for an objective change, TESTING at the latest
// for a verification change, the current state for a raised ceiling.
func restart(current State, impact config.Impact) State {
	target := current
	switch impact {
	case config.ImpactObjective:
		target = Planning
	case config.ImpactVerification:
		target = Testing
	default:
		return current
	}
	if progress[current] <= progress[target] {
		return current
	}
	return target
}

// AdoptConfig evaluates the adoption of the snapshot configID by a
// task, given the changes between its snapshot and that one. A task
// that is DONE, CANCELLED or published, or with an integration
// operation pending, is refused with ErrGuard. The task continues from
// PLANNING at the latest when the changes touch its objectives, from
// TESTING at the latest when they only touch its verifications, and
// keeps its state when they only raise ceilings; a blocked task keeps
// BLOCKED and its continuation moves instead. Any change but a raised
// ceiling invalidates the results the task holds under the old
// snapshot: its approved revision, its integration candidate and its
// approved conflict resolution. Consumed budgets — fix cycles and
// conflict failures included — are kept.
func AdoptConfig(t Task, configID string, changes config.Changes, in ConfigUpdate, at time.Time) (Task, error) {
	switch {
	case t.State.Terminal():
		return t, fmt.Errorf("%s in %s: %w", UpdateConfig, t.State, guardError("the task has ended"))
	case in.Published:
		return t, fmt.Errorf("%s in %s: %w", UpdateConfig, t.State, guardError("the publication already succeeded"))
	case !in.OperationSettled:
		return t, fmt.Errorf("%s in %s: %w", UpdateConfig, t.State, guardError("an integration operation is pending"))
	case configID == "" || configID == t.ConfigID:
		return t, fmt.Errorf("%s: %w", UpdateConfig, guardError("no other snapshot to adopt"))
	}
	impact := changes.Impact()
	next := t
	next.ConfigID = configID
	if t.State == Blocked {
		next.ResumeState = restart(t.ResumeState, impact)
	} else {
		next.State = restart(t.State, impact)
	}
	if impact == config.ImpactVerification || impact == config.ImpactObjective {
		next.ApprovedSHA, next.ResultSHA, next.ResolutionApprovedSHA = "", "", ""
	}
	next.Version++
	next.Reason = fmt.Sprintf("configuration %s replaces %s: %s", configID, t.ConfigID, changes)
	next.UpdatedAt = at
	return next, nil
}

// UpdateConfig makes a task adopt the stored snapshot in.ConfigID and
// returns the changes it brings. A task already on that snapshot is
// returned unchanged with no change and nothing stored. Otherwise the
// task must be quiescent: no turn of the task may be outside a terminal
// state, and no active time interval of the task may be open (the
// caller reconciles a crashed one first, which charges it); the turn
// reservations and the time already consumed stay as they are. The
// update is stored with its event and its journal entry — the old and
// new config_id and the changes — in one compare-and-set transaction,
// so a concurrent transition or update makes it fail with
// ErrTransition.
func (s Store) UpdateConfig(taskID string, in ConfigUpdate) (ConfigUpdated, error) {
	current, err := s.Get(taskID)
	if err != nil {
		return ConfigUpdated{}, err
	}
	outcome := ConfigUpdated{Task: current, Previous: current.ConfigID}
	if in.ConfigID == current.ConfigID {
		if current.State.Terminal() {
			return outcome, fmt.Errorf("%s in %s: %w", UpdateConfig, current.State, guardError("the task has ended"))
		}
		return outcome, nil
	}
	old, err := config.LoadSnapshot(s.DB, current.ConfigID)
	if err != nil {
		return outcome, err
	}
	adopted, err := config.LoadSnapshot(s.DB, in.ConfigID)
	if err != nil {
		return outcome, err
	}
	changes := config.Diff(old, adopted)
	at := s.now()
	next, err := AdoptConfig(current, in.ConfigID, changes, in, at)
	if err != nil {
		return outcome, err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return outcome, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := quiescent(tx, taskID); err != nil {
		return outcome, err
	}
	result, err := tx.Exec(`UPDATE tasks SET config_id = ?, state = ?, version = ?, resume_state = ?,
		approved_sha = ?, result_sha = ?, resolution_approved_sha = ?, updated_at = ?, reason = ?
		WHERE task_id = ? AND state = ? AND version = ? AND config_id = ?`,
		next.ConfigID, string(next.State), next.Version, string(next.ResumeState),
		next.ApprovedSHA, next.ResultSHA, next.ResolutionApprovedSHA, stamp(at), next.Reason,
		taskID, string(current.State), current.Version, current.ConfigID)
	if err != nil {
		return outcome, fmt.Errorf("update the configuration of task %s: %w", taskID, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return outcome, err
	} else if changed != 1 {
		return outcome, fmt.Errorf("%w: %s changed concurrently", ErrTransition, taskID)
	}
	if err := record(tx, Record{
		TaskID: taskID, Event: UpdateConfig, From: current.State, To: next.State, Reason: next.Reason, At: at,
	}); err != nil {
		return outcome, err
	}
	encoded, err := json.Marshal(changes)
	if err != nil {
		return outcome, err
	}
	if _, err := tx.Exec(`INSERT INTO task_config_updates (event_id, task_id, old_config_id, new_config_id, impact, changes)
		VALUES (last_insert_rowid(), ?, ?, ?, ?, ?)`,
		taskID, current.ConfigID, next.ConfigID, string(changes.Impact()), string(encoded)); err != nil {
		return outcome, fmt.Errorf("journal the configuration update of task %s: %w", taskID, err)
	}
	if err := tx.Commit(); err != nil {
		return outcome, err
	}
	return ConfigUpdated{Task: next, Previous: current.ConfigID, Changes: changes}, nil
}

// quiescent refuses an update while a turn of the task has not ended or
// an active time interval of the task is open.
func quiescent(tx *sql.Tx, taskID string) error {
	var running int
	err := tx.QueryRow(`SELECT COUNT(*) FROM turns WHERE task_id = ? AND state NOT IN (?, ?, ?)`,
		taskID, string(turn.Succeeded), string(turn.Interrupted), string(turn.Failed)).Scan(&running)
	if err != nil {
		return err
	}
	if running > 0 {
		return fmt.Errorf("%s: %w", UpdateConfig, guardError(fmt.Sprintf("%d turn(s) of the task have not ended", running)))
	}
	var step string
	err = tx.QueryRow(`SELECT open_step FROM budget_time WHERE task_id = ?`, taskID).Scan(&step)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if step != "" {
		return fmt.Errorf("%s: %w", UpdateConfig, guardError("the active interval of step "+step+" is open"))
	}
	return nil
}

// ConfigUpdates returns the journaled configuration updates of a task,
// oldest first.
func (s Store) ConfigUpdates(taskID string) ([]ConfigRecord, error) {
	rows, err := s.DB.Query(`SELECT event_id, old_config_id, new_config_id, impact, changes
		FROM task_config_updates WHERE task_id = ? ORDER BY event_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []ConfigRecord
	for rows.Next() {
		r := ConfigRecord{TaskID: taskID}
		var impact, changes string
		if err := rows.Scan(&r.EventID, &r.Previous, &r.ConfigID, &impact, &changes); err != nil {
			return nil, err
		}
		r.Impact = config.Impact(impact)
		if err := json.Unmarshal([]byte(changes), &r.Changes); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}
