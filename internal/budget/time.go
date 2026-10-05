// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package budget

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/task"
)

// Errors of the time budgets.
var (
	// ErrClockRegression reports a clock that went backwards against
	// the recorded timestamps: durations cannot be trusted.
	ErrClockRegression = errors.New("clock regression")
	// ErrNotActive reports a task outside the active steps of the
	// workflow.
	ErrNotActive = errors.New("the task is not in an active step")
	// ErrIntervalOpen reports a task whose active interval is still
	// open: it is stopped, or recovered after a crash, first.
	ErrIntervalOpen = errors.New("an active interval is already open")
	// ErrNoInterval reports an interval that is no longer open.
	ErrNoInterval = errors.New("no active interval is open")
)

// The time bounds of a task.
const (
	// BoundTaskTimeout bounds the cumulated active time.
	BoundTaskTimeout = "budgets.task_timeout"
	// BoundWallTimeout bounds the calendar time since creation.
	BoundWallTimeout = "budgets.wall_timeout"
)

// Step is an active step of a task: the time spent in it counts
// against budgets.task_timeout.
type Step string

// The active steps.
const (
	StepPlanning    Step = "planning"
	StepCoding      Step = "coding"
	StepTests       Step = "tests"
	StepReview      Step = "review"
	StepIntegration Step = "integration"
)

// StepOf returns the active step of a task state. NEW, the wait for
// integration, BLOCKED and the terminal states are not active.
func StepOf(s task.State) (Step, bool) {
	switch s {
	case task.Planning:
		return StepPlanning, true
	case task.Implementing, task.Fixing:
		return StepCoding, true
	case task.Testing:
		return StepTests, true
	case task.Reviewing:
		return StepReview, true
	case task.Integrating, task.MergeConflict, task.Validating:
		return StepIntegration, true
	}
	return "", false
}

// Usage is the time a task used.
type Usage struct {
	// Active is the cumulated active time, the open interval included.
	Active time.Duration
	// Wall is the calendar time since the task's creation.
	Wall time.Duration
	// Open is the step of the open interval; empty when none is open.
	Open Step
}

// Interval is one open active interval of a task. Its duration is
// measured in memory from the store's clock and persisted at each
// boundary: Checkpoint and Stop.
type Interval struct {
	store  Store
	taskID string
	step   Step
	// started is the clock reading at the start, monotonic reading
	// included.
	started time.Time
	// since and mark are the stored start and last boundary.
	since, mark time.Time
	closed      bool
}

// clockRow is the stored time account of a task.
type clockRow struct {
	active  time.Duration
	step    Step
	since   time.Time
	mark    time.Time
	openNS  time.Duration
	updated time.Time
}

// readClock returns the stored time account of a task; a task without
// one has used no active time.
func (s Store) readClock(taskID string) (clockRow, bool, error) {
	var row clockRow
	var active, openNS int64
	var step, since, mark, updated string
	err := s.DB.QueryRow(`SELECT active_ns, open_step, open_since, open_mark, open_ns, updated_at
		FROM budget_time WHERE task_id = ?`, taskID).Scan(&active, &step, &since, &mark, &openNS, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return clockRow{}, false, nil
	}
	if err != nil {
		return clockRow{}, false, err
	}
	row.active, row.step, row.openNS = time.Duration(active), Step(step), time.Duration(openNS)
	if row.updated, err = parse(updated); err != nil {
		return clockRow{}, false, err
	}
	if row.step != "" {
		if row.since, err = parse(since); err != nil {
			return clockRow{}, false, err
		}
		if row.mark, err = parse(mark); err != nil {
			return clockRow{}, false, err
		}
	}
	return row, true, nil
}

// Start opens an active interval for a task in an active step. The
// task's time bounds are checked first: a task with no time left fails
// with an *ExceededError. A task whose previous interval is still open
// fails with ErrIntervalOpen: after a crash, Recover charges it first.
func (s Store) Start(taskID string) (*Interval, error) {
	current, cfg, err := s.load(taskID)
	if err != nil {
		return nil, err
	}
	step, ok := StepOf(current.State)
	if !ok {
		return nil, fmt.Errorf("%w: task %s is %s", ErrNotActive, taskID, current.State)
	}
	reading := s.now()
	at := reading.UTC()
	row, _, err := s.readClock(taskID)
	if err != nil {
		return nil, err
	}
	if row.step != "" {
		return nil, fmt.Errorf("%w: task %s, step %s since %s", ErrIntervalOpen, taskID, row.step, stamp(row.since))
	}
	if err := regression(taskID, at, current.CreatedAt, row.updated); err != nil {
		return nil, err
	}
	if err := exceeded(taskID, cfg.Budgets, Usage{Active: row.active, Wall: at.Sub(current.CreatedAt)}); err != nil {
		return nil, err
	}
	result, err := s.DB.Exec(`INSERT INTO budget_time (task_id, open_step, open_since, open_mark, open_ns, updated_at)
		VALUES (?, ?, ?, ?, 0, ?)
		ON CONFLICT (task_id) DO UPDATE SET open_step = excluded.open_step, open_since = excluded.open_since,
		open_mark = excluded.open_mark, open_ns = 0, updated_at = excluded.updated_at
		WHERE budget_time.open_step = ''`,
		taskID, string(step), stamp(at), stamp(at), stamp(at))
	if err != nil {
		return nil, fmt.Errorf("start the active interval of task %s: %w", taskID, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return nil, err
	} else if changed != 1 {
		return nil, fmt.Errorf("%w: task %s", ErrIntervalOpen, taskID)
	}
	return &Interval{store: s, taskID: taskID, step: step, started: reading, since: at, mark: at}, nil
}

// Step returns the step of the interval.
func (i *Interval) Step() Step {
	return i.step
}

// Checkpoint persists the time elapsed so far in the interval, which
// stays open, and checks the task's time bounds: a reached bound fails
// with an *ExceededError, the time being recorded all the same.
func (i *Interval) Checkpoint() (Usage, error) {
	return i.boundary(false)
}

// Stop closes the interval: its duration is added to the task's active
// time, then the task's time bounds are checked as by Checkpoint.
func (i *Interval) Stop() (Usage, error) {
	return i.boundary(true)
}

// boundary persists the interval, closing it when stop is set.
func (i *Interval) boundary(stop bool) (Usage, error) {
	if i.closed {
		return Usage{}, fmt.Errorf("%w: task %s", ErrNoInterval, i.taskID)
	}
	current, cfg, err := i.store.load(i.taskID)
	if err != nil {
		return Usage{}, err
	}
	reading := i.store.now()
	at := reading.UTC()
	elapsed := reading.Sub(i.started)
	if elapsed < 0 {
		return Usage{}, fmt.Errorf("%w: task %s, the clock read %s before the interval started at %s",
			ErrClockRegression, i.taskID, stamp(reading), stamp(i.started))
	}
	if err := regression(i.taskID, at, current.CreatedAt, i.mark); err != nil {
		return Usage{}, err
	}
	var result sql.Result
	if stop {
		result, err = i.store.DB.Exec(`UPDATE budget_time SET active_ns = active_ns + ?, open_step = '',
			open_since = '', open_mark = '', open_ns = 0, updated_at = ?
			WHERE task_id = ? AND open_step = ? AND open_since = ?`,
			int64(elapsed), stamp(at), i.taskID, string(i.step), stamp(i.since))
	} else {
		result, err = i.store.DB.Exec(`UPDATE budget_time SET open_ns = ?, open_mark = ?, updated_at = ?
			WHERE task_id = ? AND open_step = ? AND open_since = ?`,
			int64(elapsed), stamp(at), stamp(at), i.taskID, string(i.step), stamp(i.since))
	}
	if err != nil {
		return Usage{}, fmt.Errorf("persist the active interval of task %s: %w", i.taskID, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return Usage{}, err
	} else if changed != 1 {
		i.closed = true
		return Usage{}, fmt.Errorf("%w: task %s, step %s since %s", ErrNoInterval, i.taskID, i.step, stamp(i.since))
	}
	i.mark = at
	row, _, err := i.store.readClock(i.taskID)
	if err != nil {
		return Usage{}, err
	}
	usage := Usage{Active: row.active, Wall: at.Sub(current.CreatedAt)}
	if stop {
		i.closed = true
	} else {
		usage.Active += elapsed
		usage.Open = i.step
	}
	return usage, exceeded(i.taskID, cfg.Budgets, usage)
}

// Recover closes the interval a crash left open, charging it
// conservatively: from its recorded start up to now, or the duration
// last persisted at a boundary if that is longer. A task with no open
// interval is left as is. A clock that reads before the recorded
// timestamps fails with ErrClockRegression and charges nothing. The
// task's time bounds are then checked as by Check.
func (s Store) Recover(taskID string) (Usage, error) {
	current, cfg, err := s.load(taskID)
	if err != nil {
		return Usage{}, err
	}
	at := s.now().UTC()
	row, _, err := s.readClock(taskID)
	if err != nil {
		return Usage{}, err
	}
	if err := regression(taskID, at, current.CreatedAt, row.updated, row.mark); err != nil {
		return Usage{}, err
	}
	if row.step != "" {
		charge := max(at.Sub(row.since), row.openNS)
		result, err := s.DB.Exec(`UPDATE budget_time SET active_ns = active_ns + ?, open_step = '',
			open_since = '', open_mark = '', open_ns = 0, updated_at = ?
			WHERE task_id = ? AND open_step = ? AND open_since = ?`,
			int64(charge), stamp(at), taskID, string(row.step), stamp(row.since))
		if err != nil {
			return Usage{}, fmt.Errorf("recover the active interval of task %s: %w", taskID, err)
		}
		if changed, err := result.RowsAffected(); err != nil {
			return Usage{}, err
		} else if changed != 1 {
			return Usage{}, fmt.Errorf("%w: task %s changed concurrently", ErrNoInterval, taskID)
		}
		row.active += charge
	}
	usage := Usage{Active: row.active, Wall: at.Sub(current.CreatedAt)}
	return usage, exceeded(taskID, cfg.Budgets, usage)
}

// Check returns the time a task used, counting the time last persisted
// for an open interval, and fails with an *ExceededError when a time
// bound is reached, or with ErrClockRegression when the clock reads
// before the recorded timestamps.
func (s Store) Check(taskID string) (Usage, error) {
	current, cfg, err := s.load(taskID)
	if err != nil {
		return Usage{}, err
	}
	at := s.now().UTC()
	row, _, err := s.readClock(taskID)
	if err != nil {
		return Usage{}, err
	}
	if err := regression(taskID, at, current.CreatedAt, row.updated, row.mark); err != nil {
		return Usage{}, err
	}
	usage := Usage{Active: row.active + row.openNS, Wall: at.Sub(current.CreatedAt), Open: row.step}
	return usage, exceeded(taskID, cfg.Budgets, usage)
}

// regression fails when the wall clock reads before a recorded
// timestamp; zero timestamps are ignored.
func regression(taskID string, at time.Time, recorded ...time.Time) error {
	for _, stamped := range recorded {
		if !stamped.IsZero() && at.Before(stamped) {
			return fmt.Errorf("%w: task %s, the clock reads %s, before the recorded %s",
				ErrClockRegression, taskID, stamp(at), stamp(stamped))
		}
	}
	return nil
}

// exceeded checks the time bounds of a usage: the active time against
// task_timeout, then the calendar time against wall_timeout when set.
func exceeded(taskID string, budgets config.Budgets, usage Usage) error {
	if usage.Active >= budgets.TaskTimeout.Duration {
		return &ExceededError{TaskID: taskID, Bound: BoundTaskTimeout,
			Limit: budgets.TaskTimeout.String(), Used: usage.Active.String()}
	}
	if budgets.WallTimeout != nil && usage.Wall >= budgets.WallTimeout.Duration {
		return &ExceededError{TaskID: taskID, Bound: BoundWallTimeout,
			Limit: budgets.WallTimeout.String(), Used: usage.Wall.String()}
	}
	return nil
}

// parse reads a stored instant.
func parse(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
