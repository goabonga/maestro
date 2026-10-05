// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package task

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/turn"
)

// settled is an update request outside any integration.
func settled(configID string) ConfigUpdate {
	return ConfigUpdate{ConfigID: configID, OperationSettled: true}
}

// snapshot persists a variant of the default snapshot.
func snapshot(t *testing.T, store Store, modify func(*config.Snapshot)) string {
	t.Helper()
	s := config.Snapshot{Config: config.Defaults(), Instructions: map[string]string{}}
	modify(&s)
	id, err := config.Persist(store.DB, s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The variants of the default snapshot, by impact.
func raisedCeiling(s *config.Snapshot) { s.Config.Budgets.MaxTurnsPerTask = 60 }
func newTests(s *config.Snapshot) {
	s.Config.Tests = map[string]config.TestCommand{"unit": {Argv: []string{"go", "test", "./..."}}}
}
func newInstructions(s *config.Snapshot) { s.Instructions = map[string]string{"coder.md": "be brief"} }

// toReady brings a task from NEW to READY_TO_INTEGRATE on revision rev.
func toReady(rev string) []Input {
	return append(toReview(rev), Input{Event: Approve, Revision: rev, Guard: Guard{AllReviewsApproved: true}})
}

func TestUpdateRestartsFromTheEarliestConcernedState(t *testing.T) {
	for name, test := range map[string]struct {
		modify func(*config.Snapshot)
		state  State
		kept   bool
	}{
		"objective":    {newInstructions, Planning, false},
		"verification": {newTests, Testing, false},
		"ceiling":      {raisedCeiling, ReadyToIntegrate, true},
	} {
		store, c, configID := fixture(t)
		created := create(t, store, configID)
		step(t, store, created.ID, toReady(sha('a'))...)
		c.at = c.at.Add(time.Minute)
		next := snapshot(t, store, test.modify)
		updated, err := store.UpdateConfig(created.ID, settled(next))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := updated.Task
		if got.State != test.state || got.ConfigID != next || updated.Previous != configID || len(updated.Changes) != 1 {
			t.Fatalf("%s: %+v", name, updated)
		}
		if kept := got.ApprovedSHA == sha('a'); kept != test.kept {
			t.Fatalf("%s: approved=%q", name, got.ApprovedSHA)
		}
		if got.HeadSHA != sha('a') || got.FixCycles != 0 || got.Version != 7 {
			t.Fatalf("%s: %+v", name, got)
		}
		stored, err := store.Get(created.ID)
		if err != nil || stored != got {
			t.Fatalf("%s: stored=%+v err=%v", name, stored, err)
		}
		events, err := store.Events(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		last := events[len(events)-1]
		if last.Event != UpdateConfig || last.From != ReadyToIntegrate || last.To != test.state ||
			!strings.Contains(last.Reason, configID) || !strings.Contains(last.Reason, next) || !last.At.Equal(c.at) {
			t.Fatalf("%s: event %+v", name, last)
		}
		journal, err := store.ConfigUpdates(created.ID)
		if err != nil || len(journal) != 1 || journal[0].EventID != last.ID || journal[0].Previous != configID ||
			journal[0].ConfigID != next || journal[0].Impact != updated.Changes.Impact() || len(journal[0].Changes) != 1 {
			t.Fatalf("%s: journal=%+v err=%v", name, journal, err)
		}
	}
}

func TestUpdateNeverMovesATaskForward(t *testing.T) {
	store, _, configID := fixture(t)
	planning := create(t, store, configID)
	step(t, store, planning.ID, Input{Event: Assign, Guard: Guard{AssignmentAvailable: true}})
	updated, err := store.UpdateConfig(planning.ID, settled(snapshot(t, store, newTests)))
	if err != nil || updated.Task.State != Planning {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	fresh := create(t, store, configID)
	updated, err = store.UpdateConfig(fresh.ID, settled(snapshot(t, store, newInstructions)))
	if err != nil || updated.Task.State != New {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
}

func TestUpdateOfABlockedTaskMovesItsContinuation(t *testing.T) {
	store, _, configID := fixture(t)
	created := create(t, store, configID)
	step(t, store, created.ID, append(toReview(sha('a')), Input{Event: Block, Reason: "budget exceeded"})...)
	updated, err := store.UpdateConfig(created.ID, settled(snapshot(t, store, newTests)))
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Task; got.State != Blocked || got.ResumeState != Testing || got.BlockedReason != "budget exceeded" {
		t.Fatalf("task %+v", got)
	}
	resumed := step(t, store, created.ID, Input{Event: Resume, Guard: Guard{CauseLifted: true, Reconciled: true}})
	if resumed.State != Testing {
		t.Fatalf("resumed in %s", resumed.State)
	}
}

func TestUpdateWithoutChangeStoresNothing(t *testing.T) {
	store, _, configID := fixture(t)
	created := create(t, store, configID)
	updated, err := store.UpdateConfig(created.ID, settled(configID))
	if err != nil || len(updated.Changes) != 0 || updated.Task != created {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	events, _ := store.Events(created.ID)
	journal, _ := store.ConfigUpdates(created.ID)
	if len(events) != 1 || len(journal) != 0 {
		t.Fatalf("events=%+v journal=%+v", events, journal)
	}
}

func TestUpdateIsRefusedOutsideItsGuards(t *testing.T) {
	store, _, configID := fixture(t)
	next := snapshot(t, store, newInstructions)

	cancelled := create(t, store, configID)
	step(t, store, cancelled.ID, Input{Event: Cancel, Guard: Guard{ProcessesStopped: true, OperationSettled: true}})
	for _, id := range []string{configID, next} {
		if _, err := store.UpdateConfig(cancelled.ID, settled(id)); !errors.Is(err, ErrGuard) {
			t.Fatalf("cancelled task on %s: %v", id, err)
		}
	}

	open := create(t, store, configID)
	if _, err := store.UpdateConfig(open.ID, ConfigUpdate{ConfigID: next, OperationSettled: true, Published: true}); !errors.Is(err, ErrGuard) {
		t.Fatalf("published: %v", err)
	}
	if _, err := store.UpdateConfig(open.ID, ConfigUpdate{ConfigID: next}); !errors.Is(err, ErrGuard) {
		t.Fatalf("pending operation: %v", err)
	}
	if _, err := store.UpdateConfig(open.ID, settled("sha256-missing")); err == nil {
		t.Fatal("an unknown snapshot was adopted")
	}
	if _, err := store.UpdateConfig("missing", settled(next)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}

	// A turn that has not ended keeps the task from adopting.
	turns := turn.Store{DB: store.DB}
	running, err := turns.Create(open.ID, "coder", configID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateConfig(open.ID, settled(next)); !errors.Is(err, ErrGuard) {
		t.Fatalf("running turn: %v", err)
	}
	if _, err := turns.Transition(running.ID, turn.Failed, "stopped"); err != nil {
		t.Fatal(err)
	}

	// So does an active time interval left open.
	if _, err := store.DB.Exec(`INSERT INTO budget_time (task_id, open_step, open_since, open_mark, updated_at)
		VALUES (?, 'planning', 'now', 'now', 'now')`, open.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateConfig(open.ID, settled(next)); !errors.Is(err, ErrGuard) {
		t.Fatalf("open interval: %v", err)
	}
	if _, err := store.DB.Exec(`UPDATE budget_time SET open_step = '', open_since = '', open_mark = '', active_ns = 7
		WHERE task_id = ?`, open.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(`INSERT INTO budget_turns (task_id, reservation_key, agent, reserved_at)
		VALUES (?, 'k1', 'coder', 'now')`, open.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateConfig(open.ID, settled(next)); err != nil {
		t.Fatal(err)
	}
	// The consumed budgets are kept.
	var turnsUsed int
	var active int64
	if err := store.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM budget_turns WHERE task_id = ?),
		(SELECT active_ns FROM budget_time WHERE task_id = ?)`, open.ID, open.ID).Scan(&turnsUsed, &active); err != nil {
		t.Fatal(err)
	}
	if turnsUsed != 1 || active != 7 {
		t.Fatalf("turns=%d active=%d", turnsUsed, active)
	}
}

func TestUpdateKeepsConsumedFixCyclesAndDropsTheCandidate(t *testing.T) {
	store, _, configID := fixture(t)
	validating := create(t, store, configID)
	step(t, store, validating.ID, append(toReady(sha('a')),
		Input{Event: Integrate, Guard: Guard{Trigger: Human}},
		Input{Event: BuildCandidate, Revision: sha('c')},
		Input{Event: ValidationFail, Revision: sha('c'), Guard: Guard{RolledBack: true}},
		Input{Event: Fix, Revision: sha('b'), Guard: Guard{ArtifactValid: true}},
		Input{Event: TestsPass, Revision: sha('b')},
		Input{Event: Approve, Revision: sha('b'), Guard: Guard{AllReviewsApproved: true}},
		Input{Event: Integrate, Guard: Guard{Trigger: Auto}},
		Input{Event: BuildCandidate, Revision: sha('d')},
	)...)
	updated, err := store.UpdateConfig(validating.ID, settled(snapshot(t, store, newTests)))
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Task; got.State != Testing || got.HeadSHA != sha('b') || got.FixCycles != 1 ||
		got.ResultSHA != "" || got.ApprovedSHA != "" {
		t.Fatalf("task %+v", got)
	}

	conflicting := create(t, store, configID)
	step(t, store, conflicting.ID, append(toReady(sha('a')),
		Input{Event: Integrate, Guard: Guard{Trigger: Human}},
		Input{Event: Conflict, Base: sha('e')},
		Input{Event: RejectResolution, Reason: "tests fail"},
		Input{Event: ApproveResolution, Revision: sha('f'), Guard: Guard{Trigger: Human, ResolutionVerified: true}},
	)...)
	updated, err = store.UpdateConfig(conflicting.ID, settled(snapshot(t, store, newInstructions)))
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Task; got.State != Planning || got.ResolutionApprovedSHA != "" ||
		got.ConflictBase != sha('e') || got.ConflictFailures != 1 {
		t.Fatalf("task %+v", got)
	}
}

func TestConcurrentUpdateLoses(t *testing.T) {
	store, c, configID := fixture(t)
	id := create(t, store, configID).ID
	next := snapshot(t, store, newInstructions)
	// The racing store transitions the task between the read and the
	// write of the losing update.
	racing := Store{DB: store.DB, Now: c.now}
	raced := false
	losing := Store{DB: store.DB, Now: func() time.Time {
		if !raced {
			raced = true
			if _, err := racing.Transition(id, Input{Event: Assign, Guard: Guard{AssignmentAvailable: true}}); err != nil {
				t.Fatal(err)
			}
		}
		return c.at
	}}
	if _, err := losing.UpdateConfig(id, settled(next)); !errors.Is(err, ErrTransition) {
		t.Fatalf("err=%v", err)
	}
	stored, err := store.Get(id)
	if err != nil || stored.State != Planning || stored.ConfigID != configID {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if journal, err := store.ConfigUpdates(id); err != nil || len(journal) != 0 {
		t.Fatalf("journal=%+v err=%v", journal, err)
	}
	if _, err := AdoptConfig(stored, configID, nil, settled(configID), c.at); !errors.Is(err, ErrGuard) {
		t.Fatalf("adopting the current snapshot again: %v", err)
	}
}
