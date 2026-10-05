// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package task

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/worktree"
)

// clock is a manual clock for the store.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

// fixture opens a migrated store and persists one default snapshot.
func fixture(t *testing.T) (Store, *clock, string) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	id, err := config.Persist(db, config.Snapshot{Config: config.Defaults(), Instructions: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{at: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	return Store{DB: db, Now: c.now}, c, id
}

func create(t *testing.T, store Store, configID string) Task {
	t.Helper()
	created, err := store.Create("project-1", "add a flag", configID, sha('0'))
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func step(t *testing.T, store Store, id string, inputs ...Input) Task {
	t.Helper()
	var current Task
	for _, in := range inputs {
		var err error
		if current, err = store.Transition(id, in); err != nil {
			t.Fatalf("%s: %v", in.Event, err)
		}
	}
	return current
}

// toReview brings a task from NEW to REVIEWING on revision rev.
func toReview(rev string) []Input {
	return []Input{
		{Event: Assign, Guard: Guard{AssignmentAvailable: true}},
		{Event: AcceptPlan, Guard: Guard{PlanValid: true}},
		{Event: Implement, Revision: rev, Guard: Guard{ArtifactValid: true}},
		{Event: TestsPass, Revision: rev},
	}
}

func TestCreateFreezesConfigBaseAndBranch(t *testing.T) {
	store, _, configID := fixture(t)
	created := create(t, store, configID)
	if created.State != New || created.ProjectID != "project-1" || created.Description != "add a flag" ||
		created.ConfigID != configID || created.BaseSHA != sha('0') ||
		created.Branch != worktree.TaskBranch(created.ID) || created.MaxFixCycles != DefaultMaxFixCycles {
		t.Fatalf("created=%+v", created)
	}
	stored, err := store.Get(created.ID)
	if err != nil || stored != created {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	events, err := store.Events(created.ID)
	if err != nil || len(events) != 1 || events[0].Event != Created || events[0].From != "" || events[0].To != New {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	if _, err := store.Create("project-1", "add a flag", "sha256-missing", sha('0')); err == nil {
		t.Fatal("a task on an unknown snapshot was created")
	}
	if _, err := store.Create("project-1", "add a flag", configID, "main"); err == nil {
		t.Fatal("a task on a symbolic base was created")
	}
	if _, err := store.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}

func TestNominalPathReachesDone(t *testing.T) {
	store, c, configID := fixture(t)
	created := create(t, store, configID)
	c.at = c.at.Add(time.Minute)
	inputs := append(toReview(sha('a')),
		Input{Event: Approve, Revision: sha('a'), Guard: Guard{AllReviewsApproved: true}},
		Input{Event: Integrate, Guard: Guard{Trigger: Human}},
		Input{Event: BuildCandidate, Revision: sha('e')},
		Input{Event: ValidationPass, Revision: sha('e'), Guard: Guard{Published: true}, Reason: "published"},
	)
	done := step(t, store, created.ID, inputs...)
	if done.State != Done || done.HeadSHA != sha('a') || done.ApprovedSHA != sha('a') || done.ResultSHA != sha('e') ||
		done.Version != int64(1+len(inputs)) || done.FixCycles != 0 || !done.UpdatedAt.Equal(c.at) {
		t.Fatalf("done=%+v", done)
	}
	stored, err := store.Get(created.ID)
	if err != nil || stored != done {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	events, err := store.Events(created.ID)
	if err != nil || len(events) != 1+len(inputs) {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	path := []State{New, Planning, Implementing, Testing, Reviewing, ReadyToIntegrate, Integrating, Validating, Done}
	for i, event := range events[1:] {
		if event.Event != inputs[i].Event || event.From != path[i] || event.To != path[i+1] ||
			event.Revision != inputs[i].Revision || event.Stale {
			t.Fatalf("event %d=%+v", i, event)
		}
	}
	if last := events[len(events)-1]; last.Reason != "published" || !last.At.Equal(c.at) {
		t.Fatalf("last=%+v", last)
	}
}

func TestCorrectionsInvalidateApprovalsAndBlockWhenExhausted(t *testing.T) {
	store, _, configID := fixture(t)
	id := create(t, store, configID).ID
	step(t, store, id, toReview(sha('a'))...)
	fixing := step(t, store, id, Input{Event: RequestChanges, Revision: sha('a')})
	if fixing.State != Fixing || fixing.FixCycles != 1 {
		t.Fatalf("fixing=%+v", fixing)
	}
	retest := step(t, store, id, Input{Event: Fix, Revision: sha('b'), Guard: Guard{ArtifactValid: true}})
	if retest.State != Testing || retest.HeadSHA != sha('b') || retest.ApprovedSHA != "" {
		t.Fatalf("retest=%+v", retest)
	}
	// A late result about the previous revision is logged, not applied.
	if got, err := store.Transition(id, Input{Event: TestsPass, Revision: sha('a')}); !errors.Is(err, ErrStale) || got != retest {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	events, err := store.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	if stale := events[len(events)-1]; !stale.Stale || stale.From != Testing || stale.To != Testing || stale.Revision != sha('a') {
		t.Fatalf("stale=%+v", stale)
	}
	step(t, store, id,
		Input{Event: TestsFail, Revision: sha('b')},
		Input{Event: Fix, Revision: sha('c'), Guard: Guard{ArtifactValid: true}},
		Input{Event: TestsFail, Revision: sha('c')},
		Input{Event: Fix, Revision: sha('d'), Guard: Guard{ArtifactValid: true}},
	)
	blocked := step(t, store, id, Input{Event: TestsFail, Revision: sha('d'), Reason: "3 failures"})
	if blocked.State != Blocked || blocked.ResumeState != Testing || blocked.FixCycles != DefaultMaxFixCycles ||
		blocked.BlockedReason != ReasonFixCyclesExhausted+": 3 failures" {
		t.Fatalf("blocked=%+v", blocked)
	}
	resumed := step(t, store, id, Input{Event: Resume, Guard: Guard{CauseLifted: true, Reconciled: true}})
	if resumed.State != Testing || resumed.ResumeState != "" || resumed.BlockedReason != "" || resumed.FixCycles != DefaultMaxFixCycles {
		t.Fatalf("resumed=%+v", resumed)
	}
}

func TestConflictResolutionsAreBoundedPerBase(t *testing.T) {
	store, _, configID := fixture(t)
	id := create(t, store, configID).ID
	step(t, store, id, toReview(sha('a'))...)
	step(t, store, id,
		Input{Event: Approve, Revision: sha('a'), Guard: Guard{AllReviewsApproved: true}},
		Input{Event: Integrate, Guard: Guard{Trigger: Auto}},
	)
	conflict := step(t, store, id, Input{Event: Conflict, Base: sha('1')}, Input{Event: RejectResolution, Reason: "tests fail"})
	if conflict.State != MergeConflict || conflict.ConflictBase != sha('1') || conflict.ConflictFailures != 1 {
		t.Fatalf("conflict=%+v", conflict)
	}
	verified := Guard{ProposalValid: true, ResolutionVerified: true, HumanGateRequired: true}
	if _, err := store.Transition(id, Input{Event: ResolveConflict, Revision: sha('c'), Guard: verified}); !errors.Is(err, ErrGuard) {
		t.Fatalf("an unapproved resolution resumed integration: %v", err)
	}
	approved := step(t, store, id, Input{Event: ApproveResolution, Revision: sha('c'), Guard: Guard{Trigger: Human, ResolutionVerified: true}})
	if approved.State != MergeConflict || approved.ResolutionApprovedSHA != sha('c') {
		t.Fatalf("approved=%+v", approved)
	}
	if _, err := store.Transition(id, Input{Event: ResolveConflict, Revision: sha('f'), Guard: verified}); !errors.Is(err, ErrGuard) {
		t.Fatalf("another resolution used the approval: %v", err)
	}
	integrating := step(t, store, id, Input{Event: ResolveConflict, Revision: sha('c'), Guard: verified})
	if integrating.State != Integrating || integrating.ResolutionApprovedSHA != "" || integrating.ConflictFailures != 1 {
		t.Fatalf("integrating=%+v", integrating)
	}
	blocked := step(t, store, id, Input{Event: Conflict, Base: sha('1')}, Input{Event: RejectResolution})
	if blocked.State != Blocked || blocked.ResumeState != MergeConflict || blocked.BlockedReason != ReasonConflictUnresolved {
		t.Fatalf("blocked=%+v", blocked)
	}
	resumed := step(t, store, id, Input{Event: Resume, Guard: Guard{CauseLifted: true, Reconciled: true}})
	if _, err := store.Transition(id, Input{Event: ResolveConflict, Revision: sha('c'), Guard: Guard{ProposalValid: true, ResolutionVerified: true}}); resumed.State != MergeConflict || !errors.Is(err, ErrGuard) {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
}

func TestConflictOnANewBaseResetsTheCount(t *testing.T) {
	task := Task{State: Integrating, ConflictBase: sha('1'), ConflictFailures: 1}
	next, err := Apply(task, Input{Event: Conflict, Base: sha('2')}, time.Now())
	if err != nil || next.ConflictBase != sha('2') || next.ConflictFailures != 0 {
		t.Fatalf("next=%+v err=%v", next, err)
	}
}

func TestValidationFailureNeedsRollbackAndEntersFixing(t *testing.T) {
	store, _, configID := fixture(t)
	id := create(t, store, configID).ID
	step(t, store, id, toReview(sha('a'))...)
	step(t, store, id,
		Input{Event: Approve, Revision: sha('a'), Guard: Guard{AllReviewsApproved: true}},
		Input{Event: Integrate, Guard: Guard{Trigger: Human}},
		Input{Event: BuildCandidate, Revision: sha('e')},
	)
	if _, err := store.Transition(id, Input{Event: ValidationFail, Revision: sha('e')}); !errors.Is(err, ErrGuard) {
		t.Fatalf("err=%v", err)
	}
	fixing := step(t, store, id, Input{Event: ValidationFail, Revision: sha('e'), Guard: Guard{RolledBack: true}})
	if fixing.State != Fixing || fixing.FixCycles != 1 || fixing.ResultSHA != "" || fixing.ApprovedSHA != "" {
		t.Fatalf("fixing=%+v", fixing)
	}
}

func TestRefusedEventsLeaveNoTrace(t *testing.T) {
	store, _, configID := fixture(t)
	created := create(t, store, configID)
	for _, in := range []Input{{Event: TestsPass, Revision: sha('a')}, {Event: Assign}, {Event: Resume}} {
		if _, err := store.Transition(created.ID, in); err == nil || errors.Is(err, ErrStale) {
			t.Fatalf("%s: err=%v", in.Event, err)
		}
	}
	stored, err := store.Get(created.ID)
	if err != nil || stored != created {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if events, err := store.Events(created.ID); err != nil || len(events) != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	if _, err := store.Transition("missing", Input{Event: Assign}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}

func TestCancelEndsTheTaskForGood(t *testing.T) {
	store, _, configID := fixture(t)
	id := create(t, store, configID).ID
	step(t, store, id, Input{Event: Assign, Guard: Guard{AssignmentAvailable: true}}, Input{Event: Block, Reason: "input wait"})
	cancelled := step(t, store, id, Input{Event: Cancel, Guard: Guard{ProcessesStopped: true, OperationSettled: true}})
	if cancelled.State != Cancelled || cancelled.ResumeState != "" || cancelled.BlockedReason != "" {
		t.Fatalf("cancelled=%+v", cancelled)
	}
	if _, err := store.Transition(id, Input{Event: Resume, Guard: Guard{CauseLifted: true, Reconciled: true}}); !errors.Is(err, ErrTransition) {
		t.Fatalf("err=%v", err)
	}
}

func TestConcurrentTransitionLoses(t *testing.T) {
	store, c, configID := fixture(t)
	id := create(t, store, configID).ID
	// The racing store transitions the task between the read and the
	// write of the losing one.
	racing := Store{DB: store.DB, Now: c.now}
	raced := false
	losing := Store{DB: store.DB, Now: func() time.Time {
		if !raced {
			raced = true
			if _, err := racing.Transition(id, Input{Event: Block, Reason: "timeout"}); err != nil {
				t.Fatal(err)
			}
		}
		return c.at
	}}
	if _, err := losing.Transition(id, Input{Event: Assign, Guard: Guard{AssignmentAvailable: true}}); !errors.Is(err, ErrTransition) {
		t.Fatalf("err=%v", err)
	}
	stored, err := store.Get(id)
	if err != nil || stored.State != Blocked || stored.ResumeState != New {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if events, err := store.Events(id); err != nil || len(events) != 2 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestCreateRefusesInvalidRequests(t *testing.T) {
	store, _, configID := fixture(t)
	for name, request := range map[string][2]string{
		"no project":     {"", "add a flag"},
		"blank":          {"project-1", " \n\t"},
		"oversized":      {"project-1", strings.Repeat("x", MaxDescription+1)},
		"not utf-8 text": {"project-1", "\xff\xfe"},
	} {
		if _, err := store.Create(request[0], request[1], configID, sha('0')); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	created, err := store.Create("project-1", "  trimmed\n", configID, sha('0'))
	if err != nil || created.Description != "trimmed" {
		t.Fatalf("created=%+v err=%v", created, err)
	}
}

func TestListReturnsTheTasksOfOneProjectOldestFirst(t *testing.T) {
	store, c, configID := fixture(t)
	first, err := store.Create("project-1", "first", configID, sha('0'))
	if err != nil {
		t.Fatal(err)
	}
	c.at = c.at.Add(time.Minute)
	if _, err := store.Create("project-2", "elsewhere", configID, sha('0')); err != nil {
		t.Fatal(err)
	}
	c.at = c.at.Add(time.Minute)
	second, err := store.Create("project-1", "second", configID, sha('0'))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := store.List("project-1")
	if err != nil || len(tasks) != 2 || tasks[0] != first || tasks[1] != second {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	if tasks, err := store.List("project-3"); err != nil || len(tasks) != 0 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
}
