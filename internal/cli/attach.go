// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/transport"
)

// detachKey is Ctrl-]: it leaves the session without touching it.
const detachKey = 0x1d

// attachIO is the user's terminal for one attach.
type attachIO struct {
	in  *os.File
	out io.Writer
	// winch delivers terminal resizes; nil disables propagation.
	winch <-chan os.Signal
}

// attachCommand parses `maestro attach <session>` and attaches the
// process's own terminal.
func attachCommand(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("maestro attach", flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", transport.DefaultSocket(), "daemon Unix socket path")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// Accept flags after the session too.
	if flags.NArg() == 0 {
		return errors.New("usage: maestro attach <session> [--socket <path>]")
	}
	target := flags.Arg(0)
	if err := flags.Parse(flags.Args()[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: maestro attach <session> [--socket <path>]")
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	return attach(ctx, *socket, target, attachIO{in: os.Stdin, out: output, winch: winch})
}

// attach connects to a session's stream, puts the terminal in raw
// mode, relays input, output and resizes, and leaves on Ctrl-]. The
// terminal is restored by a deferred call on every path, errors
// included.
func attach(ctx context.Context, socket, session string, term attachIO) (err error) {
	connection, reader, err := openStream(ctx, socket, session)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()

	restore, err := rawTerminal(int(term.in.Fd()))
	if err != nil {
		return err
	}
	defer func() {
		if restoreErr := restore(); restoreErr != nil && err == nil {
			err = restoreErr
		}
	}()

	sendResize := func() {
		if rows, cols, ok := terminalSize(int(term.in.Fd())); ok {
			_ = ipc.WriteFrame(connection, ipc.FrameResize, ipc.ResizePayload(rows, cols))
		}
	}
	sendResize()

	detached := make(chan struct{})
	go relayInput(term.in, connection, detached)
	if term.winch != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case <-term.winch:
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
			if _, err := term.out.Write(payload); err != nil {
				return err
			}
		case ipc.FrameError:
			select {
			case <-detached:
				return nil
			default:
			}
			return fmt.Errorf("session %s: %s", session, payload)
		}
	}
}

// relayInput forwards keystrokes as input frames until Ctrl-], which
// sends a detach frame instead.
func relayInput(in io.Reader, connection net.Conn, detached chan<- struct{}) {
	buffer := make([]byte, 4096)
	for {
		n, err := in.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if index := bytes.IndexByte(chunk, detachKey); index >= 0 {
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

// openStream dials the daemon and upgrades to the session's stream.
func openStream(ctx context.Context, socket, session string) (net.Conn, *bufio.Reader, error) {
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, nil, fmt.Errorf("daemon not reachable at %s: %w", socket, err)
	}
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	request := "GET /v1/sessions/" + url.PathEscape(session) + "/stream HTTP/1.1\r\nHost: maestro\r\n" +
		"Upgrade: " + ipc.StreamProtocol + "\r\nConnection: Upgrade\r\n\r\n"
	if _, err := connection.Write([]byte(request)); err != nil {
		_ = connection.Close()
		return nil, nil, err
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil {
		_ = connection.Close()
		return nil, nil, err
	}
	if !strings.Contains(status, " 101 ") {
		_ = connection.Close()
		if strings.Contains(status, " 404 ") {
			return nil, nil, fmt.Errorf("unknown session: %s", session)
		}
		return nil, nil, fmt.Errorf("the daemon refused the stream: %s", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = connection.Close()
			return nil, nil, err
		}
		if line == "\r\n" {
			break
		}
	}
	_ = connection.SetDeadline(time.Time{})
	return connection, reader, nil
}
