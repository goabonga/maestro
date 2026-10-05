// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/goabonga/maestro/internal/attach"
	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/worktree"
)

func TestAttachPromptEditsAndCancels(t *testing.T) {
	socket, _ := daemon(t)
	m := start(t, Options{Socket: socket})
	contains(t, m.View(), "a attach")

	m = send(t, m, key("a"))
	contains(t, m.View(), "attach to session: ", "enter attach · esc cancel")
	// Keys go to the prompt: q types, it does not quit.
	for _, k := range []tea.KeyMsg{key("q"), key("x"), {Type: tea.KeyRunes, Runes: []rune(" \x1b")}} {
		next, cmd := m.Update(k)
		if cmd != nil {
			t.Fatalf("%q ran a command in the prompt", k.String())
		}
		m = next.(Model)
	}
	contains(t, m.View(), "attach to session: qx▏")
	m = send(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	contains(t, m.View(), "attach to session: q▏")

	m = send(t, m, key("esc"))
	if strings.Contains(m.View(), "attach to session") {
		t.Fatalf("esc left the prompt open:\n%s", m.View())
	}
	contains(t, m.View(), "maestro · dashboard")
}

func TestAttachPromptIgnoresAnEmptyIDAndQuitsOnCtrlC(t *testing.T) {
	m := New(Options{Socket: "unused"})
	m = send(t, m, key("a"))
	next, cmd := m.Update(key("enter"))
	if cmd != nil || next.(Model).prompting {
		t.Fatal("an empty session id started an attach")
	}
	m = send(t, m, key("a"))
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c does not quit from the prompt")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+c does not quit from the prompt")
	}
}

func TestAttachedMessageShowsTheOutcomeAndRefreshes(t *testing.T) {
	socket, _ := daemon(t)
	m := start(t, Options{Socket: socket})
	m = send(t, m, attachedMsg{session: "s1"})
	contains(t, m.View(), "detached from s1", "daemon: maestro-svc")

	m = send(t, m, attachedMsg{session: "s2", err: errors.New("session s2: \x1b[2Jexited")})
	contains(t, m.View(), "session s2: ?[2Jexited")
	if strings.Contains(m.View(), "detached from") {
		t.Fatalf("the previous outcome is still shown:\n%s", m.View())
	}
}

func TestAttachNeedsATerminal(t *testing.T) {
	exec := &attachExec{socket: "unused", session: "s1"}
	exec.SetStdin(strings.NewReader(""))
	exec.SetStdout(io.Discard)
	exec.SetStderr(io.Discard)
	if err := exec.Run(); !errors.Is(err, ErrNoTerminal) {
		t.Fatalf("error %v", err)
	}
}

// oneSession resolves a single session under the id "s1".
type oneSession struct{ live *session.Session }

func (o oneSession) Session(id string) (*session.Session, bool) {
	return o.live, id == "s1"
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

// sessionDaemon serves a confined shell session "s1" on a Unix socket
// and returns a PTY pair standing in for the user's terminal.
func sessionDaemon(t *testing.T) (string, *os.File, *os.File) {
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

	dir, err := os.MkdirTemp("", "maestro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "svc.sock")
	listener, err := transport.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &ipc.Server{Service: "maestro-svc", Version: "0.0.0",
		Store: worktree.Store{Base: t.TempDir()}, Sessions: oneSession{live}}
	web := &http.Server{Handler: server.Handler()}
	go func() { _ = web.Serve(listener) }()
	t.Cleanup(func() { _ = web.Close() })

	keyboard, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keyboard.Close(); _ = terminal.Close() })
	if err := pty.Setsize(terminal, &pty.Winsize{Rows: 40, Cols: 160}); err != nil {
		t.Fatal(err)
	}
	return socket, keyboard, terminal
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

func TestRunAttachesToASessionAndResumes(t *testing.T) {
	socket, keyboard, terminal := sessionDaemon(t)
	before := termModes(t, terminal)
	drawn := &display{}
	go func() { _, _ = io.Copy(drawn, keyboard) }()
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), Options{Socket: socket}, terminal, terminal) }()
	drawn.await(t, 0, "maestro · dashboard")
	drawn.await(t, 0, "a attach")

	// Attach, run a command in the session, detach with Ctrl-].
	mark := drawn.mark()
	typeKeys(t, keyboard, "a")
	drawn.await(t, mark, "attach to session:")
	mark = drawn.mark()
	typeKeys(t, keyboard, "s1\r")
	drawn.await(t, mark, suspended)
	typeKeys(t, keyboard, "echo attached-$((40+2))\r")
	drawn.await(t, mark, "attached-42")
	mark = drawn.mark()
	typeKeys(t, keyboard, string([]byte{attach.DetachKey}))
	drawn.await(t, mark, "detached from s1")

	// A refused attach resumes the dashboard with the error.
	mark = drawn.mark()
	typeKeys(t, keyboard, "a")
	drawn.await(t, mark, "attach to session:")
	mark = drawn.mark()
	typeKeys(t, keyboard, "missing\r")
	drawn.await(t, mark, "unknown session: missing")

	// A session ending during the attach resumes it too.
	mark = drawn.mark()
	typeKeys(t, keyboard, "a")
	drawn.await(t, mark, "attach to session:")
	mark = drawn.mark()
	typeKeys(t, keyboard, "s1\r")
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
