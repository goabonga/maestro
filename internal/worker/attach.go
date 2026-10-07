// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"fmt"

	"github.com/goabonga/maestro/internal/session"
)

// Attach hands the live session of a started worker to one human pilot.
// The worker moves to ATTACHED from IDLE or WAITING_INPUT, whose turn is
// quiescent; a BUSY worker's turn is running, so its attach is refused
// with ErrGuard, and a worker already ATTACHED, or in any other state,
// is refused with ErrTransition, as is a worker without a live session
// here. The transition is applied to the worker as it was read, so an
// assignment taken concurrently wins over the attach. While the worker
// is ATTACHED the engine assigns it nothing and writes nothing to its
// terminal.
//
// Attach returns the session's terminal and the function that ends the
// attach, to call exactly once: detached reports a detach asked by the
// client, false a lost connection. The worker is then reconciled by its
// continuation — IDLE without an assignment, WAITING_INPUT with one —
// or FAILED once its session is ending or gone.
func (s *Supervisor) Attach(projectID, name string) (*session.Session, func(detached bool), error) {
	s.init()
	key := liveKey{projectID, name}
	current, err := s.Store.Get(projectID, name)
	if err != nil {
		return nil, nil, err
	}
	live, ok := s.attachable(key)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s has no live session", ErrTransition, name)
	}
	if _, err := s.Store.transitionFrom(current, Input{
		Event: Attach, Reason: "a human pilot attached",
		Guard: Guard{TurnQuiescent: current.State == Idle || current.State == WaitingInput},
	}); err != nil {
		return nil, nil, err
	}
	live.turn.Lock()
	terminal := live.epoch.Session()
	live.turn.Unlock()
	end := func(detached bool) {
		event, reason := Detach, "the human pilot detached"
		if !detached {
			event, reason = ConnectionLost, "the human pilot's connection was lost"
		}
		_, still := s.attachable(key)
		still = still && terminal.State().Phase == session.Running
		_, _ = s.Store.Transition(projectID, name, Input{
			Event: event, Reason: reason, Guard: Guard{Reconciled: still, ContinuationValid: still},
		})
	}
	return terminal, end, nil
}

// attachable returns the live session of a worker that no stop, failure
// or shutdown is taking down.
func (s *Supervisor) attachable(key liveKey) (*running, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.live[key]
	if live == nil || live.stopping || live.ended || s.closed {
		return nil, false
	}
	return live, true
}
