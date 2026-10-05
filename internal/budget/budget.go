// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package budget enforces the durable budgets of a task: the turns it
// may consume, in total and per named agent, and the time it may spend,
// as cumulated active time and optionally as calendar time. Every bound
// comes from the task's configuration snapshot, and every counter lives
// in the state database, independent of workers, sessions and
// assignments: a resume or a change of worker never resets it.
package budget

import (
	"errors"
	"fmt"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/task"
)

// ErrExceeded reports a bound a task has reached; ExceededError names
// it.
var ErrExceeded = errors.New("budget exceeded")

// ExceededError names the bound a task reached, its limit and what the
// task used. It wraps ErrExceeded.
type ExceededError struct {
	TaskID string
	Bound  string
	Limit  string
	Used   string
}

// Error describes the exceeded bound.
func (e *ExceededError) Error() string {
	return fmt.Sprintf("%s: %s reached (limit %s, used %s)", ErrExceeded, e.Bound, e.Limit, e.Used)
}

// Unwrap returns ErrExceeded.
func (e *ExceededError) Unwrap() error {
	return ErrExceeded
}

// Store enforces the budgets of the tasks of a state database.
type Store struct {
	DB *state.DB
	// Now is the clock; nil means time.Now. Its readings measure
	// active intervals in memory, monotonically when they carry Go's
	// monotonic reading, and are stored as wall instants at the
	// boundaries.
	Now func() time.Time
}

// now reads the store's clock as is, monotonic reading included.
func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// tasks is the task store over the same database and clock.
func (s Store) tasks() task.Store {
	return task.Store{DB: s.DB, Now: s.Now}
}

// load returns a task and the configuration of its snapshot.
func (s Store) load(taskID string) (task.Task, config.Config, error) {
	current, err := s.tasks().Get(taskID)
	if err != nil {
		return task.Task{}, config.Config{}, err
	}
	snapshot, err := config.LoadSnapshot(s.DB, current.ConfigID)
	if err != nil {
		return task.Task{}, config.Config{}, err
	}
	return current, snapshot.Config, nil
}

// stamp renders an instant for storage.
func stamp(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}
