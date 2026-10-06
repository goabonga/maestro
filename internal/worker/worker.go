// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package worker holds the registry of workers: their durable identity,
// their lifecycle state and their current assignment. An explicit
// transition table is the single authority on which event moves a
// worker from which state to which; guards are fed by explicit facts
// the caller asserts. The store persists each transition with its event
// in one compare-and-set transaction, and the supervisor starts and
// stops the workers' confined agent sessions.
package worker

import (
	"errors"
	"fmt"
	"time"
)

// State is the lifecycle state of a worker.
type State string

// The states of a worker.
const (
	Stopped      State = "STOPPED"
	Starting     State = "STARTING"
	Idle         State = "IDLE"
	Busy         State = "BUSY"
	WaitingInput State = "WAITING_INPUT"
	Attached     State = "ATTACHED"
	Paused       State = "PAUSED"
	Draining     State = "DRAINING"
	Failed       State = "FAILED"
)

// States lists every state of a worker.
var States = []State{Stopped, Starting, Idle, Busy, WaitingInput, Attached, Paused, Draining, Failed}

// Active reports whether the worker is in an active state: any state
// but STOPPED and FAILED.
func (s State) Active() bool {
	return s != Stopped && s != Failed && contains(States, s)
}

// Event is what happens to a worker: each row of the transition table
// is one event accepted in a set of states.
type Event string

// The events of the transition table.
const (
	// Start: a start is asked and a session slot is reserved.
	Start Event = "start"
	// Ready: the session is ready and its profile confirmed.
	Ready Event = "ready"
	// Assign: an assignment and its turn are persisted.
	Assign Event = "assign"
	// AcceptTurn: the assigned turn is accepted.
	AcceptTurn Event = "accept-turn"
	// RequestInput: the agent asks for an input.
	RequestInput Event = "request-input"
	// Attach: a human takes exclusive control of the session.
	Attach Event = "attach"
	// Detach: the human client detaches.
	Detach Event = "detach"
	// ConnectionLost: the human client's connection is lost.
	ConnectionLost Event = "connection-lost"
	// Pause: the worker is paused.
	Pause Event = "pause"
	// Resume: a paused worker resumes its session.
	Resume Event = "resume"
	// ScaleDown: the worker is drained out of the pool.
	ScaleDown Event = "scale-down"
	// Drained: a draining worker has nothing left running.
	Drained Event = "drained"
	// Stop: an explicit stop.
	Stop Event = "stop"
	// Fail: an error or an inconsistency.
	Fail Event = "fail"
	// Recover: a failed worker is recovered to STOPPED.
	Recover Event = "recover"
)

// Errors of the worker lifecycle.
var (
	// ErrTransition reports an event the table does not accept in the
	// worker's state, or a transition lost to a concurrent one.
	ErrTransition = errors.New("transition refused")
	// ErrGuard reports an accepted event whose guard does not hold.
	ErrGuard = errors.New("guard not satisfied")
	// ErrNotFound reports an unknown worker.
	ErrNotFound = errors.New("unknown worker")
	// ErrExists reports a worker name already registered in the project.
	ErrExists = errors.New("worker already registered")
	// ErrInvalid reports an invalid worker registration or assignment.
	ErrInvalid = errors.New("invalid worker")
	// ErrAssigned reports a turn already assigned to another worker.
	ErrAssigned = errors.New("turn already assigned")
)

// Role is the role of a worker in an assignment.
type Role string

// The roles of an assignment.
const (
	Planning       Role = "planning"
	Implementation Role = "implementation"
	Review         Role = "review"
	Correction     Role = "correction"
)

// Roles lists every role of an assignment.
var Roles = []Role{Planning, Implementation, Review, Correction}

// Assignment links a worker to one turn of a task for a role. A worker
// holds at most one.
type Assignment struct {
	TaskID string
	Role   Role
	TurnID string
}

// Guard carries the facts the guards of the table depend on. The
// components that establish them (capacity, sessions, turns, process
// supervision, attach) report them here; a fact left false does not
// hold.
type Guard struct {
	// CapacityReserved: a session slot is reserved for the start.
	CapacityReserved bool
	// SessionReady: the session is ready.
	SessionReady bool
	// ProfileConfirmed: the session runs under its confirmed profile.
	ProfileConfirmed bool
	// AssignmentPersisted: the assignment and its turn are persisted.
	AssignmentPersisted bool
	// RightsRevoked: the turn's rights are revoked.
	RightsRevoked bool
	// Reconciled: the effects of the turn or of the stopped work are
	// reconciled.
	Reconciled bool
	// InputRecognized: the input request is recognized.
	InputRecognized bool
	// TurnQuiescent: the turn is quiescent.
	TurnQuiescent bool
	// InterruptConfirmed: the interruption of the turn is confirmed.
	InterruptConfirmed bool
	// DescendantsStopped: the worker's descendant processes are stopped
	// or absent.
	DescendantsStopped bool
	// ContinuationValid: the continuation of the worker's session (and
	// of its assignment, when it holds one) is validated.
	ContinuationValid bool
	// ProfileValidated: the profile to resume under is validated.
	ProfileValidated bool
	// TurnSettled: the turn is finished or explicitly cancelled.
	TurnSettled bool
	// ClientDetached: no human client is attached.
	ClientDetached bool
	// CauseLifted: the cause of the failure is lifted.
	CauseLifted bool
}

// Input is one event submitted to a worker.
type Input struct {
	Event Event
	// Assignment is the assignment an Assign event takes.
	Assignment Assignment
	// Reason explains the event; a failure requires it.
	Reason string
	Guard  Guard
}

// Worker is one registered work slot of a project.
type Worker struct {
	// ProjectID is the registered project the worker belongs to.
	ProjectID string
	// Name identifies the worker in its project.
	Name string
	// Agent is the configured agent the worker runs.
	Agent string
	// AgentKind is the kind of that agent.
	AgentKind string
	// Driver is the versioned driver of the agent.
	Driver string
	// Repository is the worker's private repository.
	Repository string
	State      State
	// Version counts the transitions applied to the worker.
	Version int64
	// Assignment is the current assignment; nil when the worker holds
	// none.
	Assignment *Assignment
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// Reason explains the last transition: the event's reason, extended
	// with the cause of a failure found while applying it.
	Reason string
}

// row is one line of the transition table: an event accepted in a set
// of states, with its guard and its effect. The effect returns the next
// state.
type row struct {
	from   []State
	guard  func(Worker, Input) error
	effect func(*Worker, Input) State
}

// table is the authoritative transition table. An event in a state it
// does not list is refused.
var table = map[Event]row{
	Start: {from: []State{Stopped},
		guard:  require("no session slot is reserved", func(g Guard) bool { return g.CapacityReserved }),
		effect: goTo(Starting)},
	Ready: {from: []State{Starting},
		guard: require("the session is not ready or its profile not confirmed",
			func(g Guard) bool { return g.SessionReady && g.ProfileConfirmed }),
		effect: goTo(Idle)},
	Assign: {from: []State{Idle},
		guard: func(_ Worker, in Input) error {
			if !in.Guard.AssignmentPersisted {
				return guardError("the assignment and its turn are not persisted")
			}
			return validAssignment(in.Assignment)
		},
		effect: func(w *Worker, in Input) State {
			assignment := in.Assignment
			w.Assignment = &assignment
			return Busy
		}},
	AcceptTurn: {from: []State{Busy},
		guard: require("the turn's rights are not revoked or its effects not reconciled",
			func(g Guard) bool { return g.RightsRevoked && g.Reconciled }),
		effect: release(Idle)},
	RequestInput: {from: []State{Busy},
		guard:  require("the input request is not recognized", func(g Guard) bool { return g.InputRecognized }),
		effect: goTo(WaitingInput)},
	Attach: {from: []State{Idle, Busy, WaitingInput},
		guard: require("the turn is neither quiescent nor interrupted",
			func(g Guard) bool { return g.TurnQuiescent || g.InterruptConfirmed }),
		effect: goTo(Attached)},
	Detach:         {from: []State{Attached}, effect: afterDetach},
	ConnectionLost: {from: []State{Attached}, effect: afterDetach},
	Pause: {from: []State{Idle, Busy, WaitingInput},
		guard: require("the interruption and the stop of the descendants are not confirmed",
			func(g Guard) bool { return g.InterruptConfirmed && g.DescendantsStopped }),
		effect: release(Paused)},
	Resume: {from: []State{Paused},
		guard: require("the profile or the continuation is not validated",
			func(g Guard) bool { return g.ProfileValidated && g.ContinuationValid }),
		effect: goTo(Starting)},
	ScaleDown: {from: []State{Idle, Busy, WaitingInput, Attached, Paused}, effect: goTo(Draining)},
	Drained: {from: []State{Draining},
		guard: require("the turn is not settled, a client is attached or descendants run",
			func(g Guard) bool { return g.TurnSettled && g.ClientDetached && g.DescendantsStopped }),
		effect: release(Stopped)},
	Stop: {from: except(Stopped),
		guard:  require("the effects are not reconciled", func(g Guard) bool { return g.Reconciled }),
		effect: release(Stopped)},
	Fail: {from: active(),
		guard: func(_ Worker, in Input) error {
			if in.Reason == "" {
				return guardError("a failure needs a reason")
			}
			return nil
		},
		effect: goTo(Failed)},
	Recover: {from: []State{Failed},
		guard: require("the cause is not lifted or descendants remain",
			func(g Guard) bool { return g.CauseLifted && g.DescendantsStopped }),
		effect: release(Stopped)},
}

// Accepts reports whether the table accepts the event in the state,
// before its guard is evaluated.
func Accepts(s State, e Event) bool {
	r, ok := table[e]
	return ok && contains(r.from, s)
}

// Apply evaluates one event against a worker and returns the worker
// after the transition. An event the table does not accept in the
// worker's state fails with ErrTransition and an unmet guard with
// ErrGuard; the worker is returned unchanged in every failure.
func Apply(w Worker, in Input, at time.Time) (Worker, error) {
	r, ok := table[in.Event]
	if !ok || !contains(r.from, w.State) {
		return w, fmt.Errorf("%w: %s in %s", ErrTransition, in.Event, w.State)
	}
	if r.guard != nil {
		if err := r.guard(w, in); err != nil {
			return w, fmt.Errorf("%s in %s: %w", in.Event, w.State, err)
		}
	}
	next := w
	if w.Assignment != nil {
		held := *w.Assignment
		next.Assignment = &held
	}
	next.Reason = in.Reason
	next.State = r.effect(&next, in)
	next.Version++
	next.UpdatedAt = at
	return next, nil
}

// afterDetach reconciles a detached worker by its continuation: back to
// IDLE without an assignment, to WAITING_INPUT with a valid one, and to
// FAILED when the effects are not reconciled or the continuation of the
// assignment is not valid.
func afterDetach(w *Worker, in Input) State {
	switch {
	case !in.Guard.Reconciled:
		w.Reason = failure(in, "the attached session is not reconciled")
		return Failed
	case w.Assignment == nil:
		return Idle
	case !in.Guard.ContinuationValid:
		w.Reason = failure(in, "the continuation of the assignment is not valid")
		return Failed
	}
	return WaitingInput
}

// failure is the reason of a failure found while applying an event.
func failure(in Input, cause string) string {
	if in.Reason != "" {
		return in.Reason + ": " + cause
	}
	return cause
}

// validAssignment checks the assignment an Assign event takes.
func validAssignment(a Assignment) error {
	switch {
	case a.TaskID == "" || a.TurnID == "":
		return guardError("the assignment has no task or no turn")
	case !containsRole(a.Role):
		return guardError(fmt.Sprintf("unknown role %q", a.Role))
	}
	return nil
}

// goTo is the effect of a row that only changes state.
func goTo(s State) func(*Worker, Input) State {
	return func(*Worker, Input) State { return s }
}

// release is the effect of a row that frees the worker's assignment.
func release(s State) func(*Worker, Input) State {
	return func(w *Worker, _ Input) State {
		w.Assignment = nil
		return s
	}
}

// require builds a guard from a predicate on the asserted facts.
func require(failure string, holds func(Guard) bool) func(Worker, Input) error {
	return func(_ Worker, in Input) error {
		if !holds(in.Guard) {
			return guardError(failure)
		}
		return nil
	}
}

// guardError wraps ErrGuard.
func guardError(failure string) error {
	return fmt.Errorf("%w: %s", ErrGuard, failure)
}

// except lists every state but the given ones.
func except(excluded ...State) []State {
	var states []State
	for _, s := range States {
		if !contains(excluded, s) {
			states = append(states, s)
		}
	}
	return states
}

// active lists the active states.
func active() []State {
	var states []State
	for _, s := range States {
		if s.Active() {
			states = append(states, s)
		}
	}
	return states
}

// contains reports whether states holds s.
func contains(states []State, s State) bool {
	for _, candidate := range states {
		if candidate == s {
			return true
		}
	}
	return false
}

// containsRole reports whether r is a known role.
func containsRole(r Role) bool {
	for _, candidate := range Roles {
		if candidate == r {
			return true
		}
	}
	return false
}
