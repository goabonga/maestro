// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package attach is the client side of a session's stream: it puts the
// user's terminal in raw mode, relays input, output and resizes to a
// session served by the daemon, such as the live session of a worker it
// pilots, and leaves on Ctrl-].
package attach

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goabonga/maestro/internal/ipc"
)

// DetachKey is Ctrl-]: it leaves the session without touching it.
const DetachKey = 0x1d

// Terminal is the user's terminal for one attach.
type Terminal struct {
	// In is the terminal the keystrokes are read from; it is switched to
	// raw mode for the attach.
	In *os.File
	// Out receives the session's output.
	Out io.Writer
	// Winch delivers terminal resizes; nil disables propagation.
	Winch <-chan os.Signal
}

// Resizes delivers the resizes of the process's terminal until the
// returned function is called.
func Resizes() (<-chan os.Signal, func()) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	return winch, func() { signal.Stop(winch) }
}

// Run connects to a session's stream on the daemon's socket, puts the
// terminal in raw mode, relays input, output and resizes, and leaves on
// Ctrl-]. The terminal is restored by a deferred call on every path,
// errors included.
func Run(ctx context.Context, socket, session string, term Terminal) error {
	return run(ctx, socket, "/v1/sessions/"+url.PathEscape(session)+"/stream", "session "+session, term)
}

// Worker attaches the terminal as the human pilot of a worker of a
// project, as Run does for a session: the daemon moves the worker to
// ATTACHED before the stream opens, and hands it back on Ctrl-] or when
// the connection ends. A refused attach, such as a worker whose turn is
// running, returns the daemon's reason without touching the terminal.
func Worker(ctx context.Context, socket, projectID, name string, term Terminal) error {
	path := "/v1/workers/" + url.PathEscape(name) + "/stream?project_id=" + url.QueryEscape(projectID)
	return run(ctx, socket, path, "worker "+name, term)
}

// run attaches the terminal to the stream at path; label names the
// stream's end in the errors.
func run(ctx context.Context, socket, path, label string, term Terminal) (err error) {
	connection, reader, err := openStream(ctx, socket, path)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()

	restore, err := rawTerminal(int(term.In.Fd()))
	if err != nil {
		return err
	}
	defer func() {
		if restoreErr := restore(); restoreErr != nil && err == nil {
			err = restoreErr
		}
	}()

	sendResize := func() {
		if rows, cols, ok := terminalSize(int(term.In.Fd())); ok {
			_ = ipc.WriteFrame(connection, ipc.FrameResize, ipc.ResizePayload(rows, cols))
		}
	}
	sendResize()

	detached := make(chan struct{})
	done := make(chan struct{})
	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		relayInput(term.In, connection, detached, done)
	}()
	// The relay stops before the terminal is restored, so nothing reads
	// the terminal once Run returns.
	defer func() {
		close(done)
		<-relayed
	}()
	if term.Winch != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case <-term.Winch:
					sendResize()
				case <-stop:
					return
				}
			}
		}()
	}

	for {
		kind, payload, err := ipc.ReadFrame(reader)
		if err != nil {
			select {
			case <-detached:
				return nil
			default:
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		switch kind {
		case ipc.FrameOutput:
			if _, err := term.Out.Write(payload); err != nil {
				return err
			}
		case ipc.FrameError:
			select {
			case <-detached:
				return nil
			default:
			}
			return fmt.Errorf("%s: %s", label, payload)
		}
	}
}

// relayInput forwards keystrokes as input frames until Ctrl-], which
// sends a detach frame instead, or until done is closed.
func relayInput(in *os.File, connection net.Conn, detached chan<- struct{}, done <-chan struct{}) {
	fd := int(in.Fd())
	buffer := make([]byte, 4096)
	for {
		if ready, err := waitReadable(fd, done); err != nil || !ready {
			return
		}
		n, err := in.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if index := bytes.IndexByte(chunk, DetachKey); index >= 0 {
				if index > 0 {
					_ = ipc.WriteFrame(connection, ipc.FrameInput, chunk[:index])
				}
				close(detached)
				_ = ipc.WriteFrame(connection, ipc.FrameDetach, nil)
				return
			}
			if err := ipc.WriteFrame(connection, ipc.FrameInput, chunk); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// openStream dials the daemon and upgrades to the stream at path. A
// refusal returns the message of the daemon's error envelope.
func openStream(ctx context.Context, socket, path string) (net.Conn, *bufio.Reader, error) {
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, nil, fmt.Errorf("daemon not reachable at %s: %w", socket, err)
	}
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	request := "GET " + path + " HTTP/1.1\r\nHost: maestro\r\n" +
		"Upgrade: " + ipc.StreamProtocol + "\r\nConnection: Upgrade\r\n\r\n"
	if _, err := connection.Write([]byte(request)); err != nil {
		_ = connection.Close()
		return nil, nil, err
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		_ = connection.Close()
		return nil, nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		defer func() { _ = connection.Close() }()
		var envelope ipc.Envelope
		if json.NewDecoder(io.LimitReader(response.Body, maxRefusal)).Decode(&envelope) == nil &&
			envelope.Error != nil && envelope.Error.Message != "" {
			return nil, nil, errors.New(envelope.Error.Message)
		}
		return nil, nil, fmt.Errorf("the daemon refused the stream: %s", response.Status)
	}
	_ = connection.SetDeadline(time.Time{})
	return connection, reader, nil
}

// maxRefusal bounds the error envelope read from a refused stream.
const maxRefusal = 64 << 10
