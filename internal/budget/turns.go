// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package budget

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ErrKeyConflict reports a reservation key already reserved for
// another agent of the task.
var ErrKeyConflict = errors.New("reservation key already reserved for another agent")

// BoundTaskTurns is the turn cap of a task, all roles together.
const BoundTaskTurns = "budgets.max_turns_per_task"

// AgentTurnsBound names the turn cap of one named agent on a task.
func AgentTurnsBound(agent string) string {
	return "agents." + agent + ".max_turns_per_task"
}

// Reservation is one turn reserved for an agent of a task before its
// prompt is sent.
type Reservation struct {
	TaskID string
	// Key identifies the send: a retry of the same send carries the
	// same key and reserves no second turn.
	Key        string
	Agent      string
	ReservedAt time.Time
	// Replayed reports a key that was already reserved: the existing
	// reservation is returned and no turn is consumed.
	Replayed bool
}

// Turns are the turns a task consumed, in total and per named agent.
type Turns struct {
	Task   int
	Agents map[string]int
}

// Reserve consumes one turn of a task for an agent, before its prompt
// is sent. The reservation is checked against the task's total cap
// (budgets.max_turns_per_task) and the agent's own cap
// (agents.<name>.max_turns_per_task, when set) of the task's
// configuration snapshot, and stored in the same statement, so two
// concurrent reservations never both take the last turn. Every role,
// repair, resolution and retry reserves its own turn. A reservation is
// never refunded, even when the send that follows is lost in a crash.
// Reserving the same key again returns the first reservation with
// Replayed set and consumes nothing; the same key for another agent
// fails with ErrKeyConflict. A reached cap fails with an
// *ExceededError naming it.
func (s Store) Reserve(taskID, agent, key string) (Reservation, error) {
	if agent == "" || key == "" {
		return Reservation{}, fmt.Errorf("a turn reservation needs an agent and a key")
	}
	_, cfg, err := s.load(taskID)
	if err != nil {
		return Reservation{}, err
	}
	if existing, found, err := s.reservation(taskID, agent, key); err != nil || found {
		return existing, err
	}
	total, perAgent := cfg.Budgets.MaxTurnsPerTask, cfg.Agents[agent].MaxTurnsPerTask
	at := s.now().UTC()
	result, err := s.DB.Exec(`INSERT INTO budget_turns (task_id, reservation_key, agent, reserved_at)
		SELECT ?, ?, ?, ?
		WHERE (SELECT COUNT(*) FROM budget_turns WHERE task_id = ?) < ?
		AND (? = 0 OR (SELECT COUNT(*) FROM budget_turns WHERE task_id = ? AND agent = ?) < ?)
		ON CONFLICT (task_id, reservation_key) DO NOTHING`,
		taskID, key, agent, stamp(at), taskID, total, perAgent, taskID, agent, perAgent)
	if err != nil {
		return Reservation{}, fmt.Errorf("reserve a turn of task %s: %w", taskID, err)
	}
	if inserted, err := result.RowsAffected(); err != nil {
		return Reservation{}, err
	} else if inserted == 1 {
		return Reservation{TaskID: taskID, Key: key, Agent: agent, ReservedAt: at}, nil
	}
	// Nothing was stored: a concurrent retry of the same send won, or a
	// cap is reached.
	if existing, found, err := s.reservation(taskID, agent, key); err != nil || found {
		return existing, err
	}
	used, err := s.Turns(taskID)
	if err != nil {
		return Reservation{}, err
	}
	if used.Task >= total {
		return Reservation{}, &ExceededError{TaskID: taskID, Bound: BoundTaskTurns,
			Limit: strconv.Itoa(total), Used: strconv.Itoa(used.Task)}
	}
	if perAgent > 0 && used.Agents[agent] >= perAgent {
		return Reservation{}, &ExceededError{TaskID: taskID, Bound: AgentTurnsBound(agent),
			Limit: strconv.Itoa(perAgent), Used: strconv.Itoa(used.Agents[agent])}
	}
	return Reservation{}, fmt.Errorf("reserve a turn of task %s: no turn stored and no cap reached", taskID)
}

// reservation returns the stored reservation of a key, if any, marked
// as replayed. A key reserved for another agent fails with
// ErrKeyConflict.
func (s Store) reservation(taskID, agent, key string) (Reservation, bool, error) {
	var stored, at string
	err := s.DB.QueryRow("SELECT agent, reserved_at FROM budget_turns WHERE task_id = ? AND reservation_key = ?",
		taskID, key).Scan(&stored, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Reservation{}, false, nil
	}
	if err != nil {
		return Reservation{}, false, err
	}
	if stored != agent {
		return Reservation{}, false, fmt.Errorf("%w: key %q of task %s belongs to %s, not %s",
			ErrKeyConflict, key, taskID, stored, agent)
	}
	reservedAt, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return Reservation{}, false, err
	}
	return Reservation{TaskID: taskID, Key: key, Agent: agent, ReservedAt: reservedAt, Replayed: true}, true, nil
}

// Turns returns the turns a task consumed, in total and per agent.
func (s Store) Turns(taskID string) (Turns, error) {
	rows, err := s.DB.Query("SELECT agent, COUNT(*) FROM budget_turns WHERE task_id = ? GROUP BY agent", taskID)
	if err != nil {
		return Turns{}, err
	}
	defer func() { _ = rows.Close() }()
	turns := Turns{Agents: map[string]int{}}
	for rows.Next() {
		var agent string
		var count int
		if err := rows.Scan(&agent, &count); err != nil {
			return Turns{}, err
		}
		turns.Agents[agent] = count
		turns.Task += count
	}
	return turns, rows.Err()
}
