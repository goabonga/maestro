// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package turn

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/state"
)

// clock is a manual clock for the store.
type clock struct{ at time.Time }

func (c *clock) now() time.Time                   { return c.at }
func (c *clock) advance(by time.Duration)         { c.at = c.at.Add(by) }
func newClock() *clock                            { return &clock{at: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)} }
func minutes(n int) time.Duration                 { return time.Duration(n) * time.Minute }
func durationOf(d time.Duration) *config.Duration { return &config.Duration{Duration: d} }

// fixture opens a migrated store and persists one snapshot of cfg.
func fixture(t *testing.T, cfg config.Config) (Store, *clock, string) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	id, err := config.Persist(db, config.Snapshot{Config: cfg, Instructions: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	c := newClock()
	return Store{DB: db, Now: c.now}, c, id
}

func walk(t *testing.T, store Store, id string, states ...State) Turn {
	t.Helper()
	var current Turn
	for _, next := range states {
		var err error
		if current, err = store.Transition(id, next, "to "+string(next)); err != nil {
			t.Fatalf("%s: %v", next, err)
		}
	}
	return current
}

func TestTransitionTable(t *testing.T) {
	all := []State{Prepared, Running, WaitingInput, Validating, Succeeded, Interrupted, Failed}
	allowed := map[[2]State]bool{
		{Prepared, Running}: true, {Prepared, Interrupted}: true, {Prepared, Failed}: true,
		{Running, WaitingInput}: true, {Running, Validating}: true, {Running, Interrupted}: true, {Running, Failed}: true,
		{WaitingInput, Running}: true, {WaitingInput, Interrupted}: true, {WaitingInput, Failed}: true,
		{Validating, Succeeded}: true, {Validating, Interrupted}: true, {Validating, Failed}: true,
	}
	for _, from := range all {
		for _, to := range all {
			if Allowed(from, to) != allowed[[2]State{from, to}] {
				t.Fatalf("%s -> %s: allowed=%v", from, to, Allowed(from, to))
			}
		}
		if from.Terminal() != (from == Succeeded || from == Interrupted || from == Failed) {
			t.Fatalf("%s terminal=%v", from, from.Terminal())
		}
	}
}

func TestTurnSucceedsThroughAnInputWait(t *testing.T) {
	store, c, configID := fixture(t, config.Defaults())
	created, err := store.Create("task-1", "claude", configID)
	if err != nil {
		t.Fatal(err)
	}
	if created.State != Prepared || created.ConfigID != configID || created.ID == "" || created.AttemptID == "" {
		t.Fatalf("created %+v", created)
	}
	if created.Timeouts != (Timeouts{Turn: minutes(20), InputWait: minutes(5)}) {
		t.Fatalf("timeouts %+v", created.Timeouts)
	}
	walk(t, store, created.ID, Running)
	c.advance(time.Minute)
	waiting := walk(t, store, created.ID, WaitingInput)
	if waiting.WaitingSince != c.at {
		t.Fatalf("waiting since %v", waiting.WaitingSince)
	}
	c.advance(time.Minute)
	resumed := walk(t, store, created.ID, Running)
	if !resumed.WaitingSince.IsZero() || resumed.AdmittedAt != c.at.Add(-2*time.Minute) {
		t.Fatalf("resumed %+v", resumed)
	}
	walk(t, store, created.ID, Validating, Succeeded)

	stored, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != Succeeded || stored.Reason != "to SUCCEEDED" || stored.AdmittedAt != resumed.AdmittedAt || stored.Timeouts != created.Timeouts {
		t.Fatalf("stored %+v", stored)
	}
	events, err := store.Events(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]State{{"", Prepared}, {Prepared, Running}, {Running, WaitingInput}, {WaitingInput, Running}, {Running, Validating}, {Validating, Succeeded}}
	if len(events) != len(want) {
		t.Fatalf("events %+v", events)
	}
	for i, event := range events {
		if event.From != want[i][0] || event.To != want[i][1] {
			t.Fatalf("event %d: %+v", i, event)
		}
	}
	if _, err := store.Transition(created.ID, Failed, "late"); !errors.Is(err, ErrTransition) {
		t.Fatalf("a terminal turn moved: %v", err)
	}
}

func TestUnlistedTransitionsAreRefusedAndNotRecorded(t *testing.T) {
	store, _, configID := fixture(t, config.Defaults())
	created, err := store.Create("task-1", "claude", configID)
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []State{Validating, WaitingInput, Succeeded, Prepared} {
		if _, err := store.Transition(created.ID, to, "skip"); !errors.Is(err, ErrTransition) {
			t.Fatalf("PREPARED -> %s: %v", to, err)
		}
	}
	if events, err := store.Events(created.ID); err != nil || len(events) != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	if _, err := store.Transition("missing", Running, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown turn: %v", err)
	}
}

func TestInterruptionAndFailureLeaveEveryLiveState(t *testing.T) {
	store, _, configID := fixture(t, config.Defaults())
	paths := map[State][]State{
		Prepared:     nil,
		Running:      {Running},
		WaitingInput: {Running, WaitingInput},
		Validating:   {Running, Validating},
	}
	for from, path := range paths {
		for _, end := range []State{Interrupted, Failed} {
			created, err := store.Create("task-1", "claude", configID)
			if err != nil {
				t.Fatal(err)
			}
			walk(t, store, created.ID, path...)
			if ended := walk(t, store, created.ID, end); ended.State != end || !ended.WaitingSince.IsZero() {
				t.Fatalf("%s -> %s: %+v", from, end, ended)
			}
		}
	}
}

func TestRetryCreatesALinkedTurnFromATerminalOne(t *testing.T) {
	store, _, configID := fixture(t, config.Defaults())
	first, err := store.Create("task-1", "codex", configID)
	if err != nil {
		t.Fatal(err)
	}
	walk(t, store, first.ID, Running)
	if _, err := store.Retry(first.ID); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("a running turn was retried: %v", err)
	}
	walk(t, store, first.ID, Failed)
	second, err := store.Retry(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || second.AttemptID == first.AttemptID || second.PreviousID != first.ID ||
		second.State != Prepared || second.TaskID != "task-1" || second.Agent != "codex" || second.ConfigID != configID {
		t.Fatalf("retry %+v of %+v", second, first)
	}
	if stored, err := store.Get(second.ID); err != nil || stored.PreviousID != first.ID {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if _, err := store.Retry(first.ID); !errors.Is(err, ErrRetried) {
		t.Fatalf("a turn was retried twice: %v", err)
	}
	if previous, err := store.Get(first.ID); err != nil || previous.State != Failed {
		t.Fatalf("the retried turn changed: %+v %v", previous, err)
	}
	if _, err := store.Retry("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown turn: %v", err)
	}
}

func TestCreateFreezesTheAgentTimeouts(t *testing.T) {
	cfg := config.Defaults()
	cfg.Budgets.InputWaitTimeout = config.Duration{Duration: minutes(2)}
	cfg.Agents = map[string]config.Agent{"claude": {TurnTimeout: durationOf(minutes(45))}, "codex": {}}
	store, _, configID := fixture(t, cfg)
	claude, err := store.Create("task-1", "claude", configID)
	if err != nil {
		t.Fatal(err)
	}
	codex, err := store.Create("task-1", "codex", configID)
	if err != nil {
		t.Fatal(err)
	}
	if claude.Timeouts != (Timeouts{Turn: minutes(45), InputWait: minutes(2)}) || codex.Timeouts != (Timeouts{Turn: minutes(20), InputWait: minutes(2)}) {
		t.Fatalf("claude %+v codex %+v", claude.Timeouts, codex.Timeouts)
	}
	// The stored bounds are what the turn keeps, whatever the
	// configuration says afterwards.
	if _, err := store.DB.Exec("UPDATE turns SET turn_timeout_ns = ? WHERE turn_id = ?", int64(minutes(7)), claude.ID); err != nil {
		t.Fatal(err)
	}
	if stored, err := store.Get(claude.ID); err != nil || stored.Timeouts.Turn != minutes(7) {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if _, err := store.Create("task-1", "claude", "sha256-unknown"); err == nil {
		t.Fatal("a turn without a stored snapshot was created")
	}
	if _, err := store.Create("", "claude", configID); err == nil {
		t.Fatal("a turn without a task was created")
	}
}

func TestTurnTimeoutCountsFromAdmissionIncludingWaits(t *testing.T) {
	store, c, configID := fixture(t, config.Defaults())
	created, err := store.Create("task-1", "claude", configID)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	if _, expired, err := store.Expire(created.ID); err != nil || expired {
		t.Fatalf("a turn not yet admitted expired: %v %v", expired, err)
	}
	walk(t, store, created.ID, Running)
	// Four waits of four minutes each stay within their own bound but
	// count toward the turn's.
	for range 4 {
		c.advance(time.Minute)
		walk(t, store, created.ID, WaitingInput)
		c.advance(minutes(4))
		walk(t, store, created.ID, Running)
	}
	current, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deadline, ok := current.Deadline(); !ok || deadline != current.AdmittedAt.Add(minutes(20)) {
		t.Fatalf("deadline %v %v", deadline, ok)
	}
	if _, expired := current.Expired(c.at.Add(-time.Nanosecond)); expired {
		t.Fatal("expired before the deadline")
	}
	failed, expired, err := store.Expire(created.ID)
	if err != nil || !expired || failed.State != Failed || failed.Reason != string(TurnTimeout) {
		t.Fatalf("failed=%+v expired=%v err=%v", failed, expired, err)
	}
	if _, expired := failed.Expired(c.at.Add(time.Hour)); expired {
		t.Fatal("a terminal turn expired")
	}
}

func TestInputWaitTimeout(t *testing.T) {
	store, c, configID := fixture(t, config.Defaults())
	created, err := store.Create("task-1", "claude", configID)
	if err != nil {
		t.Fatal(err)
	}
	walk(t, store, created.ID, Running, WaitingInput)
	c.advance(minutes(5) - time.Nanosecond)
	if _, expired, err := store.Expire(created.ID); err != nil || expired {
		t.Fatalf("expired early: %v %v", expired, err)
	}
	c.advance(time.Nanosecond)
	failed, expired, err := store.Expire(created.ID)
	if err != nil || !expired || failed.Reason != string(InputWaitTimeout) {
		t.Fatalf("failed=%+v expired=%v err=%v", failed, expired, err)
	}
}

func TestTurnTimeoutBoundsAnInputWait(t *testing.T) {
	store, c, configID := fixture(t, config.Defaults())
	created, err := store.Create("task-1", "claude", configID)
	if err != nil {
		t.Fatal(err)
	}
	walk(t, store, created.ID, Running)
	c.advance(minutes(18))
	waiting := walk(t, store, created.ID, WaitingInput)
	if deadline, _ := waiting.Deadline(); deadline != waiting.AdmittedAt.Add(minutes(20)) {
		t.Fatalf("deadline %v", deadline)
	}
	if expiry, expired := waiting.Expired(c.at.Add(minutes(10))); !expired || expiry != TurnTimeout {
		t.Fatalf("expiry %q %v", expiry, expired)
	}
}
