// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package attach

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/worker"
	"github.com/goabonga/maestro/internal/worktree"
)

// pilots attaches the worker "claude-01" to a fixture session, refuses
// with refusal when it is set, and reports how each attach ended.
type pilots struct {
	live    *session.Session
	refusal error
	ended   chan bool
}

func (p *pilots) Attach(_, name string) (*session.Session, func(bool), error) {
	switch {
	case p.refusal != nil:
		return nil, nil, p.refusal
	case name != "claude-01":
		return nil, nil, fmt.Errorf("%w: %s", worker.ErrNotFound, name)
	}
	return p.live, func(detached bool) { p.ended <- detached }, nil
}

// workerFixture serves a confined session as the live session of the
// worker "claude-01" of a registered project, and returns the socket and
// the project's id with the pilots.
func workerFixture(t *testing.T, argv ...string) (string, string, *pilots) {
	t.Helper()
	live := confinedSession(t, argv...)
	repository := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"},
		{"config", "commit.gpgsign", "false"}, {"commit", "-q", "--allow-empty", "-m", "feat: initial"},
	} {
		command := exec.Command("git", args...)
		command.Dir = repository
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	store := worktree.Store{Base: t.TempDir()}
	project, _, err := store.Init(repository)
	if err != nil {
		t.Fatal(err)
	}
	p := &pilots{live: live, ended: make(chan bool, 1)}
	server := &ipc.Server{DB: db, Store: store, Service: "maestro-svc", Version: "0.0.0",
		Workers: &worker.Store{DB: db}, Pilots: p}
	return serve(t, server), project.ID, p
}

func TestWorkerPilotsAWorkerAndHandsItBackOnCtrlBracket(t *testing.T) {
	socket, project, p := workerFixture(t, "/bin/sh")
	keyboard, terminal := terminalPair(t)
	before := modes(t, terminal)
	output := &lockedBuffer{}
	finished := make(chan error, 1)
	go func() {
		finished <- Worker(context.Background(), socket, project, "claude-01", Terminal{In: terminal, Out: output})
	}()
	if _, err := keyboard.Write([]byte("echo piloted-$((40+2))\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, output, "piloted-42")
	if _, err := keyboard.Write([]byte{DetachKey}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("detach returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Ctrl-] did not detach")
	}
	select {
	case detached := <-p.ended:
		if !detached {
			t.Fatal("Ctrl-] was reported as a lost connection")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker was never handed back")
	}
	if modes(t, terminal) != before {
		t.Fatal("the terminal was not restored after detach")
	}
}

func TestWorkerReportsARefusalWithoutTouchingTheTerminal(t *testing.T) {
	socket, project, p := workerFixture(t, "/bin/cat")
	_, terminal := terminalPair(t)
	before := modes(t, terminal)

	err := Worker(context.Background(), socket, project, "missing", Terminal{In: terminal, Out: &lockedBuffer{}})
	if err == nil || !strings.Contains(err.Error(), "unknown worker in project "+project+": missing") {
		t.Fatalf("error %v", err)
	}
	p.refusal = fmt.Errorf("attach in BUSY: %w: the turn is neither quiescent nor interrupted", worker.ErrGuard)
	err = Worker(context.Background(), socket, project, "claude-01", Terminal{In: terminal, Out: &lockedBuffer{}})
	if err == nil || !strings.Contains(err.Error(), "the turn is neither quiescent nor interrupted") {
		t.Fatalf("error %v", err)
	}
	if modes(t, terminal) != before {
		t.Fatal("a refused attach changed the terminal")
	}
}
