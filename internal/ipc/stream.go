// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/worker"
)

// StreamProtocol names the upgrade negotiated for PTY streaming.
const StreamProtocol = "maestro-stream/1"

// Pilots hands the live session of a worker to one human pilot at a
// time; *worker.Supervisor is one.
type Pilots interface {
	// Attach takes exclusive control of a worker's live session and
	// returns its terminal with the function that ends the attach,
	// called once with whether the client detached or lost its
	// connection.
	Attach(projectID, name string) (*session.Session, func(detached bool), error)
}

// streamWorker attaches the client as the human pilot of a worker of
// the project named by ?project_id=, then upgrades the connection and
// pumps frames: output to the client from a bounded live subscription,
// input and resizes from the client to the terminal. A slow client is
// disconnected with an error frame; a detach ends only the stream, never
// the session. The worker is ATTACHED before the connection is
// upgraded, so a refused attach answers with the error envelope; once
// the stream ends, the worker is handed back on a detach frame, or on a
// lost connection for any other end.
func (s *Server) streamWorker(w http.ResponseWriter, r *http.Request) {
	if s.Pilots == nil {
		fail(w, r, http.StatusNotFound, CodeNotFound, "this daemon attaches no worker")
		return
	}
	project, ok := s.workerProject(w, r, r.URL.Query().Get("project_id"))
	if !ok {
		return
	}
	if !upgradable(w, r) {
		return
	}
	name := r.PathValue("name")
	live, end, err := s.Pilots.Attach(project.ID, name)
	switch {
	case errors.Is(err, worker.ErrNotFound):
		fail(w, r, http.StatusNotFound, CodeNotFound, fmt.Sprintf("unknown worker in project %s: %s", project.ID, name))
		return
	case errors.Is(err, worker.ErrTransition), errors.Is(err, worker.ErrGuard):
		fail(w, r, http.StatusConflict, CodeConflict, err.Error())
		return
	case err != nil:
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	detached := false
	defer func() { end(detached) }()
	connection, buffered, ok := upgrade(w, r)
	if !ok {
		return
	}
	defer func() { _ = connection.Close() }()
	detached = pump(connection, buffered.Reader, live)
}

// upgradable checks that the request asks for the stream protocol on a
// connection that can be taken over, writing the error envelope when it
// does not.
func upgradable(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Upgrade") != StreamProtocol {
		fail(w, r, http.StatusUpgradeRequired, CodeInvalidRequest, "set Upgrade: "+StreamProtocol)
		return false
	}
	if _, ok := w.(http.Hijacker); !ok {
		fail(w, r, http.StatusInternalServerError, CodeInternal, "the connection cannot be upgraded")
		return false
	}
	return true
}

// upgrade takes the connection over and switches it to the stream
// protocol.
func upgrade(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.ReadWriter, bool) {
	if !upgradable(w, r) {
		return nil, nil, false
	}
	connection, buffered, err := w.(http.Hijacker).Hijack()
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return nil, nil, false
	}
	if _, err := buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " +
		StreamProtocol + "\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		_ = connection.Close()
		return nil, nil, false
	}
	if err := buffered.Flush(); err != nil {
		_ = connection.Close()
		return nil, nil, false
	}
	return connection, buffered, true
}

// writeDeadline bounds how long one frame write may stall: a client
// that stops reading is disconnected instead of holding the pump.
const writeDeadline = 5 * time.Second

// pump moves frames between one client connection and one session. It
// reports whether the client ended the stream with a detach frame.
func pump(connection net.Conn, reader *bufio.Reader, live *session.Session) bool {
	output, cancel := live.Subscribe(256)
	defer cancel()

	var writeMu sync.Mutex
	write := func(kind FrameType, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = connection.SetWriteDeadline(time.Now().Add(writeDeadline))
		return WriteFrame(connection, kind, payload)
	}

	// leaving is closed when the pump itself ends the subscription — a
	// detach or a client hang-up — so its closure is not mistaken for a
	// slow client.
	leaving := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for chunk := range output {
			for len(chunk) > 0 {
				piece := chunk
				if len(piece) > MaxFramePayload {
					piece = piece[:MaxFramePayload]
				}
				if err := write(FrameOutput, piece); err != nil {
					// A stalled write is a slow client: hang up so
					// neither the pump nor the session waits on it.
					_ = connection.Close()
					return
				}
				chunk = chunk[len(piece):]
			}
		}
		select {
		case <-leaving:
			_ = connection.Close()
			return
		default:
		}
		// The subscription closed: the session ended, or this client
		// was too slow and lost its queue. Say which, then hang up.
		state := live.State()
		if state.Phase == session.Running {
			_ = write(FrameError, []byte("disconnected: the client read too slowly"))
		} else {
			_ = write(FrameError, []byte("the session is "+state.Phase))
		}
		_ = connection.Close()
	}()

	for {
		kind, payload, err := ReadFrame(reader)
		if err != nil {
			break
		}
		switch kind {
		case FrameInput:
			if _, err := live.Write(payload); err != nil {
				_ = write(FrameError, []byte(err.Error()))
			}
		case FrameResize:
			rows, cols, err := ParseResize(payload)
			if err == nil {
				err = live.Resize(rows, cols)
			}
			if err != nil {
				_ = write(FrameError, []byte(err.Error()))
			}
		case FrameDetach:
			close(leaving)
			cancel()
			<-finished
			return true
		default:
			_ = write(FrameError, []byte("unexpected frame from the client"))
		}
	}
	close(leaving)
	cancel()
	<-finished
	return false
}
