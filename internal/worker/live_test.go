// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/agent"
	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/turn"
)

// turnFixture records its chosen or resumed session like claudeFixture,
// then answers every pasted prompt like Claude Code ending a turn, after
// trying to write a file in its worktree and reporting whether it could.
const turnFixture = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$2" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$2.jsonl"
echo READY
while IFS= read -r line; do
	case "$line" in
	*201~*)
		if touch "probe-$$" 2>/dev/null; then echo WROTE; else echo READONLY; fi
		printf '\342\227\217 turn over\n\342\234\273 Baked for 1s \302\267 done (fixture)\n\342\235\257 \n'
		;;
	esac
done
`

// pollUntil polls a live session until its detection is final.
func pollUntil(t *testing.T, s Session) agent.Detection {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		switch detection := s.Poll(); detection {
		case agent.DetectRunning, agent.DetectUnknown:
		default:
			return detection
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn never ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitOutput waits for marker to appear in the output of the current
// PTY of a live session.
func waitOutput(t *testing.T, live *running, marker string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		output, _ := live.epoch.Session().Output()
		if strings.Contains(string(output), marker) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q in the output:\n%s", marker, output)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLiveSessionDrivesTurnsAndRevokesTheirRights(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": turnFixture})
	snapshot := config.Snapshot{
		Config:       config.Defaults(),
		Instructions: map[string]string{"agents/claude-code/CLAUDE.md": "work carefully\n"},
	}
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	const name = "claude-code-01"
	h.settle(t, name, Idle)
	if _, ok := h.supervisor.Session(h.project.ID, "claude-code-02"); ok {
		t.Fatal("a worker that never started has a live session")
	}
	s, ok := h.supervisor.Session(h.project.ID, name)
	if !ok {
		t.Fatal("an IDLE worker has no live session")
	}
	h.supervisor.mu.Lock()
	live := h.supervisor.live[liveKey{h.project.ID, name}]
	h.supervisor.mu.Unlock()
	if paths := s.RuntimePaths(); !slices.Contains(paths, "CLAUDE.md") {
		t.Fatalf("runtime paths %v", paths)
	}

	// The task branch is checked out at the revision, without what a
	// previous turn left untracked; the instruction files stay.
	workTree := h.project.WorkerWorktree(name)
	if err := os.WriteFile(filepath.Join(workTree, "leftover"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	head := testGit(t, h.project.Repository(), "rev-parse", "refs/heads/maestro/integration")
	dir, err := s.Checkout(task.Task{Branch: "maestro/task-1"}, head)
	if err != nil || dir != workTree {
		t.Fatalf("checkout in %q: %v", dir, err)
	}
	if branch := testGit(t, workTree, "symbolic-ref", "HEAD"); branch != "refs/heads/maestro/task-1" {
		t.Fatalf("checked out %s", branch)
	}
	if got := testGit(t, workTree, "rev-parse", "HEAD"); got != head {
		t.Fatalf("checked out %s, want %s", got, head)
	}
	if _, err := os.Stat(filepath.Join(workTree, "leftover")); !os.IsNotExist(err) {
		t.Fatalf("the untracked file survived the checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workTree, "CLAUDE.md")); err != nil {
		t.Fatalf("the instruction file was removed: %v", err)
	}

	// Before its first turn, the agent cannot write its worktree.
	if live.role != "review" {
		t.Fatalf("started with role %s", live.role)
	}
	before, err := agent.PromptInput("try to write before the first turn")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.epoch.Session().Write(before); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, live, "READONLY")

	// A turn runs with write access and ends on the driver's detection.
	if err := s.Send("write a probe"); err != nil {
		t.Fatal(err)
	}
	if detection := pollUntil(t, s); detection != agent.DetectCompleted {
		t.Fatalf("first turn detected %s", detection)
	}
	waitOutput(t, live, "WROTE")

	// Settling resumes the conversation read only: the agent can no
	// longer write its worktree.
	if err := s.Settle(); err != nil {
		t.Fatal(err)
	}
	if state := live.epoch.State(); state.Epoch != 3 || live.role != "review" {
		t.Fatalf("settled into epoch %d with role %s", state.Epoch, live.role)
	}
	input, err := agent.PromptInput("try to write")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.epoch.Session().Write(input); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, live, "READONLY")

	// The next turn is granted write access again.
	if err := s.Send("write another probe"); err != nil {
		t.Fatal(err)
	}
	if detection := pollUntil(t, s); detection != agent.DetectCompleted {
		t.Fatalf("second turn detected %s", detection)
	}
	waitOutput(t, live, "WROTE")
	if state := live.epoch.State(); state.Epoch != 4 {
		t.Fatalf("second turn in epoch %d", state.Epoch)
	}

	if err := s.Interrupt(); err != nil {
		t.Fatal(err)
	}
	if detection := s.Poll(); detection != agent.DetectInterrupted {
		t.Fatalf("interrupted turn detected %s", detection)
	}

	// The changes of rights neither failed the worker nor released its
	// slot; a stop ends the resumed session.
	if w, err := h.supervisor.Store.Get(h.project.ID, name); err != nil || w.State != Idle {
		t.Fatalf("worker %+v %v", w, err)
	}
	if used := h.sessionsUsed(); used != 1 {
		t.Fatalf("%d session slots used", used)
	}
	if _, err := h.supervisor.Stop(h.project.ID, name); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.supervisor.Session(h.project.ID, name); ok {
		t.Fatal("a stopped worker has a live session")
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.sessionsUsed() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the slot was never released")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLiveSessionFailsTheWorkerWhenTheResumedAgentExits(t *testing.T) {
	// The agent ends on its own once resumed for its first turn: the
	// change of rights fails and the watcher fails the worker, releasing
	// its slot.
	const exitOnResume = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$2" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$2.jsonl"
if [ "$1" = "--resume" ]; then exit 4; fi
echo READY
exec cat
`
	h := newHarness(t, confined(t), 1, map[string]string{"claude": exitOnResume})
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, config.Snapshot{Config: config.Defaults()})); err != nil {
		t.Fatal(err)
	}
	const name = "claude-code-01"
	h.settle(t, name, Idle)
	s, ok := h.supervisor.Session(h.project.ID, name)
	if !ok {
		t.Fatal("no live session")
	}
	if err := s.Send("work"); err == nil {
		t.Fatal("a resumed agent that exits took a turn")
	}
	failed := h.settle(t, name, Failed)
	if !strings.Contains(failed.Reason, "exited with code 4") {
		t.Fatalf("failed with %q", failed.Reason)
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.sessionsUsed() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the slot was never released")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := h.supervisor.Session(h.project.ID, name); ok {
		t.Fatal("a failed worker has a live session")
	}
}

// silentFixture records its chosen or resumed session like
// claudeFixture, draws its terminal, then never ends a turn.
const silentFixture = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$2" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$2.jsonl"
echo READY
exec cat >/dev/null
`

// waitGone waits for the current group of a live session to end and for
// every session slot to be released.
func waitGone(t *testing.T, h harness, live *running) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h.sessionsUsed() != 0 || live.epoch.Session().State().Phase == session.Running {
		if time.Now().After(deadline) {
			t.Fatalf("%d slots used, session %s", h.sessionsUsed(), live.epoch.Session().State().Phase)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLiveSessionOfATurnThatTimesOutIsTornDown(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": silentFixture})
	cfg := config.Defaults()
	cfg.Budgets.TurnTimeout = config.Duration{Duration: time.Second}
	snapshot := config.Snapshot{Config: cfg, Instructions: map[string]string{}}
	db := h.supervisor.Store.DB
	configID, err := config.Persist(db, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	const name = "claude-code-01"
	h.settle(t, name, Idle)
	h.supervisor.mu.Lock()
	live := h.supervisor.live[liveKey{h.project.ID, name}]
	h.supervisor.mu.Unlock()
	base := testGit(t, h.project.Repository(), "rev-parse", "refs/heads/maestro/integration")
	created, err := task.Store{DB: db}.Create(h.project.ID, "add a flag file", configID, base)
	if err != nil {
		t.Fatal(err)
	}

	engine := &Engine{DB: db, Projects: h.supervisor.Projects, Sessions: h.supervisor, PollInterval: 50 * time.Millisecond}
	if err := engine.Drive(context.Background(), h.project); err != nil {
		t.Fatal(err)
	}
	got, err := task.Store{DB: db}.Get(created.ID)
	if err != nil || got.State != task.Blocked || !strings.Contains(got.BlockedReason, string(turn.TurnTimeout)) {
		t.Fatalf("task %+v %v", got, err)
	}

	// The worker failed: its writable agent is gone, its slot released,
	// and a stop releases its assignment.
	if w, err := h.supervisor.Store.Get(h.project.ID, name); err != nil || w.State != Failed || w.Assignment == nil {
		t.Fatalf("worker %+v %v", w, err)
	}
	if _, ok := h.supervisor.Session(h.project.ID, name); ok {
		t.Fatal("a failed worker has a live session")
	}
	waitGone(t, h, live)
	stopped, err := h.supervisor.Stop(h.project.ID, name)
	if err != nil || stopped.State != Stopped || stopped.Assignment != nil {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); err != nil {
		t.Fatalf("no worker can start once the failed one stopped: %v", err)
	}
	h.settle(t, name, Idle)
}

func TestLiveSessionThatNeverSettlesOnResumeIsTornDownOnClose(t *testing.T) {
	// The resumed agent keeps running without drawing anything: the
	// change of rights fails with the group alive, and Close, which the
	// engine calls once it fails the worker, terminates it.
	const mute = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$2" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$2.jsonl"
if [ "$1" != "--resume" ]; then echo READY; fi
exec cat >/dev/null
`
	h := newHarness(t, confined(t), 1, map[string]string{"claude": mute})
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, config.Snapshot{Config: config.Defaults()})); err != nil {
		t.Fatal(err)
	}
	const name = "claude-code-01"
	h.settle(t, name, Idle)
	h.supervisor.StartTimeout = time.Second
	s, ok := h.supervisor.Session(h.project.ID, name)
	if !ok {
		t.Fatal("no live session")
	}
	h.supervisor.mu.Lock()
	live := h.supervisor.live[liveKey{h.project.ID, name}]
	h.supervisor.mu.Unlock()
	if err := s.Send("work"); err == nil || !strings.Contains(err.Error(), "did not settle") {
		t.Fatalf("send: %v", err)
	}
	if state := live.epoch.Session().State(); state.Phase != session.Running {
		t.Fatalf("the resumed group is %s before the close", state.Phase)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	waitGone(t, h, live)
	if _, ok := h.supervisor.Session(h.project.ID, name); ok {
		t.Fatal("a closed session is still live")
	}
}
