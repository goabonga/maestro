// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/transport"
)

// fixedSessions resolves one session under one id.
type fixedSessions map[string]*session.Session

func (f fixedSessions) Session(id string) (*session.Session, bool) {
	live, ok := f[id]
	return live, ok
}

// confined returns a launcher or skips when the host cannot confine.
func confined(t *testing.T) *launcher.Launcher {
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

// streamServer serves one live session on a Unix socket and returns an
// upgraded raw connection to its stream.
func streamServer(t *testing.T, live *session.Session) net.Conn {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "svc.sock")
	listener, err := transport.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := &Server{Service: "maestro-svc", Version: "0.0.0", Sessions: fixedSessions{"s1": live}}
	go func() { _ = http.Serve(listener, server.Handler()) }()

	connection, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	request := "GET /v1/sessions/s1/stream HTTP/1.1\r\nHost: maestro\r\n" +
		"Upgrade: " + StreamProtocol + "\r\nConnection: Upgrade\r\n\r\n"
	if _, err := connection.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	// One reader for the whole connection: a second bufio.Reader
	// would lose whatever the first one buffered ahead.
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("no upgrade: %q err=%v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	return &bufferedConn{Conn: connection, reader: reader}
}

// bufferedConn keeps the bytes the header scan already buffered.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.reader.Read(p) }

// collectOutput reads frames until want appears in the output stream.
func collectOutput(t *testing.T, connection net.Conn, want string) {
	t.Helper()
	var output bytes.Buffer
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
		kind, payload, err := ReadFrame(connection)
		if err != nil {
			t.Fatalf("stream ended before %q: %v (got %q)", want, err, output.String())
		}
		if kind == FrameOutput {
			output.Write(payload)
			if strings.Contains(output.String(), want) {
				return
			}
		}
	}
	t.Fatalf("output never contained %q: %q", want, output.String())
}

func TestStreamCarriesOutputInputAndResize(t *testing.T) {
	live, err := session.Start(confined(t), session.Config{
		Spec: launcher.Spec{Argv: []string{"/bin/sh"}, Dir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Stop(200 * time.Millisecond) })
	connection := streamServer(t, live)

	if err := WriteFrame(connection, FrameInput, []byte("echo over-the-wire\r")); err != nil {
		t.Fatal(err)
	}
	collectOutput(t, connection, "over-the-wire")

	if err := WriteFrame(connection, FrameResize, ResizePayload(37, 91)); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(connection, FrameInput, []byte("stty size\r")); err != nil {
		t.Fatal(err)
	}
	collectOutput(t, connection, "37 91")
}

func TestDetachEndsTheStreamNotTheSession(t *testing.T) {
	live, err := session.Start(confined(t), session.Config{
		Spec: launcher.Spec{Argv: []string{"/bin/cat"}, Dir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Stop(200 * time.Millisecond) })
	connection := streamServer(t, live)

	if err := WriteFrame(connection, FrameDetach, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, _, err := ReadFrame(connection); err != nil {
			break
		}
	}
	if state := live.State(); state.Phase != session.Running {
		t.Fatalf("detach ended the session: %+v", state)
	}
}

func TestStreamReportsASessionEnd(t *testing.T) {
	live, err := session.Start(confined(t), session.Config{
		Spec: launcher.Spec{Argv: []string{"/bin/sh", "-c", "echo last-words; exit 0"}, Dir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := streamServer(t, live)
	deadline := time.Now().Add(10 * time.Second)
	var sawError string
	for time.Now().Before(deadline) && sawError == "" {
		_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
		kind, payload, err := ReadFrame(connection)
		if err != nil {
			break
		}
		if kind == FrameError {
			sawError = string(payload)
		}
	}
	if !strings.Contains(sawError, "exited") {
		t.Fatalf("no end-of-session diagnostic, got %q", sawError)
	}
}

func TestStreamRejectsUnknownSessionsAndMissingUpgrade(t *testing.T) {
	_, web := newServer(t)
	status, envelope, _ := call(t, web, "GET", "/v1/sessions/s1/stream", nil, "")
	if status != http.StatusNotFound || envelope.Error == nil {
		t.Fatalf("status=%d envelope=%+v", status, envelope)
	}
}

func TestFrameProtocolBounds(t *testing.T) {
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, FrameOutput, bytes.Repeat([]byte("x"), MaxFramePayload+1)); err == nil {
		t.Fatal("an oversized frame was written")
	}
	if err := WriteFrame(&buffer, FrameResize, ResizePayload(24, 80)); err != nil {
		t.Fatal(err)
	}
	kind, payload, err := ReadFrame(&buffer)
	if err != nil || kind != FrameResize {
		t.Fatalf("kind=%c err=%v", kind, err)
	}
	rows, cols, err := ParseResize(payload)
	if err != nil || rows != 24 || cols != 80 {
		t.Fatalf("rows=%d cols=%d err=%v", rows, cols, err)
	}
	if _, _, err := ReadFrame(bytes.NewReader([]byte{'z', 0, 0, 0, 0})); err == nil {
		t.Fatal("an unknown frame type was accepted")
	}
	oversize := []byte{byte(FrameOutput), 0xff, 0xff, 0xff, 0xff}
	if _, _, err := ReadFrame(bytes.NewReader(oversize)); err == nil {
		t.Fatal("an oversized frame length was accepted")
	}
	if _, _, err := ParseResize([]byte{1}); err == nil {
		t.Fatal("a short resize payload was accepted")
	}

}

func TestAStalledClientIsHungUpOn(t *testing.T) {
	live, err := session.Start(confined(t), session.Config{
		Spec: launcher.Spec{Argv: []string{"/bin/sh"}, Dir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Stop(200 * time.Millisecond) })
	connection := streamServer(t, live)

	// Trigger the flood only once the stream is attached, then stop
	// reading: the server's write stalls, its deadline fires and it
	// hangs up; the session itself keeps running.
	if err := WriteFrame(connection, FrameInput, []byte("seq 1 2000000\r")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(8 * time.Second)
	buffer := make([]byte, 64<<10)
	drained := 0
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := connection.Read(buffer)
		drained += n
		if err != nil {
			if timeoutError(err) {
				t.Fatalf("the connection is still open after draining %d bytes", drained)
			}
			// EOF or a reset: the server hung up on the slow client.
			if state := live.State(); state.Phase != session.Running {
				t.Fatalf("the session did not survive the hang-up: %+v", state)
			}
			return
		}
	}
	t.Fatalf("the server kept streaming %d bytes to a stalled client", drained)
}

// timeoutError reports a deadline-style error.
func timeoutError(err error) bool {
	type timeout interface{ Timeout() bool }
	te, ok := err.(timeout)
	return ok && te.Timeout()
}
