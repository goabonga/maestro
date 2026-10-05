// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package budget

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/task"
)

func TestBlockKeepsTheContinuationAndNamesTheBound(t *testing.T) {
	store, _, taskID := fixture(t, caps(1, nil))
	plan(t, store, taskID)
	if _, err := store.Reserve(taskID, "planner", "plan-1"); err != nil {
		t.Fatal(err)
	}
	_, cause := store.Reserve(taskID, "planner", "plan-2")
	blocked, err := store.Block(taskID, cause)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.State != task.Blocked || blocked.ResumeState != task.Planning ||
		!strings.Contains(blocked.BlockedReason, BoundTaskTurns) {
		t.Fatalf("blocked %+v", blocked)
	}
	// The consumed turns stay recorded, and a blocked task cannot be
	// blocked again.
	if turns, _ := store.Turns(taskID); turns.Task != 1 {
		t.Fatalf("turns %+v", turns)
	}
	if _, err := store.Block(taskID, cause); !errors.Is(err, task.ErrTransition) {
		t.Fatalf("second block: %v", err)
	}
}

func TestBlockAppliesAClockRegression(t *testing.T) {
	store, c, taskID := fixture(t, timeouts(time.Hour, 0))
	plan(t, store, taskID)
	c.advance(-time.Minute)
	_, cause := store.Check(taskID)
	c.advance(time.Minute)
	blocked, err := store.Block(taskID, cause)
	if err != nil || blocked.State != task.Blocked || !strings.Contains(blocked.BlockedReason, "clock regression") {
		t.Fatalf("blocked %+v %v", blocked, err)
	}
}

func TestBlockRefusesOtherCauses(t *testing.T) {
	store, _, taskID := fixture(t, caps(1, nil))
	for _, cause := range []error{nil, errors.New("disk full")} {
		if _, err := store.Block(taskID, cause); !errors.Is(err, ErrNotBudget) {
			t.Fatalf("%v: %v", cause, err)
		}
	}
	if current, err := store.tasks().Get(taskID); err != nil || current.State != task.New {
		t.Fatalf("task %+v %v", current, err)
	}
}
