// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"errors"
	"testing"
	"time"
)

var at = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// all asserts every fact of a guard.
var all = Guard{
	CapacityReserved: true, SessionReady: true, ProfileConfirmed: true, AssignmentPersisted: true,
	RightsRevoked: true, Reconciled: true, InputRecognized: true, TurnQuiescent: true,
	InterruptConfirmed: true, DescendantsStopped: true, ContinuationValid: true, ProfileValidated: true,
	TurnSettled: true, ClientDetached: true, CauseLifted: true,
}

var assignment = Assignment{TaskID: "t1", Role: Implementation, TurnID: "u1"}

// in builds an input with every guard fact asserted.
func in(e Event) Input {
	return Input{Event: e, Assignment: assignment, Reason: "because", Guard: all}
}

// apply applies inputs in order and fails the test on any error.
func apply(t *testing.T, w Worker, inputs ...Input) Worker {
	t.Helper()
	for _, input := range inputs {
		next, err := Apply(w, input, at)
		if err != nil {
			t.Fatalf("%s in %s: %v", input.Event, w.State, err)
		}
		w = next
	}
	return w
}

func TestTransitionTableMatchesTheLifecycle(t *testing.T) {
	want := map[Event][]State{
		Start:          {Stopped},
		Ready:          {Starting},
		Assign:         {Idle},
		AcceptTurn:     {Busy},
		RequestInput:   {Busy},
		Attach:         {Idle, Busy, WaitingInput},
		Detach:         {Attached},
		ConnectionLost: {Attached},
		Pause:          {Idle, Busy, WaitingInput},
		Resume:         {Paused},
		ScaleDown:      {Idle, Busy, WaitingInput, Attached, Paused},
		Drained:        {Draining},
		Stop:           {Starting, Idle, Busy, WaitingInput, Attached, Paused, Draining, Failed},
		Fail:           {Starting, Idle, Busy, WaitingInput, Attached, Paused, Draining},
		Recover:        {Failed},
	}
	if len(table) != len(want) {
		t.Fatalf("the table has %d events, want %d", len(table), len(want))
	}
	for event, from := range want {
		for _, s := range States {
			if got, expected := Accepts(s, event), contains(from, s); got != expected {
				t.Errorf("Accepts(%s, %s) = %v, want %v", s, event, got, expected)
			}
		}
	}
}

func TestUnlistedTransitionsAreRefusedUnchanged(t *testing.T) {
	w := Worker{Name: "claude-01", State: Stopped, Version: 1}
	for _, e := range []Event{Ready, Assign, AcceptTurn, Attach, Stop, Fail, Recover, "unknown"} {
		got, err := Apply(w, in(e), at)
		if !errors.Is(err, ErrTransition) {
			t.Fatalf("%s from STOPPED: err=%v", e, err)
		}
		if got.State != Stopped || got.Version != 1 {
			t.Fatalf("a refused %s changed the worker: %+v", e, got)
		}
	}
}

func TestNominalCycleHoldsAndReleasesTheAssignment(t *testing.T) {
	w := apply(t, Worker{State: Stopped, Version: 1}, in(Start), in(Ready), in(Assign))
	if w.State != Busy || w.Assignment == nil || *w.Assignment != assignment {
		t.Fatalf("assign: %+v", w)
	}
	w = apply(t, w, in(RequestInput))
	if w.State != WaitingInput || w.Assignment == nil {
		t.Fatalf("an input request dropped the continuation: %+v", w)
	}
	w = apply(t, w, in(Attach), in(Detach))
	if w.State != WaitingInput || w.Assignment == nil {
		t.Fatalf("a detach with a valid continuation: %+v", w)
	}
	w.State = Busy
	w = apply(t, w, in(AcceptTurn))
	if w.State != Idle || w.Assignment != nil {
		t.Fatalf("an accepted turn did not free the worker: %+v", w)
	}
	if w.Version != 8 || !w.UpdatedAt.Equal(at) || w.Reason != "because" {
		t.Fatalf("version=%d updated=%v reason=%q", w.Version, w.UpdatedAt, w.Reason)
	}
}

func TestGuardsRefuseUnassertedFacts(t *testing.T) {
	cases := []struct {
		from  State
		input Input
	}{
		{Stopped, Input{Event: Start}},
		{Starting, Input{Event: Ready, Guard: Guard{SessionReady: true}}},
		{Idle, Input{Event: Assign, Assignment: assignment}},
		{Idle, Input{Event: Assign, Guard: all, Assignment: Assignment{TaskID: "t1", Role: Review}}},
		{Idle, Input{Event: Assign, Guard: all, Assignment: Assignment{TaskID: "t1", Role: "owner", TurnID: "u1"}}},
		{Busy, Input{Event: AcceptTurn, Guard: Guard{RightsRevoked: true}}},
		{Busy, Input{Event: RequestInput}},
		{Busy, Input{Event: Attach}},
		{Busy, Input{Event: Pause, Guard: Guard{InterruptConfirmed: true}}},
		{Paused, Input{Event: Resume, Guard: Guard{ProfileValidated: true}}},
		{Draining, Input{Event: Drained, Guard: Guard{TurnSettled: true, DescendantsStopped: true}}},
		{Idle, Input{Event: Stop}},
		{Idle, Input{Event: Fail}},
		{Failed, Input{Event: Recover, Guard: Guard{CauseLifted: true}}},
	}
	for _, c := range cases {
		w := Worker{State: c.from, Version: 3}
		if c.from == Busy || c.from == Draining {
			held := assignment
			w.Assignment = &held
		}
		got, err := Apply(w, c.input, at)
		if !errors.Is(err, ErrGuard) {
			t.Fatalf("%s in %s: err=%v", c.input.Event, c.from, err)
		}
		if got.State != c.from || got.Version != 3 {
			t.Fatalf("a refused %s changed the worker: %+v", c.input.Event, got)
		}
	}
}

func TestAttachNeedsAQuiescentOrInterruptedTurn(t *testing.T) {
	for _, guard := range []Guard{{TurnQuiescent: true}, {InterruptConfirmed: true}} {
		w := apply(t, Worker{State: Idle}, Input{Event: Attach, Guard: guard})
		if w.State != Attached {
			t.Fatalf("attach with %+v: %s", guard, w.State)
		}
	}
}

func TestDetachFollowsTheContinuation(t *testing.T) {
	held := assignment
	cases := []struct {
		name       string
		assignment *Assignment
		guard      Guard
		want       State
	}{
		{"idle", nil, Guard{Reconciled: true}, Idle},
		{"continuation", &held, Guard{Reconciled: true, ContinuationValid: true}, WaitingInput},
		{"invalid continuation", &held, Guard{Reconciled: true}, Failed},
		{"unreconciled", nil, Guard{}, Failed},
	}
	for _, c := range cases {
		for _, e := range []Event{Detach, ConnectionLost} {
			w := apply(t, Worker{State: Attached, Assignment: c.assignment}, Input{Event: e, Guard: c.guard})
			if w.State != c.want {
				t.Fatalf("%s (%s): %s, want %s", c.name, e, w.State, c.want)
			}
			if c.want == Failed && w.Reason == "" {
				t.Fatalf("%s (%s): a failure without its reason", c.name, e)
			}
			if (w.Assignment == nil) != (c.assignment == nil) {
				t.Fatalf("%s (%s): the assignment changed: %+v", c.name, e, w.Assignment)
			}
		}
	}
}

func TestPauseResumeAndDrainNeverKeepAnIdleReservation(t *testing.T) {
	held := assignment
	w := apply(t, Worker{State: Busy, Assignment: &held}, in(Pause))
	if w.State != Paused || w.Assignment != nil {
		t.Fatalf("pause: %+v", w)
	}
	w = apply(t, w, in(Resume))
	if w.State != Starting {
		t.Fatalf("resume: %s", w.State)
	}
	w = apply(t, Worker{State: WaitingInput, Assignment: &held}, in(ScaleDown))
	if w.State != Draining || w.Assignment == nil {
		t.Fatalf("a scale down dropped the running activity: %+v", w)
	}
	w = apply(t, w, in(Drained))
	if w.State != Stopped || w.Assignment != nil {
		t.Fatalf("drained: %+v", w)
	}
}

func TestFailureKeepsTheAssignmentUntilRecovery(t *testing.T) {
	held := assignment
	w := apply(t, Worker{State: Busy, Assignment: &held}, Input{Event: Fail, Reason: "session lost"})
	if w.State != Failed || w.Assignment == nil || w.Reason != "session lost" {
		t.Fatalf("fail: %+v", w)
	}
	if _, err := Apply(w, Input{Event: Fail, Reason: "again"}, at); !errors.Is(err, ErrTransition) {
		t.Fatalf("a failed worker failed again: %v", err)
	}
	w = apply(t, w, in(Recover))
	if w.State != Stopped || w.Assignment != nil {
		t.Fatalf("recover: %+v", w)
	}
}

func TestStopFreesTheWorkerFromAnyState(t *testing.T) {
	for _, s := range States {
		if s == Stopped {
			continue
		}
		w := Worker{State: s}
		if s == Busy || s == WaitingInput {
			held := assignment
			w.Assignment = &held
		}
		w = apply(t, w, Input{Event: Stop, Guard: Guard{Reconciled: true}})
		if w.State != Stopped || w.Assignment != nil {
			t.Fatalf("stop from %s: %+v", s, w)
		}
	}
}

func TestApplyDoesNotShareTheAssignment(t *testing.T) {
	held := assignment
	w := Worker{State: Busy, Assignment: &held}
	next := apply(t, w, in(RequestInput))
	next.Assignment.Role = Review
	if w.Assignment.Role != Implementation {
		t.Fatal("the transition aliased the previous worker's assignment")
	}
}
