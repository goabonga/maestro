// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package budget

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/task"
)

// timeouts is the default configuration with a task timeout and an
// optional wall timeout.
func timeouts(active time.Duration, wall time.Duration) config.Config {
	cfg := config.Defaults()
	cfg.Budgets.TaskTimeout = config.Duration{Duration: active}
	if wall > 0 {
		cfg.Budgets.WallTimeout = &config.Duration{Duration: wall}
	}
	return cfg
}

// plan moves a task from NEW to PLANNING.
func plan(t *testing.T, store Store, taskID string) {
	t.Helper()
	_, err := store.tasks().Transition(taskID, task.Input{Event: task.Assign, Guard: task.Guard{AssignmentAvailable: true}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStepOfCoversOnlyTheActiveSteps(t *testing.T) {
	want := map[task.State]Step{
		task.Planning: StepPlanning, task.Implementing: StepCoding, task.Fixing: StepCoding,
		task.Testing: StepTests, task.Reviewing: StepReview, task.Integrating: StepIntegration,
		task.MergeConflict: StepIntegration, task.Validating: StepIntegration,
	}
	for _, s := range task.States {
		step, ok := StepOf(s)
		if expected, active := want[s]; ok != active || step != expected {
			t.Fatalf("%s: step=%q active=%v", s, step, ok)
		}
	}
}

func TestActiveTimeCountsOnlyActiveIntervals(t *testing.T) {
	store, c, taskID := fixture(t, timeouts(time.Hour, 0))
	if _, err := store.Start(taskID); !errors.Is(err, ErrNotActive) {
		t.Fatalf("a NEW task started an interval: %v", err)
	}
	c.advance(10 * time.Minute) // queued: not active
	plan(t, store, taskID)
	interval, err := store.Start(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(taskID); !errors.Is(err, ErrIntervalOpen) {
		t.Fatalf("a second interval opened: %v", err)
	}
	c.advance(5 * time.Minute)
	usage, err := interval.Checkpoint()
	if err != nil || usage.Active != 5*time.Minute || usage.Open != StepPlanning {
		t.Fatalf("checkpoint %+v %v", usage, err)
	}
	c.advance(5 * time.Minute)
	usage, err = interval.Stop()
	if err != nil || usage.Active != 10*time.Minute || usage.Open != "" || usage.Wall != 20*time.Minute {
		t.Fatalf("stop %+v %v", usage, err)
	}
	if _, err := interval.Stop(); !errors.Is(err, ErrNoInterval) {
		t.Fatalf("a closed interval stopped again: %v", err)
	}
	c.advance(30 * time.Minute) // a human wait: not active
	again, err := store.Start(taskID)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(2 * time.Minute)
	if usage, err := again.Stop(); err != nil || usage.Active != 12*time.Minute {
		t.Fatalf("second interval %+v %v", usage, err)
	}
	if usage, err := store.Check(taskID); err != nil || usage.Active != 12*time.Minute || usage.Wall != 52*time.Minute {
		t.Fatalf("check %+v %v", usage, err)
	}
}

func TestTaskTimeoutIsReachedByActiveTime(t *testing.T) {
	store, c, taskID := fixture(t, timeouts(time.Hour, 0))
	plan(t, store, taskID)
	interval, err := store.Start(taskID)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour + time.Second)
	usage, err := interval.Stop()
	var exceeded *ExceededError
	if !errors.As(err, &exceeded) || exceeded.Bound != BoundTaskTimeout || exceeded.Limit != "1h0m0s" ||
		usage.Active != time.Hour+time.Second {
		t.Fatalf("stop %+v %v", usage, err)
	}
	// The time is charged all the same, and no new interval starts.
	if _, err := store.Start(taskID); !errors.As(err, &exceeded) || exceeded.Bound != BoundTaskTimeout {
		t.Fatalf("start after the timeout: %v", err)
	}
}

func TestWallTimeoutCountsWaits(t *testing.T) {
	store, c, taskID := fixture(t, timeouts(time.Hour, 3*time.Hour))
	plan(t, store, taskID)
	c.advance(3 * time.Hour) // waiting, not active
	_, err := store.Start(taskID)
	var exceeded *ExceededError
	if !errors.As(err, &exceeded) || exceeded.Bound != BoundWallTimeout ||
		!strings.Contains(err.Error(), "budgets.wall_timeout") {
		t.Fatalf("start after the wall timeout: %v", err)
	}
	if usage, err := store.Check(taskID); !errors.Is(err, ErrExceeded) || usage.Active != 0 {
		t.Fatalf("check %+v %v", usage, err)
	}
}

func TestRecoverChargesAnUnconfirmedIntervalConservatively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.db")
	db := open(t, path)
	configID, err := config.Persist(db, config.Snapshot{Config: timeouts(time.Hour, 0), Instructions: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{at: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	created, err := task.Store{DB: db, Now: c.now}.Create(configID, strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	store := Store{DB: db, Now: c.now}
	plan(t, store, created.ID)
	interval, err := store.Start(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(4 * time.Minute)
	if _, err := interval.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	// The daemon crashes 3 minutes after the checkpoint and restarts
	// 10 minutes later: the interval was never confirmed.
	c.advance(13 * time.Minute)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store = Store{DB: open(t, path), Now: c.now}
	if usage, err := store.Check(created.ID); err != nil || usage.Active != 4*time.Minute || usage.Open != StepPlanning {
		t.Fatalf("check before recovery %+v %v", usage, err)
	}
	if _, err := store.Start(created.ID); !errors.Is(err, ErrIntervalOpen) {
		t.Fatalf("an interval started over an unrecovered one: %v", err)
	}
	usage, err := store.Recover(created.ID)
	if err != nil || usage.Active != 17*time.Minute || usage.Open != "" {
		t.Fatalf("recover %+v %v", usage, err)
	}
	// A second recovery charges nothing.
	if usage, err := store.Recover(created.ID); err != nil || usage.Active != 17*time.Minute {
		t.Fatalf("second recovery %+v %v", usage, err)
	}
}

func TestARecoveredIntervalIsNoLongerPersisted(t *testing.T) {
	store, c, taskID := fixture(t, timeouts(time.Hour, 0))
	plan(t, store, taskID)
	interval, err := store.Start(taskID)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if _, err := store.Recover(taskID); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if _, err := interval.Stop(); !errors.Is(err, ErrNoInterval) {
		t.Fatalf("a recovered interval stopped: %v", err)
	}
	if usage, err := store.Check(taskID); err != nil || usage.Active != time.Minute {
		t.Fatalf("the recovered interval was charged twice: %+v %v", usage, err)
	}
}

func TestClockRegressionIsRefusedWithADiagnostic(t *testing.T) {
	store, c, taskID := fixture(t, timeouts(time.Hour, 0))
	plan(t, store, taskID)
	interval, err := store.Start(taskID)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(5 * time.Minute)
	if _, err := interval.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	c.advance(-2 * time.Minute)
	if _, err := interval.Checkpoint(); !errors.Is(err, ErrClockRegression) || !strings.Contains(err.Error(), "before the recorded") {
		t.Fatalf("checkpoint after a regression: %v", err)
	}
	c.advance(-10 * time.Minute)
	if _, err := interval.Stop(); !errors.Is(err, ErrClockRegression) {
		t.Fatalf("stop before the start: %v", err)
	}
	if _, err := store.Recover(taskID); !errors.Is(err, ErrClockRegression) {
		t.Fatalf("recover after a regression: %v", err)
	}
	if usage, err := store.Check(taskID); !errors.Is(err, ErrClockRegression) || usage.Active != 0 {
		t.Fatalf("check after a regression: %+v %v", usage, err)
	}
	// Nothing was charged while the clock read backwards.
	c.advance(20 * time.Minute)
	if usage, err := store.Check(taskID); err != nil || usage.Active != 5*time.Minute {
		t.Fatalf("check once the clock is back: %+v %v", usage, err)
	}
}

func TestMonotonicReadingsMeasureIntervals(t *testing.T) {
	// With the real clock, a wall reading carries Go's monotonic
	// reading: the interval is measured on it.
	fixed, _, _ := fixture(t, timeouts(time.Hour, 0))
	store := Store{DB: fixed.DB}
	configID, err := config.Persist(store.DB, config.Snapshot{Config: timeouts(time.Hour, 0), Instructions: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.tasks().Create(configID, strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	taskID := created.ID
	if _, err := store.tasks().Transition(taskID, task.Input{Event: task.Assign, Guard: task.Guard{AssignmentAvailable: true}}); err != nil {
		t.Fatal(err)
	}
	interval, err := store.Start(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if interval.Step() != StepPlanning {
		t.Fatalf("step %s", interval.Step())
	}
	if usage, err := interval.Stop(); err != nil || usage.Active < 0 || usage.Active > time.Minute {
		t.Fatalf("stop %+v %v", usage, err)
	}
}
