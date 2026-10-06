// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/task"
)

// found is a survivor search returning fixed processes per repository.
func found(byRepository map[string][]int) Survivors {
	return func(dir string) ([]int, error) {
		return byRepository[dir], nil
	}
}

// none is a survivor search that finds nothing.
var none = found(nil)

// toState drives a registered worker into a state of the table.
func toState(t *testing.T, e env, name string, s State) Worker {
	t.Helper()
	w := register(t, e, name)
	start := Input{Event: Start, Guard: Guard{CapacityReserved: true}}
	ready := Input{Event: Ready, Guard: Guard{SessionReady: true, ProfileConfirmed: true}}
	switch s {
	case Stopped:
		return w
	case Starting:
		return step(t, e, name, start)
	case Idle:
		return step(t, e, name, start, ready)
	case Busy:
		return step(t, e, name, start, ready, e.assign(e.turns[0]))
	case WaitingInput:
		return step(t, e, name, start, ready, e.assign(e.turns[0]),
			Input{Event: RequestInput, Guard: Guard{InputRecognized: true}})
	case Attached:
		return step(t, e, name, start, ready, Input{Event: Attach, Guard: Guard{TurnQuiescent: true}})
	case Paused:
		return step(t, e, name, start, ready,
			Input{Event: Pause, Guard: Guard{InterruptConfirmed: true, DescendantsStopped: true}})
	case Draining:
		return step(t, e, name, start, ready, Input{Event: ScaleDown})
	case Failed:
		return step(t, e, name, start, Input{Event: Fail, Reason: "crashed"})
	}
	t.Fatalf("no path to %s", s)
	return w
}

func reconcile(t *testing.T, e env, survivors Survivors) map[string]Reconciliation {
	t.Helper()
	outcomes, err := e.store.Reconcile(e.project.ID, survivors)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Reconciliation{}
	for _, outcome := range outcomes {
		byName[outcome.Name] = outcome
	}
	return byName
}

func TestReconcileStopsWorkersThatLostAnIdleRuntime(t *testing.T) {
	for _, s := range []State{Starting, Idle, Attached, Draining} {
		t.Run(string(s), func(t *testing.T) {
			e := fixture(t)
			toState(t, e, "w", s)
			outcome := reconcile(t, e, none)["w"]
			if outcome.From != s || outcome.To != Stopped || !outcome.Changed() || outcome.TaskBlocked {
				t.Fatalf("outcome %+v", outcome)
			}
			stored, err := e.store.Get(e.project.ID, "w")
			if err != nil {
				t.Fatal(err)
			}
			if stored.State != Stopped || stored.Assignment != nil || stored.Reason != reasonLost {
				t.Fatalf("stored %+v", stored)
			}
			events, err := e.store.Events(e.project.ID, "w")
			if err != nil {
				t.Fatal(err)
			}
			last := events[len(events)-1]
			if last.Event != Stop || last.From != s || last.To != Stopped || last.Reason != reasonLost {
				t.Fatalf("last event %+v", last)
			}
		})
	}
}

func TestReconcileFailsAWorkerHoldingATurnAndBlocksItsTask(t *testing.T) {
	for _, s := range []State{Busy, WaitingInput} {
		t.Run(string(s), func(t *testing.T) {
			e := fixture(t)
			toState(t, e, "w", s)
			outcome := reconcile(t, e, none)["w"]
			if outcome.To != Failed || !outcome.TaskBlocked {
				t.Fatalf("outcome %+v", outcome)
			}
			held := Assignment{TaskID: e.taskID, Role: Implementation, TurnID: e.turns[0]}
			if outcome.Assignment == nil || *outcome.Assignment != held {
				t.Fatalf("assignment %+v, want %+v kept", outcome.Assignment, held)
			}
			if !strings.Contains(outcome.Reason, "holding turn "+e.turns[0]) {
				t.Fatalf("reason %q", outcome.Reason)
			}
			stored, err := e.store.Get(e.project.ID, "w")
			if err != nil {
				t.Fatal(err)
			}
			if stored.State != Failed || stored.Assignment == nil || *stored.Assignment != held {
				t.Fatalf("stored %+v", stored)
			}
			blocked, err := task.Store{DB: e.store.DB}.Get(e.taskID)
			if err != nil {
				t.Fatal(err)
			}
			if blocked.State != task.Blocked || blocked.ResumeState != task.New ||
				!strings.Contains(blocked.BlockedReason, "worker w failed") {
				t.Fatalf("task %+v", blocked)
			}
		})
	}
}

func TestReconcileKeepsTheHeldTurnFromAnyOtherWorker(t *testing.T) {
	e := fixture(t)
	toState(t, e, "w", Busy)
	reconcile(t, e, none)
	other := toState(t, e, "other", Idle)
	_, err := e.store.Transition(e.project.ID, other.Name, e.assign(e.turns[0]))
	if !errors.Is(err, ErrAssigned) {
		t.Fatalf("the held turn was reassigned: %v", err)
	}
}

func TestReconcileLeavesAnAlreadyBlockedTaskAlone(t *testing.T) {
	e := fixture(t)
	tasks := task.Store{DB: e.store.DB, Now: e.clock.now}
	if _, err := tasks.Transition(e.taskID, task.Input{Event: task.Block, Reason: "waiting for a human"}); err != nil {
		t.Fatal(err)
	}
	toState(t, e, "w", Busy)
	outcome := reconcile(t, e, none)["w"]
	if outcome.To != Failed || outcome.TaskBlocked {
		t.Fatalf("outcome %+v", outcome)
	}
	blocked, err := tasks.Get(e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.BlockedReason != "waiting for a human" {
		t.Fatalf("the block was overwritten: %+v", blocked)
	}
}

func TestReconcileFailsActiveWorkersWithUnidentifiedSurvivors(t *testing.T) {
	for _, s := range []State{Starting, Idle, Attached, Paused, Draining} {
		t.Run(string(s), func(t *testing.T) {
			e := fixture(t)
			w := toState(t, e, "w", s)
			outcome := reconcile(t, e, found(map[string][]int{w.Repository: {41, 42}}))["w"]
			if outcome.To != Failed || !slices.Equal(outcome.Survivors, []int{41, 42}) {
				t.Fatalf("outcome %+v", outcome)
			}
			if !strings.Contains(outcome.Reason, "unidentified processes 41, 42 still work in "+w.Repository) {
				t.Fatalf("reason %q", outcome.Reason)
			}
		})
	}
}

func TestReconcileFailsAWorkerWithSurvivorsInItsWorktree(t *testing.T) {
	e := fixture(t)
	toState(t, e, "w", Idle)
	worktree := e.project.WorkerWorktree("w")
	search := func(dir string) ([]int, error) {
		if worktree == dir || strings.HasPrefix(worktree, dir+"/") {
			return []int{51}, nil
		}
		return nil, nil
	}
	outcome := reconcile(t, e, search)["w"]
	if outcome.To != Failed || !slices.Equal(outcome.Survivors, []int{51}) {
		t.Fatalf("outcome %+v", outcome)
	}
	if !strings.Contains(outcome.Reason, "unidentified processes 51 still work in "+filepath.Dir(worktree)) {
		t.Fatalf("reason %q", outcome.Reason)
	}
}

func TestReconcileFailsAWorkerWhoseSurvivorsCannotBeSearched(t *testing.T) {
	e := fixture(t)
	toState(t, e, "w", Idle)
	broken := func(string) ([]int, error) { return nil, errors.New("no process table") }
	outcome := reconcile(t, e, broken)["w"]
	if outcome.To != Failed || !strings.Contains(outcome.Reason, "no process table") {
		t.Fatalf("outcome %+v", outcome)
	}
}

func TestReconcileReportsSurvivorsOfInactiveWorkersWithoutMovingThem(t *testing.T) {
	for _, s := range []State{Stopped, Failed} {
		t.Run(string(s), func(t *testing.T) {
			e := fixture(t)
			w := toState(t, e, "w", s)
			outcome := reconcile(t, e, found(map[string][]int{w.Repository: {7}}))["w"]
			if outcome.Changed() || outcome.To != s || !slices.Equal(outcome.Survivors, []int{7}) || outcome.Reason == "" {
				t.Fatalf("outcome %+v", outcome)
			}
			stored, err := e.store.Get(e.project.ID, "w")
			if err != nil {
				t.Fatal(err)
			}
			if stored.Version != w.Version {
				t.Fatalf("stored %+v, before %+v", stored, w)
			}
		})
	}
}

func TestReconcileKeepsAPausedWorkerWithoutSurvivors(t *testing.T) {
	e := fixture(t)
	w := toState(t, e, "w", Paused)
	outcome := reconcile(t, e, none)["w"]
	if outcome.Changed() || outcome.Reason != "" {
		t.Fatalf("outcome %+v", outcome)
	}
	stored, err := e.store.Get(e.project.ID, "w")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != w.Version || stored.State != Paused {
		t.Fatalf("stored %+v", stored)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	e := fixture(t)
	toState(t, e, "busy", Busy)
	toState(t, e, "idle", Idle)
	toState(t, e, "paused", Paused)
	reconcile(t, e, none)
	again := reconcile(t, e, none)
	for name, outcome := range again {
		if outcome.Changed() || outcome.TaskBlocked {
			t.Fatalf("second run moved %s: %+v", name, outcome)
		}
	}
	if again["busy"].To != Failed || again["idle"].To != Stopped || again["paused"].To != Paused {
		t.Fatalf("second run: %+v", again)
	}
}

func TestReconcileNeedsASurvivorSearch(t *testing.T) {
	e := fixture(t)
	if _, err := e.store.Reconcile(e.project.ID, nil); err == nil {
		t.Fatal("a reconciliation without a survivor search is accepted")
	}
}

func TestReconcileFailsAnAttachedOrDrainingWorkerHoldingATurn(t *testing.T) {
	for _, last := range []Input{
		{Event: Attach, Guard: Guard{InterruptConfirmed: true}},
		{Event: ScaleDown},
	} {
		t.Run(string(last.Event), func(t *testing.T) {
			e := fixture(t)
			toState(t, e, "w", Busy)
			step(t, e, "w", last)
			outcome := reconcile(t, e, none)["w"]
			if outcome.To != Failed || outcome.Assignment == nil || !outcome.TaskBlocked {
				t.Fatalf("outcome %+v", outcome)
			}
		})
	}
}
