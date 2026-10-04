// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package fixture

import (
	"sync"
	"time"

	"github.com/goabonga/maestro/internal/session"
)

// Recorder captures one live session: its output from a subscription,
// and the input and resizes sent through the recorder.
type Recorder struct {
	live    *session.Session
	header  Header
	start   time.Time
	mu      sync.Mutex
	events  []Event
	drained chan struct{}
}

// Record starts capturing a live session. Output produced before the
// call is not part of the fixture.
func Record(live *session.Session, header Header) *Recorder {
	recorder := &Recorder{live: live, header: header, start: time.Now(), drained: make(chan struct{})}
	output, _ := live.Subscribe(4096)
	go func() {
		defer close(recorder.drained)
		for chunk := range output {
			recorder.add(Event{Kind: Output, Data: chunk})
		}
	}()
	return recorder
}

// add stamps and appends one event.
func (r *Recorder) add(event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	event.AtMillis = time.Since(r.start).Milliseconds()
	r.events = append(r.events, event)
}

// Input writes keystrokes to the session and records them.
func (r *Recorder) Input(data []byte) error {
	r.add(Event{Kind: Input, Data: append([]byte(nil), data...)})
	_, err := r.live.Write(data)
	return err
}

// Resize resizes the session and records it.
func (r *Recorder) Resize(rows, cols uint16) error {
	r.add(Event{Kind: Resize, Rows: rows, Cols: cols})
	return r.live.Resize(rows, cols)
}

// Finish waits for the session to end, records its exit and returns
// the raw fixture — still to be sanitized before it may be saved.
func (r *Recorder) Finish() Fixture {
	r.live.Wait()
	<-r.drained
	state := r.live.State()
	r.add(Event{Kind: Exit, ExitCode: state.ExitCode, Phase: state.Phase})
	r.mu.Lock()
	defer r.mu.Unlock()
	return Fixture{Header: r.header, Events: append([]Event(nil), r.events...)}
}
