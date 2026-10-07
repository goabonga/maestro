// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package attach

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/transport"
)

// lockedBuffer is a concurrency-safe output sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// oneSession resolves a single session under the id "s1".
type oneSession struct{ live *session.Session }

func (o oneSession) Session(id string) (*session.Session, bool) {
	return o.live, id == "s1"
}

// shortSocket returns a socket path short enough for sockaddr_un.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "maestro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "svc.sock")
}

// attachFixture starts a confined session served on a Unix socket and
// a PTY pair standing in for the user's terminal.
func attachFixture(t *testing.T, argv ...string) (string, *session.Session, *os.File, *os.File) {
	t.Helper()
	live := confinedSession(t, argv...)
	socket := serve(t, &ipc.Server{Service: "maestro-svc", Version: "0.0.0", Sessions: oneSession{live}})
	keyboard, terminal := terminalPair(t)
	return socket, live, keyboard, terminal
}

// confinedSession starts a confined session running argv, skipping the
// hosts that cannot confine.
func confinedSession(t *testing.T, argv ...string) *session.Session {
	t.Helper()
	probed, err := launcher.New()
	if errors.Is(err, launcher.ErrUnsupported) {
		t.Skipf("host cannot confine: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	live, err := session.Start(probed, session.Config{Spec: launcher.Spec{Argv: argv, Dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Stop(200 * time.Millisecond) })
	return live
}

// serve serves the daemon's routes on a Unix socket and returns it.
func serve(t *testing.T, server *ipc.Server) string {
	t.Helper()
	socket := shortSocket(t)
	listener, err := transport.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = http.Serve(listener, server.Handler()) }()
	return socket
}

// terminalPair opens a PTY pair: the keyboard side types, the terminal
// side stands in for the user's terminal.
func terminalPair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	keyboard, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keyboard.Close(); _ = terminal.Close() })
	return keyboard, terminal
}

// modes captures the termios flags that raw mode changes.
func modes(t *testing.T, terminal *os.File) [3]uint32 {
	t.Helper()
	settings, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return [3]uint32{settings.Iflag, settings.Oflag, settings.Lflag}
}

// eventually polls until the output contains want.
func eventually(t *testing.T, output *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("output never contained %q: %q", want, output.String())
}

func TestRunRelaysAndDetachesOnCtrlBracket(t *testing.T) {
	socket, live, keyboard, terminal := attachFixture(t, "/bin/sh")
	before := modes(t, terminal)
	output := &lockedBuffer{}
	finished := make(chan error, 1)
	go func() {
		finished <- Run(context.Background(), socket, "s1", Terminal{In: terminal, Out: output})
	}()

	// Raw mode is on while attached.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && modes(t, terminal) == before {
		time.Sleep(10 * time.Millisecond)
	}
	if modes(t, terminal) == before {
		t.Fatal("the terminal never entered raw mode")
	}

	if _, err := keyboard.Write([]byte("echo attached-$((40+2))\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, output, "attached-42")

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
	if modes(t, terminal) != before {
		t.Fatal("the terminal was not restored after detach")
	}
	if state := live.State(); state.Phase != session.Running {
		t.Fatalf("detach ended the session: %+v", state)
	}
}

func TestRunPropagatesResizes(t *testing.T) {
	socket, _, keyboard, terminal := attachFixture(t, "/bin/sh")
	if err := pty.Setsize(terminal, &pty.Winsize{Rows: 33, Cols: 77}); err != nil {
		t.Fatal(err)
	}
	output := &lockedBuffer{}
	winch := make(chan os.Signal, 1)
	finished := make(chan error, 1)
	go func() {
		finished <- Run(context.Background(), socket, "s1", Terminal{In: terminal, Out: output, Winch: winch})
	}()

	if _, err := keyboard.Write([]byte("stty size\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, output, "33 77")

	if err := pty.Setsize(terminal, &pty.Winsize{Rows: 44, Cols: 88}); err != nil {
		t.Fatal(err)
	}
	winch <- syscall.SIGWINCH
	time.Sleep(100 * time.Millisecond)
	if _, err := keyboard.Write([]byte("stty size\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, output, "44 88")

	if _, err := keyboard.Write([]byte{DetachKey}); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestRunRestoresTheTerminalWhenTheSessionEnds(t *testing.T) {
	// The session prints only after a line typed through the attach, so
	// its output cannot be emitted before the attach subscribed to it.
	socket, _, keyboard, terminal := attachFixture(t, "/bin/sh", "-c", "read line; echo goodbye-$line; exit 3")
	before := modes(t, terminal)
	output := &lockedBuffer{}
	finished := make(chan error, 1)
	go func() {
		finished <- Run(context.Background(), socket, "s1", Terminal{In: terminal, Out: output})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && modes(t, terminal) == before {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := keyboard.Write([]byte("now\r")); err != nil {
		t.Fatal(err)
	}
	var err error
	select {
	case err = <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("attach did not return after the session ended")
	}
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("expected the session end to be reported, got %v", err)
	}
	if !strings.Contains(output.String(), "goodbye-now") {
		t.Fatalf("output %q", output.String())
	}
	if modes(t, terminal) != before {
		t.Fatal("the terminal was not restored after an error")
	}
}

func TestRunRefusesAnUnknownSessionWithoutTouchingTheTerminal(t *testing.T) {
	socket, _, _, terminal := attachFixture(t, "/bin/cat")
	before := modes(t, terminal)
	err := Run(context.Background(), socket, "missing", Terminal{In: terminal, Out: &lockedBuffer{}})
	if err == nil || !strings.Contains(err.Error(), "unknown session") {
		t.Fatalf("error %v", err)
	}
	if modes(t, terminal) != before {
		t.Fatal("a refused attach changed the terminal")
	}
}

func TestResizesDeliversWindowChangesUntilStopped(t *testing.T) {
	winch, stop := Resizes()
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-winch:
		if got != syscall.SIGWINCH {
			t.Fatalf("signal %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGWINCH was not delivered")
	}
}

func TestRunStopsReadingTheTerminalWhenItReturns(t *testing.T) {
	socket, _, keyboard, terminal := attachFixture(t, "/bin/sh", "-c", "read line; exit 0")
	before := modes(t, terminal)
	finished := make(chan error, 1)
	go func() {
		finished <- Run(context.Background(), socket, "s1", Terminal{In: terminal, Out: &lockedBuffer{}})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && modes(t, terminal) == before {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := keyboard.Write([]byte("now\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("attach did not return after the session ended")
	}

	// What is typed after the attach belongs to the next reader.
	if _, err := keyboard.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}
	fds := []unix.PollFd{{Fd: int32(terminal.Fd()), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, 5000); err != nil || n == 0 {
		t.Fatalf("the input typed after the attach was consumed: %d, %v", n, err)
	}
	line := make([]byte, 64)
	n, err := terminal.Read(line)
	if err != nil || !strings.Contains(string(line[:n]), "after") {
		t.Fatalf("read %q, %v", line[:n], err)
	}
}
