// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package session

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/launcher"
)

// sandbox returns a probed launcher or skips when the host cannot
// confine.
func sandbox(t *testing.T) *launcher.Launcher {
	t.Helper()
	probed, err := launcher.New()
	if errors.Is(err, launcher.ErrUnsupported) {
		t.Skipf("host cannot confine: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return probed
}

// start runs one fixture command in a confined session.
func start(t *testing.T, config Config) *Session {
	t.Helper()
	session, err := Start(sandbox(t), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Stop(200 * time.Millisecond) })
	return session
}

// waitOutput polls until the kept output satisfies the predicate.
func waitOutput(t *testing.T, session *Session, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var kept []byte
	for time.Now().Before(deadline) {
		kept, _ = session.Output()
		if strings.Contains(string(kept), want) {
			return string(kept)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("output never contained %q: %q", want, kept)
	return ""
}

func TestSessionEchoesThroughTheTerminal(t *testing.T) {
	session := start(t, Config{Spec: launcher.Spec{Argv: []string{"/bin/cat"}, Dir: t.TempDir()}})
	if _, err := session.Write([]byte("hello terminal\r")); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, session, "hello terminal")
	if state := session.State(); state.Phase != Running {
		t.Fatalf("state %+v", state)
	}
}

func TestOutputIsBounded(t *testing.T) {
	session := start(t, Config{
		Spec:        launcher.Spec{Argv: []string{"/bin/sh", "-c", "seq 1 5000; echo FINISHED"}, Dir: t.TempDir()},
		BufferBytes: 1024,
	})
	waitOutput(t, session, "FINISHED")
	kept, total := session.Output()
	if len(kept) > 1024 {
		t.Fatalf("kept %d bytes for a 1024 bound", len(kept))
	}
	if total <= 1024 {
		t.Fatalf("total %d should exceed the bound", total)
	}
	if !bytes.Contains(kept, []byte("FINISHED")) {
		t.Fatal("the newest output was dropped")
	}
	if bytes.Contains(kept, []byte("\n1\r\n2\r\n")) {
		t.Fatal("the oldest output survived a full buffer")
	}
}

func TestResizeReachesTheTerminal(t *testing.T) {
	session := start(t, Config{
		Spec: launcher.Spec{Argv: []string{"/bin/sh"}, Dir: t.TempDir()},
		Rows: 24, Cols: 80,
	})
	if err := session.Resize(41, 101); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Write([]byte("stty size\r")); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, session, "41 101")
}

func TestExitIsReconciledIntoTheState(t *testing.T) {
	session := start(t, Config{Spec: launcher.Spec{Argv: []string{"/bin/sh", "-c", "exit 7"}, Dir: t.TempDir()}})
	session.Wait()
	state := session.State()
	if state.Phase != Exited || state.ExitCode != 7 {
		t.Fatalf("state %+v", state)
	}
	if _, err := session.Write([]byte("late\r")); err == nil {
		t.Fatal("write to an exited session succeeded")
	}
	if err := session.Resize(10, 10); err == nil {
		t.Fatal("resize of an exited session succeeded")
	}
	// Stopping a finished session is a no-op.
	if err := session.Stop(50 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestStopTerminatesEveryDescendant(t *testing.T) {
	marker := fmt.Sprintf("maestro-session-%d", time.Now().UnixNano())
	session := start(t, Config{Spec: launcher.Spec{
		Argv: []string{"/bin/sh", "-c",
			"/bin/sh -c 'sleep 1000' " + marker + "-bg & exec /bin/sh -c 'sleep 1001' " + marker + "-fg"},
		Dir: t.TempDir(),
	}})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("pgrep", "-f", marker).Run() == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := session.Stop(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if state := session.State(); state.Phase != Stopped {
		t.Fatalf("state %+v", state)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("pgrep", "-f", marker).Run() != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a descendant survived the stop")
}

func TestRingKeepsOnlyTheNewestBytes(t *testing.T) {
	ring := newRing(8)
	if _, err := ring.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Write([]byte("GHIJ")); err != nil {
		t.Fatal(err)
	}
	kept, total := ring.Snapshot()
	if string(kept) != "cdefGHIJ" || total != 10 {
		t.Fatalf("kept=%q total=%d", kept, total)
	}
	if _, err := ring.Write(bytes.Repeat([]byte("x"), 20)); err != nil {
		t.Fatal(err)
	}
	kept, total = ring.Snapshot()
	if string(kept) != "xxxxxxxx" || total != 30 {
		t.Fatalf("kept=%q total=%d", kept, total)
	}
}
