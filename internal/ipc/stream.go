// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"bufio"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/goabonga/maestro/internal/session"
)

// StreamProtocol names the upgrade negotiated for PTY streaming.
const StreamProtocol = "maestro-stream/1"

// SessionSource resolves a session by identifier.
type SessionSource interface {
	Session(id string) (*session.Session, bool)
}

// streamSession upgrades the connection and pumps frames: output to
// the client from a bounded live subscription, input and resizes from
// the client to the terminal. A slow client is disconnected with an
// error frame; a detach ends only the stream, never the session.
func (s *Server) streamSession(w http.ResponseWriter, r *http.Request) {
	if s.Sessions == nil {
		fail(w, r, http.StatusNotFound, CodeNotFound, "this daemon exposes no sessions")
		return
	}
	live, ok := s.Sessions.Session(r.PathValue("id"))
	if !ok {
		fail(w, r, http.StatusNotFound, CodeNotFound, "unknown session: "+r.PathValue("id"))
		return
	}
	if r.Header.Get("Upgrade") != StreamProtocol {
		fail(w, r, http.StatusUpgradeRequired, CodeInvalidRequest, "set Upgrade: "+StreamProtocol)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		fail(w, r, http.StatusInternalServerError, CodeInternal, "the connection cannot be upgraded")
		return
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	defer func() { _ = connection.Close() }()
	if _, err := buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " +
		StreamProtocol + "\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	pump(connection, buffered.Reader, live)
}

// writeDeadline bounds how long one frame write may stall: a client
// that stops reading is disconnected instead of holding the pump.
const writeDeadline = 5 * time.Second

// pump moves frames between one client connection and one session.
func pump(connection net.Conn, reader *bufio.Reader, live *session.Session) {
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
			return
		default:
			_ = write(FrameError, []byte("unexpected frame from the client"))
		}
	}
	close(leaving)
	cancel()
	<-finished
}
