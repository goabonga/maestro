// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/goabonga/maestro/internal/attach"
	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/worker"
)

func TestAttachKeyWaitsForAWorker(t *testing.T) {
	server, socket, project := daemonWith(t, func(*ipc.Server) {})
	taskID := createTask(t, socket, project.ID, "add a verbose flag")
	m := start(t, Options{Socket: socket})
	if strings.Contains(m.View(), "a attach") {
		t.Fatalf("the dashboard offers an attach:\n%s", m.View())
	}
	// Screens without a worker ignore the key.
	for _, screen := range []string{"dashboard", "tasks"} {
		if _, cmd := m.Update(key("a")); cmd != nil {
			t.Fatalf("a on the %s ran a command", screen)
		}
		m = send(t, m, key("enter"))
	}
	contains(t, m.View(), "maestro · task "+taskID, "a attach")
	m = send(t, m, key("a"))
	contains(t, m.View(), "no worker drives this task")

	// Once a worker drives the task, a attaches it.
	registerWorker(t, server, project, "claude-01")
	assignWorker(t, server, project, "claude-01", taskID, "implement the flag")
	m = send(t, m, key("r"))
	if _, cmd := m.Update(key("a")); cmd == nil {
		t.Fatal("a on a driven task attached nothing")
	}
	m = send(t, m, key("esc"))
	m = send(t, m, key("w"))
	contains(t, m.View(), "> claude-01", "a attach")
	if _, cmd := m.Update(key("a")); cmd == nil {
		t.Fatal("a on the workers screen attached nothing")
	}
}

func TestAttachedMessageShowsTheOutcomeAndRefreshes(t *testing.T) {
	socket, _ := daemon(t)
	m := start(t, Options{Socket: socket})
	m = send(t, m, attachedMsg{worker: "claude-01"})
	contains(t, m.View(), "detached from claude-01", "daemon: maestro-svc")

	m = send(t, m, attachedMsg{worker: "claude-02", err: errors.New("worker claude-02: \x1b[2Jexited")})
	contains(t, m.View(), "worker claude-02: ?[2Jexited")
	if strings.Contains(m.View(), "detached from") {
		t.Fatalf("the previous outcome is still shown:\n%s", m.View())
	}
}

func TestAttachNeedsATerminal(t *testing.T) {
	exec := &attachExec{socket: "unused", project: "p1", worker: "claude-01"}
	exec.SetStdin(strings.NewReader(""))
	exec.SetStdout(io.Discard)
	exec.SetStderr(io.Discard)
	if err := exec.Run(); !errors.Is(err, ErrNoTerminal) {
		t.Fatalf("error %v", err)
	}
}

// pilots attaches every worker to one fixture session, or refuses with
// refusal when it is set.
type pilots struct {
	mu      sync.Mutex
	live    *session.Session
	refusal error
}

func (p *pilots) Attach(_, _ string) (*session.Session, func(bool), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refusal != nil {
		return nil, nil, p.refusal
	}
	return p.live, func(bool) {}, nil
}

// refuse makes the next attaches fail with err.
func (p *pilots) refuse(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refusal = err
}

// display records what the program draws on the terminal.
type display struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *display) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// since returns what was drawn after offset.
func (s *display) since(offset int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()[offset:]
}

// mark returns the current end of the recording.
func (s *display) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

// await waits until the screen shows want after offset.
func (s *display) await(t *testing.T, offset int, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.since(offset), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the screen never showed %q: %q", want, s.since(offset))
}

// termModes captures the termios flags that raw mode changes.
func termModes(t *testing.T, terminal *os.File) [3]uint32 {
	t.Helper()
	settings, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return [3]uint32{settings.Iflag, settings.Oflag, settings.Lflag}
}

// sessionDaemon serves a confined shell session as the live session of
// the worker "claude-01" of a registered project, and returns the
// socket, the project, the pilots and a PTY pair standing in for the
// user's terminal.
func sessionDaemon(t *testing.T) (string, string, *pilots, *os.File, *os.File) {
	t.Helper()
	probed, err := launcher.New()
	if errors.Is(err, launcher.ErrUnsupported) {
		t.Skipf("host cannot confine: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	live, err := session.Start(probed, session.Config{Spec: launcher.Spec{Argv: []string{"/bin/sh"}, Dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Stop(200 * time.Millisecond) })
	p := &pilots{live: live}
	server, socket, project := daemonWith(t, func(server *ipc.Server) { server.Pilots = p })
	registerWorker(t, server, project, "claude-01")

	keyboard, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keyboard.Close(); _ = terminal.Close() })
	if err := pty.Setsize(terminal, &pty.Winsize{Rows: 40, Cols: 160}); err != nil {
		t.Fatal(err)
	}
	return socket, project.ID, p, keyboard, terminal
}

// typeKeys writes keystrokes on the terminal.
func typeKeys(t *testing.T, keyboard *os.File, keys string) {
	t.Helper()
	if _, err := keyboard.Write([]byte(keys)); err != nil {
		t.Fatal(err)
	}
}

// suspended is written when the program leaves the alternate screen to
// hand the terminal over.
const suspended = "\x1b[?1049l"

func TestRunAttachesTheSelectedWorkerAndResumes(t *testing.T) {
	socket, project, p, keyboard, terminal := sessionDaemon(t)
	before := termModes(t, terminal)
	drawn := &display{}
	go func() { _, _ = io.Copy(drawn, keyboard) }()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{Socket: socket, Project: project}, terminal, terminal)
	}()
	drawn.await(t, 0, "maestro · tasks of "+project)
	typeKeys(t, keyboard, "w")
	drawn.await(t, 0, "> claude-01")

	// Attach, run a command in the session, detach with Ctrl-].
	mark := drawn.mark()
	typeKeys(t, keyboard, "a")
	drawn.await(t, mark, suspended)
	typeKeys(t, keyboard, "echo attached-$((40+2))\r")
	drawn.await(t, mark, "attached-42")
	mark = drawn.mark()
	typeKeys(t, keyboard, string([]byte{attach.DetachKey}))
	drawn.await(t, mark, "detached from claude-01")

	// A refused attach resumes the dashboard with the daemon's reason.
	p.refuse(fmt.Errorf("attach in BUSY: %w: the turn is neither quiescent nor interrupted", worker.ErrGuard))
	mark = drawn.mark()
	typeKeys(t, keyboard, "a")
	drawn.await(t, mark, "the turn is neither quiescent nor interrupted")

	// A session ending during the attach resumes it too.
	p.refuse(nil)
	mark = drawn.mark()
	typeKeys(t, keyboard, "a")
	drawn.await(t, mark, suspended)
	typeKeys(t, keyboard, "exit 3\r")
	drawn.await(t, mark, "exited")
	drawn.await(t, mark, "a attach")

	typeKeys(t, keyboard, "q")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("q does not quit after the attaches")
	}
	if termModes(t, terminal) != before {
		t.Fatal("the terminal was not restored")
	}
}
