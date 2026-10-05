// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package budget

import (
	"errors"
	"fmt"

	"github.com/goabonga/maestro/internal/task"
)

// ErrNotBudget reports a cause that is not a budget failure.
var ErrNotBudget = errors.New("not a budget failure")

// Block applies a budget failure to its task: the task goes BLOCKED
// through the workflow's block transition, with the failure, which
// names the bound or the clock regression, as its reason and its
// current state as its continuation. Nothing is killed: the caller
// stops the task's work as for any other block, and the consumed
// budgets stay recorded. A cause that wraps neither ErrExceeded nor
// ErrClockRegression is refused with ErrNotBudget.
func (s Store) Block(taskID string, cause error) (task.Task, error) {
	if !errors.Is(cause, ErrExceeded) && !errors.Is(cause, ErrClockRegression) {
		return task.Task{}, fmt.Errorf("%w: %v", ErrNotBudget, cause)
	}
	return s.tasks().Transition(taskID, task.Input{Event: task.Block, Reason: cause.Error()})
}
