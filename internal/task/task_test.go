// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package task

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var allEvents = []Event{
	Assign, AcceptPlan, Implement, Fix, TestsPass, TestsFail, RequestChanges, Approve, Integrate,
	BuildCandidate, Conflict, ResolveConflict, RejectResolution, ApproveResolution, ValidationPass,
	ValidationFail, Block, Resume, Cancel,
}

func sha(c byte) string { return strings.Repeat(string(c), 40) }

func TestTableAcceptsExactlyTheListedEvents(t *testing.T) {
	active := []State{New, Planning, Implementing, Testing, Reviewing, Fixing, ReadyToIntegrate,
		Integrating, MergeConflict, Validating}
	expected := map[Event][]State{
		Assign: {New}, AcceptPlan: {Planning}, Implement: {Implementing}, Fix: {Fixing},
		TestsPass: {Testing}, TestsFail: {Testing}, RequestChanges: {Reviewing}, Approve: {Reviewing},
		Integrate: {ReadyToIntegrate}, BuildCandidate: {Integrating}, Conflict: {Integrating},
		ResolveConflict: {MergeConflict}, RejectResolution: {MergeConflict}, ApproveResolution: {MergeConflict},
		ValidationPass: {Validating}, ValidationFail: {Validating},
		Block: active, Resume: {Blocked}, Cancel: append(append([]State{}, active...), Blocked),
	}
	for _, event := range allEvents {
		for _, s := range States {
			if Accepts(s, event) != contains(expected[event], s) {
				t.Fatalf("%s in %s: accepted=%v", event, s, Accepts(s, event))
			}
		}
	}
	if Accepts(New, Created) || Accepts(New, Event("unknown")) {
		t.Fatal("an event outside the table was accepted")
	}
	for _, s := range States {
		if s.Terminal() != (s == Done || s == Cancelled) {
			t.Fatalf("%s terminal=%v", s, s.Terminal())
		}
	}
}

func TestUnlistedEventIsRefused(t *testing.T) {
	task := Task{State: New}
	got, err := Apply(task, Input{Event: TestsPass, Revision: sha('a')}, time.Now())
	if !errors.Is(err, ErrTransition) || got != task {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	for _, event := range allEvents {
		for _, terminal := range []State{Done, Cancelled} {
			if _, err := Apply(Task{State: terminal}, Input{Event: event, Reason: "r"}, time.Now()); !errors.Is(err, ErrTransition) {
				t.Fatalf("%s in %s: %v", event, terminal, err)
			}
		}
	}
}

func TestGuardsRefuseMissingFacts(t *testing.T) {
	cases := []struct {
		name  string
		task  Task
		input Input
	}{
		{"assign without assignment", Task{State: New}, Input{Event: Assign}},
		{"invalid plan", Task{State: Planning}, Input{Event: AcceptPlan}},
		{"invalid artifact", Task{State: Implementing}, Input{Event: Implement, Revision: sha('a')}},
		{"revision not a commit", Task{State: Implementing}, Input{Event: Implement, Revision: "HEAD", Guard: Guard{ArtifactValid: true}}},
		{"invalid fix", Task{State: Fixing}, Input{Event: Fix, Revision: sha('b')}},
		{"review not approved", Task{State: Reviewing, HeadSHA: sha('a')}, Input{Event: Approve, Revision: sha('a')}},
		{"integrate without trigger", Task{State: ReadyToIntegrate, HeadSHA: sha('a'), ApprovedSHA: sha('a')}, Input{Event: Integrate}},
		{"integrate with outdated approval", Task{State: ReadyToIntegrate, HeadSHA: sha('b'), ApprovedSHA: sha('a')}, Input{Event: Integrate, Guard: Guard{Trigger: Human}}},
		{"candidate not a commit", Task{State: Integrating}, Input{Event: BuildCandidate}},
		{"conflict without base", Task{State: Integrating}, Input{Event: Conflict}},
		{"invalid proposal", Task{State: MergeConflict}, Input{Event: ResolveConflict, Revision: sha('c'), Guard: Guard{ResolutionVerified: true}}},
		{"untested resolution", Task{State: MergeConflict}, Input{Event: ResolveConflict, Revision: sha('c'), Guard: Guard{ProposalValid: true}}},
		{"resolution lacks human approval", Task{State: MergeConflict}, Input{Event: ResolveConflict, Revision: sha('c'), Guard: Guard{ProposalValid: true, ResolutionVerified: true, HumanGateRequired: true}}},
		{"automatic resolution approval", Task{State: MergeConflict}, Input{Event: ApproveResolution, Revision: sha('c'), Guard: Guard{Trigger: Auto, ResolutionVerified: true}}},
		{"approval of an untested resolution", Task{State: MergeConflict}, Input{Event: ApproveResolution, Revision: sha('c'), Guard: Guard{Trigger: Human}}},
		{"done without publication", Task{State: Validating, ResultSHA: sha('d')}, Input{Event: ValidationPass, Revision: sha('d')}},
		{"fix without rollback", Task{State: Validating, ResultSHA: sha('d')}, Input{Event: ValidationFail, Revision: sha('d')}},
		{"block without reason", Task{State: Testing}, Input{Event: Block}},
		{"resume before the cause is lifted", Task{State: Blocked, ResumeState: Testing}, Input{Event: Resume, Guard: Guard{Reconciled: true}}},
		{"resume before reconciliation", Task{State: Blocked, ResumeState: Testing}, Input{Event: Resume, Guard: Guard{CauseLifted: true}}},
		{"resume without a continuation", Task{State: Blocked}, Input{Event: Resume, Guard: Guard{CauseLifted: true, Reconciled: true}}},
		{"resume into an unknown state", Task{State: Blocked, ResumeState: "LOST"}, Input{Event: Resume, Guard: Guard{CauseLifted: true, Reconciled: true}}},
		{"resume into a terminal state", Task{State: Blocked, ResumeState: Done}, Input{Event: Resume, Guard: Guard{CauseLifted: true, Reconciled: true}}},
		{"resume into a block", Task{State: Blocked, ResumeState: Blocked}, Input{Event: Resume, Guard: Guard{CauseLifted: true, Reconciled: true}}},
		{"cancel after publication", Task{State: Validating}, Input{Event: Cancel, Guard: Guard{ProcessesStopped: true, OperationSettled: true, Published: true}}},
		{"cancel with live processes", Task{State: Testing}, Input{Event: Cancel, Guard: Guard{OperationSettled: true}}},
		{"cancel with a pending operation", Task{State: Integrating}, Input{Event: Cancel, Guard: Guard{ProcessesStopped: true}}},
	}
	for _, c := range cases {
		got, err := Apply(c.task, c.input, time.Now())
		if !errors.Is(err, ErrGuard) || got != c.task {
			t.Fatalf("%s: got=%+v err=%v", c.name, got, err)
		}
	}
}

func TestResultsAboutAnotherRevisionAreStale(t *testing.T) {
	cases := []struct {
		task  Task
		event Event
	}{
		{Task{State: Testing, HeadSHA: sha('b')}, TestsPass},
		{Task{State: Testing, HeadSHA: sha('b')}, TestsFail},
		{Task{State: Reviewing, HeadSHA: sha('b')}, RequestChanges},
		{Task{State: Reviewing, HeadSHA: sha('b')}, Approve},
		{Task{State: Validating, ResultSHA: sha('b')}, ValidationPass},
		{Task{State: Validating, ResultSHA: sha('b')}, ValidationFail},
	}
	for _, c := range cases {
		got, err := Apply(c.task, Input{Event: c.event, Revision: sha('a'), Guard: Guard{
			AllReviewsApproved: true, Published: true, RolledBack: true,
		}}, time.Now())
		if !errors.Is(err, ErrStale) || got != c.task {
			t.Fatalf("%s: got=%+v err=%v", c.event, got, err)
		}
	}
}

func TestFixCyclesAreCountedThenExhausted(t *testing.T) {
	task := Task{State: Testing, HeadSHA: sha('a'), MaxFixCycles: 1}
	fixing, err := Apply(task, Input{Event: TestsFail, Revision: sha('a')}, time.Now())
	if err != nil || fixing.State != Fixing || fixing.FixCycles != 1 {
		t.Fatalf("got=%+v err=%v", fixing, err)
	}
	reviewing := Task{State: Reviewing, HeadSHA: sha('b'), FixCycles: 1, MaxFixCycles: 1}
	blocked, err := Apply(reviewing, Input{Event: RequestChanges, Revision: sha('b'), Reason: "rename"}, time.Now())
	if err != nil || blocked.State != Blocked || blocked.ResumeState != Reviewing ||
		blocked.BlockedReason != ReasonFixCyclesExhausted+": rename" || blocked.FixCycles != 1 {
		t.Fatalf("got=%+v err=%v", blocked, err)
	}
}

func TestBlockFromPlanningKeepsItsContinuationWithoutAFixCycle(t *testing.T) {
	task := Task{State: Planning, MaxFixCycles: DefaultMaxFixCycles}
	blocked, err := Apply(task, Input{Event: Block, Reason: "turn error"}, time.Now())
	if err != nil || blocked.State != Blocked || blocked.ResumeState != Planning || blocked.FixCycles != 0 {
		t.Fatalf("got=%+v err=%v", blocked, err)
	}
	if _, err := Apply(blocked, Input{Event: Block, Reason: "again"}, time.Now()); !errors.Is(err, ErrTransition) {
		t.Fatalf("a blocked task was blocked again: %v", err)
	}
	resumed, err := Apply(blocked, Input{Event: Resume, Guard: Guard{CauseLifted: true, Reconciled: true}}, time.Now())
	if err != nil || resumed.State != Planning || resumed.ResumeState != "" || resumed.BlockedReason != "" {
		t.Fatalf("got=%+v err=%v", resumed, err)
	}
}
