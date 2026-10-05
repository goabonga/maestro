// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package turn

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/state"
)

// Event is one recorded transition of a turn. Creation is recorded as
// a transition from the empty state to PREPARED.
type Event struct {
	ID     int64
	TurnID string
	From   State
	To     State
	Reason string
	At     time.Time
}

// Store persists turns and their events.
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

// Create prepares the first turn of an agent on a task. Its bounds are
// resolved from the stored configuration snapshot config_id and frozen
// in the turn: they are never read again from configuration files.
func (s Store) Create(taskID, agent, configID string) (Turn, error) {
	return s.create(taskID, agent, configID, "")
}

// Retry prepares a new turn that retries a terminal one: a new turn_id
// and attempt_id, the same task, agent and configuration, linked to
// the previous turn. A turn is never reused, and a turn is retried at
// most once; later retries chain from the latest turn.
func (s Store) Retry(previousID string) (Turn, error) {
	previous, err := s.Get(previousID)
	if err != nil {
		return Turn{}, err
	}
	if !previous.State.Terminal() {
		return Turn{}, fmt.Errorf("%w: %s is %s", ErrNotTerminal, previousID, previous.State)
	}
	return s.create(previous.TaskID, previous.Agent, previous.ConfigID, previous.ID)
}

// create inserts a PREPARED turn and its creation event in one
// transaction.
func (s Store) create(taskID, agent, configID, previousID string) (Turn, error) {
	if taskID == "" || agent == "" {
		return Turn{}, errors.New("a turn needs a task and an agent")
	}
	snapshot, err := config.LoadSnapshot(s.DB, configID)
	if err != nil {
		return Turn{}, err
	}
	at := s.now()
	created := Turn{
		ID: newID(), AttemptID: newID(), TaskID: taskID, Agent: agent, ConfigID: configID,
		PreviousID: previousID, State: Prepared, Timeouts: TimeoutsFor(snapshot.Config, agent),
		CreatedAt: at, UpdatedAt: at,
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return Turn{}, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO turns
		(turn_id, attempt_id, task_id, agent, config_id, previous_turn_id, state,
		 turn_timeout_ns, input_wait_timeout_ns, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		created.ID, created.AttemptID, taskID, agent, configID, nullable(previousID), string(Prepared),
		int64(created.Timeouts.Turn), int64(created.Timeouts.InputWait), stamp(at), stamp(at))
	if err != nil {
		if previousID != "" && strings.Contains(err.Error(), "UNIQUE constraint failed: turns.previous_turn_id") {
			return Turn{}, fmt.Errorf("%w: %s", ErrRetried, previousID)
		}
		return Turn{}, fmt.Errorf("create turn: %w", err)
	}
	if err := record(tx, created.ID, "", Prepared, "", at); err != nil {
		return Turn{}, err
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, err
	}
	return created, nil
}

// Transition moves a turn to a new state with the reason for it. The
// transition table is enforced, and the new state is stored with its
// event in one transaction: a concurrent transition of the same turn
// makes this one fail rather than overwrite it.
func (s Store) Transition(turnID string, to State, reason string) (Turn, error) {
	current, err := s.Get(turnID)
	if err != nil {
		return Turn{}, err
	}
	if err := check(current.State, to); err != nil {
		return Turn{}, err
	}
	at := s.now()
	next := current
	next.State, next.Reason, next.UpdatedAt = to, reason, at
	if to == Running && next.AdmittedAt.IsZero() {
		next.AdmittedAt = at
	}
	next.WaitingSince = time.Time{}
	if to == WaitingInput {
		next.WaitingSince = at
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return Turn{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE turns SET state = ?, reason = ?, updated_at = ?, admitted_at = ?, waiting_since = ?
		WHERE turn_id = ? AND state = ?`,
		string(to), reason, stamp(at), optional(next.AdmittedAt), optional(next.WaitingSince), turnID, string(current.State))
	if err != nil {
		return Turn{}, fmt.Errorf("transition turn %s: %w", turnID, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return Turn{}, err
	} else if changed != 1 {
		return Turn{}, fmt.Errorf("%w: %s changed state concurrently", ErrTransition, turnID)
	}
	if err := record(tx, turnID, current.State, to, reason, at); err != nil {
		return Turn{}, err
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, err
	}
	return next, nil
}

// Expire fails the turn when one of its bounds has passed at the
// store's clock, and reports whether it did.
func (s Store) Expire(turnID string) (Turn, bool, error) {
	current, err := s.Get(turnID)
	if err != nil {
		return Turn{}, false, err
	}
	expiry, expired := current.Expired(s.now())
	if !expired {
		return current, false, nil
	}
	failed, err := s.Transition(turnID, Failed, string(expiry))
	if err != nil {
		return Turn{}, false, err
	}
	return failed, true, nil
}

// Get returns a turn by id.
func (s Store) Get(turnID string) (Turn, error) {
	var t Turn
	var previous, admitted, waiting sql.NullString
	var created, updated, current string
	var turnTimeout, inputWait int64
	err := s.DB.QueryRow(`SELECT turn_id, attempt_id, task_id, agent, config_id, previous_turn_id, state,
		turn_timeout_ns, input_wait_timeout_ns, created_at, admitted_at, waiting_since, updated_at, reason
		FROM turns WHERE turn_id = ?`, turnID).Scan(
		&t.ID, &t.AttemptID, &t.TaskID, &t.Agent, &t.ConfigID, &previous, &current,
		&turnTimeout, &inputWait, &created, &admitted, &waiting, &updated, &t.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, fmt.Errorf("%w: %s", ErrNotFound, turnID)
	}
	if err != nil {
		return Turn{}, err
	}
	t.PreviousID, t.State = previous.String, State(current)
	t.Timeouts = Timeouts{Turn: time.Duration(turnTimeout), InputWait: time.Duration(inputWait)}
	for _, field := range []struct {
		raw    string
		target *time.Time
	}{{created, &t.CreatedAt}, {admitted.String, &t.AdmittedAt}, {waiting.String, &t.WaitingSince}, {updated, &t.UpdatedAt}} {
		if field.raw == "" {
			continue
		}
		if *field.target, err = time.Parse(time.RFC3339Nano, field.raw); err != nil {
			return Turn{}, err
		}
	}
	return t, nil
}

// Events returns the recorded transitions of a turn, oldest first.
func (s Store) Events(turnID string) ([]Event, error) {
	rows, err := s.DB.Query(`SELECT event_id, from_state, to_state, reason, at
		FROM turn_events WHERE turn_id = ? ORDER BY event_id`, turnID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []Event
	for rows.Next() {
		event := Event{TurnID: turnID}
		var from, to, at string
		if err := rows.Scan(&event.ID, &from, &to, &event.Reason, &at); err != nil {
			return nil, err
		}
		event.From, event.To = State(from), State(to)
		if event.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// record appends one event inside the caller's transaction.
func record(tx *sql.Tx, turnID string, from, to State, reason string, at time.Time) error {
	_, err := tx.Exec("INSERT INTO turn_events (turn_id, from_state, to_state, reason, at) VALUES (?, ?, ?, ?, ?)",
		turnID, string(from), string(to), reason, stamp(at))
	if err != nil {
		return fmt.Errorf("record turn event: %w", err)
	}
	return nil
}

// stamp renders an instant for storage.
func stamp(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}

// optional renders an instant, or NULL for the zero time.
func optional(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return stamp(at)
}

// nullable renders an id, or NULL when empty.
func nullable(id string) any {
	if id == "" {
		return nil
	}
	return id
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
