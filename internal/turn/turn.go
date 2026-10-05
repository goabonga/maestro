// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package turn drives the lifecycle of an agent turn: an explicit
// state machine, persisted one transition per transaction with its
// audit event, retries as new linked turns, and timeouts frozen when
// the turn is created and evaluated against an injected clock.
package turn

import (
	"errors"
	"fmt"
	"time"

	"github.com/goabonga/maestro/internal/config"
)

// State is the lifecycle state of a turn.
type State string

// The states of a turn. SUCCEEDED, INTERRUPTED and FAILED are terminal.
const (
	Prepared     State = "PREPARED"
	Running      State = "RUNNING"
	WaitingInput State = "WAITING_INPUT"
	Validating   State = "VALIDATING"
	Succeeded    State = "SUCCEEDED"
	Interrupted  State = "INTERRUPTED"
	Failed       State = "FAILED"
)

// Errors of the turn lifecycle.
var (
	// ErrTransition reports a transition the table does not list.
	ErrTransition = errors.New("transition refused")
	// ErrNotTerminal reports a retry of a turn that has not ended.
	ErrNotTerminal = errors.New("turn is not terminal")
	// ErrRetried reports a turn that already has a retry.
	ErrRetried = errors.New("turn already retried")
	// ErrNotFound reports an unknown turn.
	ErrNotFound = errors.New("unknown turn")
)

// transitions is the complete table of allowed transitions. Anything
// it does not list is refused.
var transitions = map[State][]State{
	Prepared:     {Running, Interrupted, Failed},
	Running:      {WaitingInput, Validating, Interrupted, Failed},
	WaitingInput: {Running, Interrupted, Failed},
	Validating:   {Succeeded, Interrupted, Failed},
}

// Terminal reports whether no transition leaves the state.
func (s State) Terminal() bool {
	return s == Succeeded || s == Interrupted || s == Failed
}

// Allowed reports whether the table lists the transition from -> to.
func Allowed(from, to State) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// check refuses a transition the table does not list.
func check(from, to State) error {
	if !Allowed(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrTransition, from, to)
	}
	return nil
}

// Timeouts are the bounds of one turn, frozen when it is created.
type Timeouts struct {
	// Turn bounds the whole turn from its admission, input waits
	// included.
	Turn time.Duration
	// InputWait bounds each wait for input.
	InputWait time.Duration
}

// TimeoutsFor resolves the bounds of a turn of the named agent: the
// project budgets, with the agent's turn_timeout when it sets one.
func TimeoutsFor(cfg config.Config, agent string) Timeouts {
	timeouts := Timeouts{Turn: cfg.Budgets.TurnTimeout.Duration, InputWait: cfg.Budgets.InputWaitTimeout.Duration}
	if override := cfg.Agents[agent].TurnTimeout; override != nil {
		timeouts.Turn = override.Duration
	}
	return timeouts
}

// Turn is one attempt of an agent at a task step.
type Turn struct {
	ID        string
	AttemptID string
	TaskID    string
	Agent     string
	ConfigID  string
	// PreviousID is the turn this one retries, empty for a first turn.
	PreviousID string
	State      State
	Timeouts   Timeouts
	CreatedAt  time.Time
	// AdmittedAt is when the turn first entered RUNNING; zero before.
	AdmittedAt time.Time
	// WaitingSince is when the current input wait began; zero outside
	// WAITING_INPUT.
	WaitingSince time.Time
	UpdatedAt    time.Time
	// Reason explains the last transition.
	Reason string
}

// Expiry names the bound a turn has exceeded.
type Expiry string

// The bounds a turn can exceed.
const (
	TurnTimeout      Expiry = "turn timeout"
	InputWaitTimeout Expiry = "input wait timeout"
)

// Expired evaluates the turn's bounds at now. The turn timeout counts
// from admission, whatever non-terminal state the turn is in; an input
// wait is also bounded on its own. When both have passed, the one that
// passed first is reported. A turn not yet admitted or already ended
// never expires.
func (t Turn) Expired(now time.Time) (Expiry, bool) {
	if t.State.Terminal() || t.AdmittedAt.IsZero() {
		return "", false
	}
	turnDeadline := t.AdmittedAt.Add(t.Timeouts.Turn)
	if t.State == WaitingInput && !t.WaitingSince.IsZero() {
		waitDeadline := t.WaitingSince.Add(t.Timeouts.InputWait)
		if !now.Before(waitDeadline) && waitDeadline.Before(turnDeadline) {
			return InputWaitTimeout, true
		}
	}
	if !now.Before(turnDeadline) {
		return TurnTimeout, true
	}
	return "", false
}

// Deadline returns the next instant at which the turn expires, if it
// can expire at all.
func (t Turn) Deadline() (time.Time, bool) {
	if t.State.Terminal() || t.AdmittedAt.IsZero() {
		return time.Time{}, false
	}
	deadline := t.AdmittedAt.Add(t.Timeouts.Turn)
	if t.State == WaitingInput && !t.WaitingSince.IsZero() {
		if wait := t.WaitingSince.Add(t.Timeouts.InputWait); wait.Before(deadline) {
			deadline = wait
		}
	}
	return deadline, true
}
