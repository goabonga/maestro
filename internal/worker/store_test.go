// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/turn"
	"github.com/goabonga/maestro/internal/worktree"
)

// clock is a manual clock for the store.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

// env is a migrated store with one task and two turns of the claude
// agent on it.
type env struct {
	store   Store
	clock   *clock
	project worktree.Project
	taskID  string
	turns   []string
}

func fixture(t *testing.T) env {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	configID, err := config.Persist(db, config.Snapshot{Config: config.Defaults(), Instructions: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{at: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	created, err := task.Store{DB: db, Now: c.now}.Create("project-1", "add a flag", configID, strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	e := env{
		store: Store{DB: db, Now: c.now}, clock: c, taskID: created.ID,
		project: worktree.Project{ID: "project-1", Dir: filepath.Join(t.TempDir(), "projects", "project-1")},
	}
	for range 2 {
		u, err := turn.Store{DB: db, Now: c.now}.Create(created.ID, "claude", configID)
		if err != nil {
			t.Fatal(err)
		}
		e.turns = append(e.turns, u.ID)
	}
	return e
}

func register(t *testing.T, e env, name string) Worker {
	t.Helper()
	w, err := e.store.Register(e.project, Spec{Name: name, Agent: "claude", AgentKind: "claude-code", Driver: "claude-code-2.1"})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func step(t *testing.T, e env, name string, inputs ...Input) Worker {
	t.Helper()
	var w Worker
	for _, input := range inputs {
		var err error
		if w, err = e.store.Transition(e.project.ID, name, input); err != nil {
			t.Fatalf("%s: %v", input.Event, err)
		}
	}
	return w
}

// assign is an Assign input on a turn of the fixture's task.
func (e env) assign(turnID string) Input {
	return Input{Event: Assign, Assignment: Assignment{TaskID: e.taskID, Role: Implementation, TurnID: turnID},
		Guard: Guard{AssignmentPersisted: true}}
}

func TestRegisterStoresAStoppedWorker(t *testing.T) {
	e := fixture(t)
	w := register(t, e, "claude-01")
	if w.State != Stopped || w.Version != 1 || w.Assignment != nil {
		t.Fatalf("registered: %+v", w)
	}
	if w.Repository != e.project.WorkerRepository("claude-01") {
		t.Fatalf("repository %q", w.Repository)
	}
	got, err := e.store.Get(e.project.ID, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	if got != w {
		t.Fatalf("stored %+v, registered %+v", got, w)
	}
	events, err := e.store.Events(e.project.ID, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event != Registered || events[0].From != "" || events[0].To != Stopped {
		t.Fatalf("events: %+v", events)
	}
}

func TestRegisterRefusesInvalidAndDuplicateWorkers(t *testing.T) {
	e := fixture(t)
	register(t, e, "claude-01")
	valid := Spec{Name: "codex-01", Agent: "codex", AgentKind: "codex", Driver: "codex-0.160"}
	for _, name := range []string{"", "Claude", "../x", "a_b", "-a", "a-", strings.Repeat("a", MaxName+1)} {
		spec := valid
		spec.Name = name
		if _, err := e.store.Register(e.project, spec); !errors.Is(err, ErrInvalid) {
			t.Fatalf("name %q: err=%v", name, err)
		}
	}
	for _, spec := range []Spec{
		{Name: "codex-01", AgentKind: "codex", Driver: "codex-0.160"},
		{Name: "codex-01", Agent: "codex", Driver: "codex-0.160"},
		{Name: "codex-01", Agent: "codex", AgentKind: "codex"},
	} {
		if _, err := e.store.Register(e.project, spec); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v: err=%v", spec, err)
		}
	}
	if _, err := e.store.Register(worktree.Project{}, valid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no project: err=%v", err)
	}
	if _, err := e.store.Register(e.project, Spec{Name: "claude-01", Agent: "codex", AgentKind: "codex", Driver: "codex-0.160"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: err=%v", err)
	}
	other := worktree.Project{ID: "project-2", Dir: t.TempDir()}
	if _, err := e.store.Register(other, Spec{Name: "claude-01", Agent: "claude", AgentKind: "claude-code", Driver: "claude-code-2.1"}); err != nil {
		t.Fatalf("the same name in another project: %v", err)
	}
}

func TestListReturnsTheProjectsWorkersByName(t *testing.T) {
	e := fixture(t)
	register(t, e, "codex-01")
	register(t, e, "claude-01")
	other := worktree.Project{ID: "project-2", Dir: t.TempDir()}
	if _, err := e.store.Register(other, Spec{Name: "claude-02", Agent: "claude", AgentKind: "claude-code", Driver: "claude-code-2.1"}); err != nil {
		t.Fatal(err)
	}
	workers, err := e.store.List(e.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 2 || workers[0].Name != "claude-01" || workers[1].Name != "codex-01" {
		t.Fatalf("workers: %+v", workers)
	}
	if _, err := e.store.Get(e.project.ID, "claude-02"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a worker of another project: err=%v", err)
	}
}

func TestTransitionsPersistTheAssignmentWithTheirEvents(t *testing.T) {
	e := fixture(t)
	register(t, e, "claude-01")
	e.clock.at = e.clock.at.Add(time.Minute)
	w := step(t, e, "claude-01",
		Input{Event: Start, Guard: Guard{CapacityReserved: true}},
		Input{Event: Ready, Guard: Guard{SessionReady: true, ProfileConfirmed: true}},
		e.assign(e.turns[0]))
	stored, err := e.store.Get(e.project.ID, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != Busy || stored.Version != 4 || stored.Assignment == nil || *stored.Assignment != *w.Assignment {
		t.Fatalf("stored %+v", stored)
	}
	if !stored.UpdatedAt.Equal(e.clock.at) {
		t.Fatalf("updated at %v", stored.UpdatedAt)
	}
	w = step(t, e, "claude-01", Input{Event: AcceptTurn, Reason: "accepted", Guard: Guard{RightsRevoked: true, Reconciled: true}})
	stored, err = e.store.Get(e.project.ID, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != Idle || stored.Assignment != nil || stored.Reason != "accepted" || stored != w {
		t.Fatalf("an accepted turn did not free the worker: %+v", stored)
	}
	events, err := e.store.Events(e.project.ID, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		event    Event
		from, to State
		turn     string
	}{
		{Registered, "", Stopped, ""},
		{Start, Stopped, Starting, ""},
		{Ready, Starting, Idle, ""},
		{Assign, Idle, Busy, e.turns[0]},
		{AcceptTurn, Busy, Idle, e.turns[0]},
	}
	if len(events) != len(want) {
		t.Fatalf("events: %+v", events)
	}
	for i, r := range events {
		if r.Event != want[i].event || r.From != want[i].from || r.To != want[i].to || r.TurnID != want[i].turn {
			t.Fatalf("event %d: %+v", i, r)
		}
		if want[i].turn != "" && r.TaskID != e.taskID {
			t.Fatalf("event %d without its task: %+v", i, r)
		}
	}
}

func TestRefusedTransitionsLeaveNoTrace(t *testing.T) {
	e := fixture(t)
	register(t, e, "claude-01")
	if _, err := e.store.Transition(e.project.ID, "claude-01", Input{Event: Ready, Guard: Guard{SessionReady: true, ProfileConfirmed: true}}); !errors.Is(err, ErrTransition) {
		t.Fatalf("ready from STOPPED: err=%v", err)
	}
	if _, err := e.store.Transition(e.project.ID, "claude-01", Input{Event: Start}); !errors.Is(err, ErrGuard) {
		t.Fatalf("start without capacity: err=%v", err)
	}
	if _, err := e.store.Transition(e.project.ID, "missing", Input{Event: Start}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown worker: err=%v", err)
	}
	w, err := e.store.Get(e.project.ID, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	if w.State != Stopped || w.Version != 1 {
		t.Fatalf("a refused event changed the worker: %+v", w)
	}
	if events, err := e.store.Events(e.project.ID, "claude-01"); err != nil || len(events) != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestAssignmentNeedsAStoredTurnOfItsTaskAndAgent(t *testing.T) {
	e := fixture(t)
	register(t, e, "claude-01")
	if _, err := e.store.Register(e.project, Spec{Name: "codex-01", Agent: "codex", AgentKind: "codex", Driver: "codex-0.160"}); err != nil {
		t.Fatal(err)
	}
	ready := []Input{
		{Event: Start, Guard: Guard{CapacityReserved: true}},
		{Event: Ready, Guard: Guard{SessionReady: true, ProfileConfirmed: true}},
	}
	step(t, e, "claude-01", ready...)
	step(t, e, "codex-01", ready...)

	unknown := e.assign("missing")
	otherTask := e.assign(e.turns[0])
	otherTask.Assignment.TaskID = "another-task"
	for _, input := range []Input{unknown, otherTask} {
		if _, err := e.store.Transition(e.project.ID, "claude-01", input); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v: err=%v", input.Assignment, err)
		}
	}
	if _, err := e.store.Transition(e.project.ID, "codex-01", e.assign(e.turns[0])); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a turn of another agent: err=%v", err)
	}
	w, err := e.store.Get(e.project.ID, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	if w.State != Idle || w.Assignment != nil {
		t.Fatalf("a refused assignment changed the worker: %+v", w)
	}
}

func TestATurnIsAssignedToOneWorkerAtMost(t *testing.T) {
	e := fixture(t)
	ready := []Input{
		{Event: Start, Guard: Guard{CapacityReserved: true}},
		{Event: Ready, Guard: Guard{SessionReady: true, ProfileConfirmed: true}},
	}
	for _, name := range []string{"claude-01", "claude-02"} {
		register(t, e, name)
		step(t, e, name, ready...)
	}
	step(t, e, "claude-01", e.assign(e.turns[0]))
	if _, err := e.store.Transition(e.project.ID, "claude-02", e.assign(e.turns[0])); !errors.Is(err, ErrAssigned) {
		t.Fatalf("a turn assigned twice: err=%v", err)
	}
	if _, err := e.store.Transition(e.project.ID, "claude-01", e.assign(e.turns[1])); !errors.Is(err, ErrTransition) {
		t.Fatalf("a second active assignment: err=%v", err)
	}
	step(t, e, "claude-02", e.assign(e.turns[1]))
	step(t, e, "claude-01", Input{Event: AcceptTurn, Guard: Guard{RightsRevoked: true, Reconciled: true}})
	if events, err := e.store.Events(e.project.ID, "claude-02"); err != nil || len(events) != 4 {
		t.Fatalf("the refused assignment left a trace: %+v err=%v", events, err)
	}
}

func TestConcurrentTransitionLosesWithoutOverwriting(t *testing.T) {
	e := fixture(t)
	register(t, e, "claude-01")
	stale, err := e.store.Get(e.project.ID, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	step(t, e, "claude-01", Input{Event: Start, Guard: Guard{CapacityReserved: true}})
	next, err := Apply(stale, Input{Event: Start, Guard: Guard{CapacityReserved: true}}, e.clock.at)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := write(tx, stale, next, Input{Event: Start}, e.clock.at); !errors.Is(err, ErrTransition) {
		t.Fatalf("a stale write: err=%v", err)
	}
}

func TestFailureKeepsTheAssignmentForTheConcernedTask(t *testing.T) {
	e := fixture(t)
	register(t, e, "claude-01")
	step(t, e, "claude-01",
		Input{Event: Start, Guard: Guard{CapacityReserved: true}},
		Input{Event: Ready, Guard: Guard{SessionReady: true, ProfileConfirmed: true}},
		e.assign(e.turns[0]),
		Input{Event: Fail, Reason: "session lost"})
	w, err := e.store.Get(e.project.ID, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	if w.State != Failed || w.Assignment == nil || w.Assignment.TaskID != e.taskID || w.Reason != "session lost" {
		t.Fatalf("failed: %+v", w)
	}
	w = step(t, e, "claude-01", Input{Event: Recover, Guard: Guard{CauseLifted: true, DescendantsStopped: true}})
	if w.State != Stopped || w.Assignment != nil {
		t.Fatalf("recovered: %+v", w)
	}
}
