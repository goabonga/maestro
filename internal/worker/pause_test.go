// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/agent"
	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/handoff"
	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/turn"
)

// startIdle starts one worker of the fixture and returns its live
// session once it is IDLE.
func startIdle(t *testing.T, h harness, snapshot config.Snapshot) *running {
	t.Helper()
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	h.settle(t, "claude-code-01", Idle)
	h.supervisor.mu.Lock()
	defer h.supervisor.mu.Unlock()
	return h.supervisor.live[liveKey{h.project.ID, "claude-code-01"}]
}

// waitSlots waits for the session slots in use to reach want.
func waitSlots(t *testing.T, h harness, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h.sessionsUsed() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%d session slots used, want %d", h.sessionsUsed(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSupervisorPausesAndResumesAnIdleWorker(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": turnFixture})
	live := startIdle(t, h, config.Snapshot{Config: config.Defaults()})
	const name = "claude-code-01"
	nativeID := live.epoch.State().NativeID

	paused, err := h.supervisor.Pause(h.project.ID, name)
	if err != nil || paused.State != Paused || paused.Assignment != nil || paused.Reason != reasonPaused {
		t.Fatalf("pause: %+v %v", paused, err)
	}
	// The group is gone, the engine finds no session, and the slot and
	// the native session are kept for the resume.
	if state := live.epoch.Session().State(); state.Phase == session.Running {
		t.Fatal("the paused group still runs")
	}
	if _, ok := h.supervisor.Session(h.project.ID, name); ok {
		t.Fatal("a paused worker has a live session")
	}
	if used := h.sessionsUsed(); used != 1 {
		t.Fatalf("%d session slots used while paused", used)
	}
	if _, err := h.supervisor.Pause(h.project.ID, name); !errors.Is(err, ErrTransition) {
		t.Fatalf("second pause: %v", err)
	}

	resumed, err := h.supervisor.Resume(h.project.ID, name)
	if err != nil || resumed.State != Starting {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	h.settle(t, name, Idle)
	if _, err := h.supervisor.Resume(h.project.ID, name); !errors.Is(err, ErrTransition) {
		t.Fatalf("resume of an IDLE worker: %v", err)
	}
	state := live.epoch.State()
	live.turn.Lock()
	role := live.role
	live.turn.Unlock()
	if state.NativeID != nativeID || state.Phase != session.EpochActive || role != session.Review {
		t.Fatalf("resumed epoch %+v with role %s, want native session %s", state, role, nativeID)
	}
	want := []Event{Registered, Start, Ready, Pause, Resume, Ready}
	if events := h.events(t, name); !slices.Equal(events, want) {
		t.Fatalf("events %v, want %v", events, want)
	}

	// The resumed conversation takes a turn.
	s, ok := h.supervisor.Session(h.project.ID, name)
	if !ok {
		t.Fatal("a resumed worker has no live session")
	}
	if err := s.Send("write a probe"); err != nil {
		t.Fatal(err)
	}
	if detection := pollUntil(t, s); detection != agent.DetectCompleted {
		t.Fatalf("turn after the resume detected %s", detection)
	}
	waitOutput(t, live, "WROTE")
	if used := h.sessionsUsed(); used != 1 {
		t.Fatalf("%d session slots used after the resume", used)
	}
}

func TestSupervisorRefusesPausesItCannotApply(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": turnFixture})
	if _, err := h.supervisor.Pause(h.project.ID, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pause of an unknown worker: %v", err)
	}
	startIdle(t, h, config.Snapshot{Config: config.Defaults()})
	const name = "claude-code-01"
	if _, err := h.supervisor.Resume(h.project.ID, name); !errors.Is(err, ErrTransition) {
		t.Fatalf("resume of an IDLE worker: %v", err)
	}
	if _, err := h.supervisor.Stop(h.project.ID, name); err != nil {
		t.Fatal(err)
	}
	if w, err := h.supervisor.Pause(h.project.ID, name); !errors.Is(err, ErrTransition) || w.State != Stopped {
		t.Fatalf("pause of a STOPPED worker: %+v %v", w, err)
	}

	// A worker recorded IDLE without a live session in this supervisor
	// cannot be paused.
	other := &Supervisor{Store: h.supervisor.Store, Projects: h.supervisor.Projects, Capacity: h.capacity}
	t.Cleanup(other.Close)
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, config.Snapshot{Config: config.Defaults()})); err != nil {
		t.Fatal(err)
	}
	h.settle(t, name, Idle)
	if _, err := other.Pause(h.project.ID, name); err == nil || !strings.Contains(err.Error(), "no live session") {
		t.Fatalf("pause without a live session: %v", err)
	}
}

func TestSupervisorStopsAPausedWorker(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": turnFixture})
	startIdle(t, h, config.Snapshot{Config: config.Defaults()})
	const name = "claude-code-01"
	if _, err := h.supervisor.Pause(h.project.ID, name); err != nil {
		t.Fatal(err)
	}
	stopped, err := h.supervisor.Stop(h.project.ID, name)
	if err != nil || stopped.State != Stopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	waitSlots(t, h, 0)
	if _, err := h.supervisor.Resume(h.project.ID, name); !errors.Is(err, ErrTransition) {
		t.Fatalf("resume of a stopped worker: %v", err)
	}
	// Its name and native session ownership are free again.
	startIdle(t, h, config.Snapshot{Config: config.Defaults()})
}

func TestSupervisorCloseKeepsPausedWorkersPaused(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": turnFixture})
	startIdle(t, h, config.Snapshot{Config: config.Defaults()})
	const name = "claude-code-01"
	if _, err := h.supervisor.Pause(h.project.ID, name); err != nil {
		t.Fatal(err)
	}
	h.supervisor.Close()
	waitSlots(t, h, 0)
	if w, err := h.supervisor.Store.Get(h.project.ID, name); err != nil || w.State != Paused {
		t.Fatalf("worker after close: %+v %v", w, err)
	}

	// The next daemon cannot resume a session it never held; a stop
	// releases the worker.
	next := &Supervisor{Store: h.supervisor.Store, Projects: h.supervisor.Projects, Capacity: h.capacity,
		Launcher: h.supervisor.Launcher, LookPath: h.supervisor.LookPath, StopGrace: h.supervisor.StopGrace}
	t.Cleanup(next.Close)
	if w, err := next.Resume(h.project.ID, name); !errors.Is(err, ErrTransition) ||
		!strings.Contains(err.Error(), "lost with a previous daemon") || w.State != Paused {
		t.Fatalf("resume in the next daemon: %+v %v", w, err)
	}
	if stopped, err := next.Stop(h.project.ID, name); err != nil || stopped.State != Stopped {
		t.Fatalf("stop in the next daemon: %+v %v", stopped, err)
	}
}

func TestSupervisorRefusesAResumeWhoseProfileDoesNotHold(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": turnFixture})
	startIdle(t, h, config.Snapshot{Config: config.Defaults()})
	const name = "claude-code-01"
	if _, err := h.supervisor.Pause(h.project.ID, name); err != nil {
		t.Fatal(err)
	}
	// Without its Git link, the worktree's revision cannot be read for
	// the profile to resume under.
	link := filepath.Join(h.project.WorkerWorktree(name), ".git")
	saved, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if w, err := h.supervisor.Resume(h.project.ID, name); !errors.Is(err, ErrGuard) || w.State != Paused {
		t.Fatalf("resume without a profile: %+v %v", w, err)
	}
	if err := os.WriteFile(link, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.supervisor.Resume(h.project.ID, name); err != nil {
		t.Fatal(err)
	}
	h.settle(t, name, Idle)
}

func TestSupervisorFailsAWorkerWhoseResumedAgentExits(t *testing.T) {
	const exitOnResume = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$2" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$2.jsonl"
if [ "$1" = "--resume" ]; then exit 4; fi
echo READY
exec cat
`
	h := newHarness(t, confined(t), 1, map[string]string{"claude": exitOnResume})
	startIdle(t, h, config.Snapshot{Config: config.Defaults()})
	const name = "claude-code-01"
	if _, err := h.supervisor.Pause(h.project.ID, name); err != nil {
		t.Fatal(err)
	}
	if _, err := h.supervisor.Resume(h.project.ID, name); err != nil {
		t.Fatal(err)
	}
	failed := h.settle(t, name, Failed)
	if !strings.Contains(failed.Reason, "resume failed") {
		t.Fatalf("failed with %q", failed.Reason)
	}
	waitSlots(t, h, 0)
	if stopped, err := h.supervisor.Stop(h.project.ID, name); err != nil || stopped.State != Stopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
}

func TestPauseInterruptsTheTurnOfABusyWorker(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": silentFixture})
	snapshot := config.Snapshot{Config: config.Defaults(), Instructions: map[string]string{}}
	db := h.supervisor.Store.DB
	configID, err := config.Persist(db, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	live := startIdle(t, h, snapshot)
	const name = "claude-code-01"
	base := testGit(t, h.project.Repository(), "rev-parse", "refs/heads/maestro/integration")
	created, err := task.Store{DB: db}.Create(h.project.ID, "add a flag file", configID, base)
	if err != nil {
		t.Fatal(err)
	}

	engine := &Engine{DB: db, Projects: h.supervisor.Projects, Sessions: h.supervisor, PollInterval: 50 * time.Millisecond}
	driven := make(chan error, 1)
	go func() { driven <- engine.Drive(context.Background(), h.project) }()
	busy := h.settle(t, name, Busy)
	deadline := time.Now().Add(15 * time.Second)
	// The turn runs once the agent is resumed with its rights.
	for {
		if state := live.epoch.State(); state.Epoch >= 2 && state.Phase == session.EpochActive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn never got its rights")
		}
		time.Sleep(20 * time.Millisecond)
	}
	group := live.epoch.Session()

	paused, err := h.supervisor.Pause(h.project.ID, name)
	if err != nil || paused.State != Paused || paused.Assignment != nil {
		t.Fatalf("pause: %+v %v", paused, err)
	}
	if group.State().Phase == session.Running {
		t.Fatal("the writable group of the turn still runs")
	}
	select {
	case err := <-driven:
		if err != nil {
			t.Fatalf("drive: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the drive never returned")
	}

	// The turn is interrupted and the task blocked with its
	// continuation; nothing else failed.
	u, err := turn.Store{DB: db}.Get(busy.Assignment.TurnID)
	if err != nil || u.State != turn.Interrupted {
		t.Fatalf("turn %+v %v", u, err)
	}
	got, err := task.Store{DB: db}.Get(created.ID)
	if err != nil || got.State != task.Blocked || !strings.Contains(got.BlockedReason, causePaused) {
		t.Fatalf("task %+v %v", got, err)
	}
	if w, err := h.supervisor.Store.Get(h.project.ID, name); err != nil || w.State != Paused {
		t.Fatalf("worker %+v %v", w, err)
	}
	if used := h.sessionsUsed(); used != 1 {
		t.Fatalf("%d session slots used while paused", used)
	}

	// The resumed worker is IDLE and takes no prompt on its own.
	if _, err := h.supervisor.Resume(h.project.ID, name); err != nil {
		t.Fatal(err)
	}
	h.settle(t, name, Idle)
	if got, err := (task.Store{DB: db}).Get(created.ID); err != nil || got.State != task.Blocked {
		t.Fatalf("task after the resume %+v %v", got, err)
	}
}

// pausableSessions finds the sessions of an engine harness and settles
// their turns against the pauses of a supervisor.
type pausableSessions struct {
	*engineHarness
	supervisor *Supervisor
}

func (p pausableSessions) Settling(projectID, name string) (func(), <-chan struct{}) {
	return p.supervisor.Settling(projectID, name)
}

// pauseAware makes the engine of the harness settle its turns against the
// pauses of a supervisor of its workers, which it returns.
func pauseAware(h *engineHarness) *Supervisor {
	s := &Supervisor{Store: h.engine.workers()}
	h.engine.Sessions = pausableSessions{engineHarness: h, supervisor: s}
	return s
}

// pauseTurn applies what Pause does once the worker's group is gone: it
// waits for a settling turn, then ends the turn the worker holds and
// moves it to PAUSED, as the pause in progress.
func pauseTurn(s *Supervisor, projectID, name string) (Worker, error) {
	s.init()
	key := liveKey{projectID, name}
	s.mu.Lock()
	for s.settling[key] != nil {
		settling := s.settling[key]
		s.mu.Unlock()
		<-settling
		s.mu.Lock()
	}
	done := make(chan struct{})
	s.pausing[key] = done
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pausing, key)
		s.mu.Unlock()
		close(done)
	}()
	w, err := s.Store.Get(projectID, name)
	if err != nil {
		return Worker{}, err
	}
	if w.Assignment != nil {
		if err := s.endPausedTurn(w); err != nil {
			return w, err
		}
	}
	return s.Store.transitionFrom(w, Input{Event: Pause, Reason: reasonPaused,
		Guard: Guard{InterruptConfirmed: true, DescendantsStopped: true}})
}

func TestSettlingAndPausingExcludeEachOther(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": turnFixture})
	startIdle(t, h, config.Snapshot{Config: config.Defaults()})
	const name = "claude-code-01"
	key := liveKey{h.project.ID, name}

	release, pausing := h.supervisor.Settling(h.project.ID, name)
	if release == nil || pausing != nil {
		t.Fatal("no settling mark on a worker no pause holds")
	}
	type result struct {
		w   Worker
		err error
	}
	paused := make(chan result, 1)
	go func() {
		w, err := h.supervisor.Pause(h.project.ID, name)
		paused <- result{w, err}
	}()
	// The pause waits for the settling turn: it takes nothing meanwhile.
	select {
	case r := <-paused:
		t.Fatalf("a pause ran while a turn was settling: %+v %v", r.w, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	if w, err := h.supervisor.Store.Get(h.project.ID, name); err != nil || w.State != Idle {
		t.Fatalf("worker %+v %v during the settling", w, err)
	}
	release()
	release()
	select {
	case r := <-paused:
		if r.err != nil || r.w.State != Paused {
			t.Fatalf("pause: %+v %v", r.w, r.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the pause never ran once the turn was settled")
	}

	// A pause in progress owns the turn: nothing is marked.
	done := make(chan struct{})
	h.supervisor.mu.Lock()
	h.supervisor.pausing[key] = done
	h.supervisor.mu.Unlock()
	if release, pausing := h.supervisor.Settling(h.project.ID, name); release != nil || pausing != done {
		t.Fatal("a turn was marked settling during a pause")
	}
	h.supervisor.mu.Lock()
	delete(h.supervisor.pausing, key)
	h.supervisor.mu.Unlock()
}

func TestEngineLeavesTheTurnAPauseTookAsItCompleted(t *testing.T) {
	h := newEngineHarness(t, config.Defaults(), planner)
	supervisor := pauseAware(h)
	var pauseErr error
	polled := false
	h.sessions[h.project.ID+"/"+h.worker.Name] = admissionHook{fakeSession: h.session,
		checkout: func() {}, send: func() {},
		poll: func() {
			if polled {
				return
			}
			polled = true
			// The agent ended its turn while a pause takes it.
			_, pauseErr = pauseTurn(supervisor, h.project.ID, h.worker.Name)
		}}
	h.drive(t)
	if pauseErr != nil {
		t.Fatal(pauseErr)
	}

	// The pause interrupted the turn and blocked the task; the job
	// applied nothing more.
	got, w := h.current(t)
	if got.State != task.Blocked || !strings.Contains(got.BlockedReason, causePaused) {
		t.Fatalf("task %s: %s", got.State, got.BlockedReason)
	}
	if w.State != Paused || w.Assignment != nil {
		t.Fatalf("worker %s holds %+v", w.State, w.Assignment)
	}
	if turns := h.turnStates(t); len(turns) != 1 || turns[0].State != turn.Interrupted {
		t.Fatalf("turns %+v", turns)
	}
	if _, err := handoff.Latest(h.engine.DB, h.task.ID, handoff.Plan); err == nil {
		t.Fatal("the handoff of a turn a pause took was accepted")
	}
	if h.session.settles != 0 || h.session.closes != 0 {
		t.Fatalf("%d settles, %d closes", h.session.settles, h.session.closes)
	}
	if usage, err := h.engine.budgets().Check(h.task.ID); err != nil || usage.Open != "" {
		t.Fatalf("time usage %+v %v", usage, err)
	}
}

// settleHook runs a function as the engine settles the turn of a fake
// session.
type settleHook struct {
	*fakeSession
	hook func()
}

func (s settleHook) Settle() error {
	s.hook()
	return s.fakeSession.Settle()
}

func TestPauseWaitsForTheTurnTheEngineSettles(t *testing.T) {
	h := newEngineHarness(t, config.Defaults(), planner)
	supervisor := pauseAware(h)
	type result struct {
		w   Worker
		err error
	}
	paused := make(chan result, 1)
	hooked := settleHook{fakeSession: h.session, hook: func() {
		// The pause is asked once the agent ended its turn, while the
		// engine settles it.
		go func() {
			w, err := pauseTurn(supervisor, h.project.ID, h.worker.Name)
			paused <- result{w, err}
		}()
		select {
		case r := <-paused:
			t.Errorf("a pause ran while the turn was settling: %+v %v", r.w, r.err)
		case <-time.After(200 * time.Millisecond):
		}
	}}
	j := &job{e: h.engine, project: h.project, task: h.task, worker: h.worker, session: hooked,
		role: stages[task.New].role, kind: stages[task.New].kind}
	if err := j.run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The engine accepted the turn, then the pause took the IDLE worker:
	// the task goes on once the worker is resumed.
	var r result
	select {
	case r = <-paused:
	case <-time.After(15 * time.Second):
		t.Fatal("the pause never ran once the turn was settled")
	}
	if r.err != nil || r.w.State != Paused || r.w.Assignment != nil {
		t.Fatalf("pause: %+v %v", r.w, r.err)
	}
	if !sameEvents(h.events(t), task.Created, task.Assign, task.AcceptPlan) {
		t.Fatalf("events %v", h.events(t))
	}
	if turns := h.turnStates(t); len(turns) != 1 || turns[0].State != turn.Succeeded {
		t.Fatalf("turns %+v", turns)
	}
	if _, err := handoff.Latest(h.engine.DB, h.task.ID, handoff.Plan); err != nil {
		t.Fatal(err)
	}
	if usage, err := h.engine.budgets().Check(h.task.ID); err != nil || usage.Open != "" {
		t.Fatalf("time usage %+v %v", usage, err)
	}
}

// admissionHook runs functions as the engine checks out the task, sends
// the prompt and polls the turn of a fake session.
type admissionHook struct {
	*fakeSession
	checkout, send, poll func()
}

func (s admissionHook) Checkout(t task.Task, revision string) (string, error) {
	s.checkout()
	return s.fakeSession.Checkout(t, revision)
}

func (s admissionHook) Send(prompt string) error {
	s.send()
	return s.fakeSession.Send(prompt)
}

func (s admissionHook) Poll() agent.Detection {
	s.poll()
	return s.fakeSession.Poll()
}

func TestPauseWaitsForThePromptOfAnAdmittedTurn(t *testing.T) {
	h := newEngineHarness(t, config.Defaults(), planner)
	supervisor := pauseAware(h)
	type result struct {
		w   Worker
		err error
	}
	paused := make(chan result, 1)
	var r *result
	notYet := func(when string) {
		select {
		case got := <-paused:
			r = &got
			t.Errorf("a pause took the turn %s: %+v %v", when, got.w, got.err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	hooked := admissionHook{fakeSession: h.session,
		checkout: func() {
			// The pause is asked once the worker took the turn, while
			// the engine checks out the task.
			go func() {
				w, err := pauseTurn(supervisor, h.project.ID, h.worker.Name)
				paused <- result{w, err}
			}()
			notYet("before its prompt was sent")
		},
		send: func() { notYet("as its prompt was sent") },
		poll: func() {
			if r != nil {
				return
			}
			// Once the prompt is sent, the pause takes the turn.
			select {
			case got := <-paused:
				r = &got
			case <-time.After(15 * time.Second):
				t.Fatal("the pause never ran once the prompt was sent")
			}
		}}
	h.sessions[h.project.ID+"/"+h.worker.Name] = hooked
	h.drive(t)

	if r == nil || r.err != nil || r.w.State != Paused || r.w.Assignment != nil {
		t.Fatalf("pause: %+v", r)
	}
	// The pause interrupted the running turn and blocked the task; the
	// job applied nothing more.
	got, w := h.current(t)
	if got.State != task.Blocked || !strings.Contains(got.BlockedReason, causePaused) {
		t.Fatalf("task %s: %s", got.State, got.BlockedReason)
	}
	if w.State != Paused || w.Assignment != nil {
		t.Fatalf("worker %s holds %+v", w.State, w.Assignment)
	}
	if !sameEvents(h.events(t), task.Created, task.Assign, task.Block) {
		t.Fatalf("events %v", h.events(t))
	}
	if turns := h.turnStates(t); len(turns) != 1 || turns[0].State != turn.Interrupted {
		t.Fatalf("turns %+v", turns)
	}
	if len(h.session.prompts) != 1 || h.session.settles != 0 || h.session.closes != 0 {
		t.Fatalf("%d prompts, %d settles, %d closes", len(h.session.prompts), h.session.settles, h.session.closes)
	}
	if usage, err := h.engine.budgets().Check(h.task.ID); err != nil || usage.Open != "" {
		t.Fatalf("time usage %+v %v", usage, err)
	}
}
