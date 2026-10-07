// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/turn"
)

// awaitEcho types a line in a terminal and waits for its echo.
func awaitEcho(t *testing.T, terminal *session.Session, line string) {
	t.Helper()
	output, cancel := terminal.Subscribe(64)
	defer cancel()
	if _, err := terminal.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
	var seen strings.Builder
	deadline := time.After(10 * time.Second)
	for !strings.Contains(seen.String(), line) {
		select {
		case chunk, open := <-output:
			if !open {
				t.Fatalf("the terminal closed before echoing %q: %q", line, seen.String())
			}
			seen.Write(chunk)
		case <-deadline:
			t.Fatalf("the terminal never echoed %q: %q", line, seen.String())
		}
	}
}

// startedWorker starts one worker of the fixture agent and waits for it
// to be IDLE.
func startedWorker(t *testing.T, fixture string) harness {
	t.Helper()
	h := newHarness(t, confined(t), 1, map[string]string{"claude": fixture})
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, config.Snapshot{Config: config.Defaults()})); err != nil {
		t.Fatal(err)
	}
	h.settle(t, "claude-code-01", Idle)
	return h
}

func TestAttachTakesAnIdleWorkerAndDetachReturnsItToIdle(t *testing.T) {
	h := startedWorker(t, claudeFixture)
	terminal, end, err := h.supervisor.Attach(h.project.ID, "claude-code-01")
	if err != nil {
		t.Fatal(err)
	}
	h.settle(t, "claude-code-01", Attached)
	awaitEcho(t, terminal, "piloted-by-hand")

	// One pilot at a time, and the engine assigns nothing meanwhile.
	if _, _, err := h.supervisor.Attach(h.project.ID, "claude-code-01"); !errors.Is(err, ErrTransition) {
		t.Fatalf("a second pilot attached: %v", err)
	}
	if _, err := h.supervisor.Store.Transition(h.project.ID, "claude-code-01", Input{Event: Assign,
		Assignment: Assignment{TaskID: "t1", Role: Implementation, TurnID: "u1"},
		Guard:      Guard{AssignmentPersisted: true}}); !errors.Is(err, ErrTransition) {
		t.Fatalf("an attached worker took an assignment: %v", err)
	}

	end(true)
	h.settle(t, "claude-code-01", Idle)
	_, end, err = h.supervisor.Attach(h.project.ID, "claude-code-01")
	if err != nil {
		t.Fatal(err)
	}
	end(false)
	h.settle(t, "claude-code-01", Idle)
	events := h.events(t, "claude-code-01")
	if got := events[len(events)-4:]; got[0] != Attach || got[1] != Detach || got[2] != Attach || got[3] != ConnectionLost {
		t.Fatalf("events %v", events)
	}
	if h.sessionsUsed() != 1 {
		t.Fatalf("the attach changed the sessions used: %d", h.sessionsUsed())
	}
}

func TestAttachRefusesABusyWorkerAndKeepsAWaitingOnesContinuation(t *testing.T) {
	h := startedWorker(t, claudeFixture)
	db := h.supervisor.Store.DB
	configID, err := config.Persist(db, config.Snapshot{Config: config.Defaults(), Instructions: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := task.Store{DB: db}.Create(h.project.ID, "add a flag", configID, strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	u, err := turn.Store{DB: db}.Create(created.ID, "claude-code", configID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.supervisor.Store.Transition(h.project.ID, "claude-code-01", Input{Event: Assign,
		Assignment: Assignment{TaskID: created.ID, Role: Implementation, TurnID: u.ID},
		Guard:      Guard{AssignmentPersisted: true}}); err != nil {
		t.Fatal(err)
	}

	// The turn runs: the attach is refused and the worker stays BUSY.
	if _, _, err := h.supervisor.Attach(h.project.ID, "claude-code-01"); !errors.Is(err, ErrGuard) {
		t.Fatalf("a busy worker was attached: %v", err)
	}
	h.settle(t, "claude-code-01", Busy)

	// The turn waits for input: the human answers it and hands it back.
	if _, err := h.supervisor.Store.Transition(h.project.ID, "claude-code-01", Input{Event: RequestInput,
		Guard: Guard{InputRecognized: true}}); err != nil {
		t.Fatal(err)
	}
	_, end, err := h.supervisor.Attach(h.project.ID, "claude-code-01")
	if err != nil {
		t.Fatal(err)
	}
	attached := h.settle(t, "claude-code-01", Attached)
	if attached.Assignment == nil || attached.Assignment.TurnID != u.ID {
		t.Fatalf("the attach dropped the assignment: %+v", attached.Assignment)
	}
	end(true)
	waiting := h.settle(t, "claude-code-01", WaitingInput)
	if waiting.Assignment == nil || waiting.Assignment.TurnID != u.ID {
		t.Fatalf("the detach dropped the continuation: %+v", waiting.Assignment)
	}
}

func TestAttachRefusesAWorkerWithoutALiveSession(t *testing.T) {
	h := startedWorker(t, claudeFixture)
	if _, _, err := h.supervisor.Attach(h.project.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown worker was attached: %v", err)
	}
	if _, err := h.supervisor.Stop(h.project.ID, "claude-code-01"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.supervisor.Attach(h.project.ID, "claude-code-01"); !errors.Is(err, ErrTransition) {
		t.Fatalf("a stopped worker was attached: %v", err)
	}
}

func TestDetachFromAnEndedSessionFailsTheWorker(t *testing.T) {
	h := startedWorker(t, signalledFixture)
	terminal, end, err := h.supervisor.Attach(h.project.ID, "claude-code-01")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.project.WorkerWorktree("claude-code-01"), "end-session"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	terminal.Wait()
	end(true)
	failed := h.settle(t, "claude-code-01", Failed)
	if !strings.Contains(failed.Reason, "session") {
		t.Fatalf("reason %q", failed.Reason)
	}
}
