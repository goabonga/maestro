// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package task drives the workflow of a task: an explicit transition
// table that is the single authority on which event moves a task from
// which state to which, per-transition guards fed by explicit inputs,
// and a durable continuation for blocked tasks. The store persists each
// transition with its event in one compare-and-set transaction.
package task

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// State is the workflow state of a task.
type State string

// The states of a task. DONE and CANCELLED are terminal.
const (
	New              State = "NEW"
	Planning         State = "PLANNING"
	Implementing     State = "IMPLEMENTING"
	Testing          State = "TESTING"
	Reviewing        State = "REVIEWING"
	Fixing           State = "FIXING"
	ReadyToIntegrate State = "READY_TO_INTEGRATE"
	Integrating      State = "INTEGRATING"
	MergeConflict    State = "MERGE_CONFLICT"
	Validating       State = "VALIDATING"
	Blocked          State = "BLOCKED"
	Done             State = "DONE"
	Cancelled        State = "CANCELLED"
)

// States lists every state of a task.
var States = []State{
	New, Planning, Implementing, Testing, Reviewing, Fixing, ReadyToIntegrate,
	Integrating, MergeConflict, Validating, Blocked, Done, Cancelled,
}

// Terminal reports whether no transition leaves the state.
func (s State) Terminal() bool {
	return s == Done || s == Cancelled
}

// Event is what happens to a task: each row of the transition table
// is one event accepted in a set of states.
type Event string

// The events of the transition table.
const (
	// Assign: an assignment is available for the planning turn.
	Assign Event = "assign"
	// AcceptPlan: the PLAN artifact is valid.
	AcceptPlan Event = "accept-plan"
	// Implement: the implementation artifact and its revision are valid.
	Implement Event = "implement"
	// Fix: the correction artifact and its revision are valid.
	Fix Event = "fix"
	// TestsPass: the tests passed on the current revision.
	TestsPass Event = "tests-pass"
	// TestsFail: the tests failed on the current revision.
	TestsFail Event = "tests-fail"
	// RequestChanges: a review asked for changes on the current revision.
	RequestChanges Event = "request-changes"
	// Approve: every required review approved the current revision.
	Approve Event = "approve"
	// Integrate: a human or the automatic gate asked for integration.
	Integrate Event = "integrate"
	// BuildCandidate: the integration candidate is built.
	BuildCandidate Event = "build-candidate"
	// Conflict: the integration found a conflict.
	Conflict Event = "conflict"
	// ResolveConflict: a verified conflict resolution is retried.
	ResolveConflict Event = "resolve-conflict"
	// RejectResolution: a conflict resolution failed or was rejected.
	RejectResolution Event = "reject-resolution"
	// ApproveResolution: a human approved a tested resolution.
	ApproveResolution Event = "approve-resolution"
	// ValidationPass: the integration tests passed and the atomic
	// publication succeeded.
	ValidationPass Event = "validation-pass"
	// ValidationFail: the integration tests failed and the rollback is
	// confirmed.
	ValidationFail Event = "validation-fail"
	// Block: a turn error, timeout, exceeded budget, input wait,
	// invalid contract, inconsistency or human gate stops the task.
	Block Event = "block"
	// Resume: the cause of a block is lifted.
	Resume Event = "resume"
	// Cancel: the task is cancelled.
	Cancel Event = "cancel"
)

// Errors of the task workflow.
var (
	// ErrTransition reports an event the table does not accept in the
	// task's state, or a transition lost to a concurrent one.
	ErrTransition = errors.New("transition refused")
	// ErrGuard reports an accepted event whose guard does not hold.
	ErrGuard = errors.New("guard not satisfied")
	// ErrStale reports a response about another revision than the
	// current one: it is recorded and leaves the state unchanged.
	ErrStale = errors.New("stale event")
	// ErrNotFound reports an unknown task.
	ErrNotFound = errors.New("unknown task")
)

// DefaultMaxFixCycles bounds the entries into FIXING of a task.
const DefaultMaxFixCycles = 3

// MaxConflictFailures is the number of failed conflict resolutions,
// per integration base, after which the task blocks.
const MaxConflictFailures = 2

// ReasonFixCyclesExhausted is the blocked reason of a task that needs
// a correction when its fix cycles are exhausted.
const ReasonFixCyclesExhausted = "fix cycles exhausted"

// ReasonConflictUnresolved is the blocked reason of a task whose
// second conflict resolution failed.
const ReasonConflictUnresolved = "conflict resolution failed twice"

// Trigger is who asked for an integration.
type Trigger string

// The integration triggers.
const (
	// Human is maestro integrate on the task.
	Human Trigger = "human"
	// Auto is the scheduler under an automatic merge gate.
	Auto Trigger = "auto"
)

// Guard carries the facts the guards of the table depend on. The
// components that establish them (assignments, artifacts, test runner,
// reviews, integration, process supervision) report them here; a fact
// left false does not hold.
type Guard struct {
	// AssignmentAvailable: an assignment and its turn are persisted.
	AssignmentAvailable bool
	// PlanValid: the PLAN artifact is valid.
	PlanValid bool
	// ArtifactValid: the implementation or correction artifact and its
	// revision are valid.
	ArtifactValid bool
	// AllReviewsApproved: every required review approves the revision.
	AllReviewsApproved bool
	// Trigger: who asks for integration or approves a resolution.
	Trigger Trigger
	// ProposalValid: the conflict resolution proposal is valid.
	ProposalValid bool
	// ResolutionVerified: the tests and review of the resolved diff
	// passed.
	ResolutionVerified bool
	// HumanGateRequired: a resolution needs a human approval of its
	// candidate before integration resumes.
	HumanGateRequired bool
	// RolledBack: the failed integration candidate is rolled back.
	RolledBack bool
	// Published: the atomic publication succeeded.
	Published bool
	// CauseLifted: the cause of the block is lifted.
	CauseLifted bool
	// Reconciled: the effects of the interrupted step are reconciled.
	Reconciled bool
	// ProcessesStopped: the task's processes are stopped.
	ProcessesStopped bool
	// OperationSettled: no integration operation is pending: it was
	// abandoned or reconciled.
	OperationSettled bool
}

// Input is one event submitted to a task.
type Input struct {
	Event Event
	// Revision is the commit the event is about: the implemented or
	// fixed revision, the tested or reviewed revision, the integration
	// candidate, or the proposed resolution.
	Revision string
	// Base is the integration base of a conflict.
	Base string
	// Reason explains the event; a block requires it.
	Reason string
	Guard  Guard
}

// Task is one unit of work driven through the workflow.
type Task struct {
	ID string
	// ProjectID is the registered project the task belongs to.
	ProjectID string
	// Description is what the task is asked to achieve.
	Description string
	ConfigID    string
	// BaseSHA is the commit the task branch starts from.
	BaseSHA string
	Branch  string
	State   State
	// Version counts the transitions applied to the task.
	Version int64
	// ResumeState is the state a blocked task resumes in; empty
	// outside BLOCKED.
	ResumeState State
	// BlockedReason is why the task is blocked; empty outside BLOCKED.
	BlockedReason string
	// HeadSHA is the revision under test and review.
	HeadSHA string
	// ApprovedSHA is the revision every required review approved;
	// empty when no approval holds.
	ApprovedSHA string
	// ResultSHA is the integration candidate under validation.
	ResultSHA string
	// FixCycles counts the entries into FIXING.
	FixCycles    int
	MaxFixCycles int
	// ConflictBase is the integration base of the current conflict.
	ConflictBase string
	// ConflictFailures counts the failed resolutions on ConflictBase.
	ConflictFailures int
	// ResolutionApprovedSHA is the resolution a human approved.
	ResolutionApprovedSHA string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	// Reason explains the last transition.
	Reason string
}

// row is one line of the transition table: an event accepted in a set
// of states, with its guard and its effect. The effect returns the next
// state.
type row struct {
	from     []State
	revision func(Task) string
	guard    func(Task, Input) error
	effect   func(*Task, Input) State
}

// nonTerminal lists the states an event may leave.
func nonTerminal(except ...State) []State {
	var states []State
	for _, state := range States {
		if state.Terminal() || contains(except, state) {
			continue
		}
		states = append(states, state)
	}
	return states
}

// head and result name the revision a result event must refer to.
func head(t Task) string   { return t.HeadSHA }
func result(t Task) string { return t.ResultSHA }

// table is the authoritative transition table. An event in a state it
// does not list is refused.
var table = map[Event]row{
	Assign: {from: []State{New},
		guard:  require(func(_ Task, in Input) bool { return in.Guard.AssignmentAvailable }, "no assignment available"),
		effect: goTo(Planning)},
	AcceptPlan: {from: []State{Planning},
		guard:  require(func(_ Task, in Input) bool { return in.Guard.PlanValid }, "the plan is not valid"),
		effect: goTo(Implementing)},
	Implement:      {from: []State{Implementing}, guard: validRevision, effect: freezeRevision},
	Fix:            {from: []State{Fixing}, guard: validRevision, effect: freezeRevision},
	TestsPass:      {from: []State{Testing}, revision: head, effect: goTo(Reviewing)},
	TestsFail:      {from: []State{Testing}, revision: head, effect: fixOrBlock},
	RequestChanges: {from: []State{Reviewing}, revision: head, effect: fixOrBlock},
	Approve: {from: []State{Reviewing}, revision: head,
		guard: require(func(_ Task, in Input) bool { return in.Guard.AllReviewsApproved }, "a required review does not approve"),
		effect: func(t *Task, _ Input) State {
			t.ApprovedSHA = t.HeadSHA
			return ReadyToIntegrate
		}},
	Integrate: {from: []State{ReadyToIntegrate},
		guard: func(t Task, in Input) error {
			if in.Guard.Trigger != Human && in.Guard.Trigger != Auto {
				return guardError("integration needs a human or automatic trigger")
			}
			if t.ApprovedSHA == "" || t.ApprovedSHA != t.HeadSHA {
				return guardError("the approvals no longer hold")
			}
			return nil
		},
		effect: goTo(Integrating)},
	BuildCandidate: {from: []State{Integrating},
		guard: require(func(_ Task, in Input) bool { return objectID.MatchString(in.Revision) }, "the candidate is not a commit"),
		effect: func(t *Task, in Input) State {
			t.ResultSHA = in.Revision
			return Validating
		}},
	Conflict: {from: []State{Integrating},
		guard: require(func(_ Task, in Input) bool { return objectID.MatchString(in.Base) }, "the integration base is not a commit"),
		effect: func(t *Task, in Input) State {
			if t.ConflictBase != in.Base {
				t.ConflictBase, t.ConflictFailures = in.Base, 0
			}
			t.ResolutionApprovedSHA = ""
			return MergeConflict
		}},
	ResolveConflict: {from: []State{MergeConflict},
		guard: func(t Task, in Input) error {
			switch {
			case !objectID.MatchString(in.Revision):
				return guardError("the resolution is not a commit")
			case !in.Guard.ProposalValid:
				return guardError("the resolution proposal is not valid")
			case !in.Guard.ResolutionVerified:
				return guardError("the resolved diff is not tested and reviewed")
			case in.Guard.HumanGateRequired && t.ResolutionApprovedSHA != in.Revision:
				return guardError("the resolution lacks its human approval")
			case t.ConflictFailures >= MaxConflictFailures:
				return guardError("conflict resolutions are exhausted")
			}
			return nil
		},
		effect: func(t *Task, _ Input) State {
			t.ResolutionApprovedSHA = ""
			return Integrating
		}},
	RejectResolution: {from: []State{MergeConflict},
		effect: func(t *Task, in Input) State {
			t.ConflictFailures++
			t.ResolutionApprovedSHA = ""
			if t.ConflictFailures >= MaxConflictFailures {
				return block(t, MergeConflict, ReasonConflictUnresolved, in.Reason)
			}
			return MergeConflict
		}},
	ApproveResolution: {from: []State{MergeConflict},
		guard: func(_ Task, in Input) error {
			switch {
			case in.Guard.Trigger != Human:
				return guardError("only a human approves a resolution")
			case !objectID.MatchString(in.Revision):
				return guardError("the resolution is not a commit")
			case !in.Guard.ResolutionVerified:
				return guardError("the resolution is not tested and reviewed")
			}
			return nil
		},
		effect: func(t *Task, in Input) State {
			t.ResolutionApprovedSHA = in.Revision
			return MergeConflict
		}},
	ValidationPass: {from: []State{Validating}, revision: result,
		guard:  require(func(_ Task, in Input) bool { return in.Guard.Published }, "the publication did not succeed"),
		effect: goTo(Done)},
	ValidationFail: {from: []State{Validating}, revision: result,
		guard:  require(func(_ Task, in Input) bool { return in.Guard.RolledBack }, "the rollback is not confirmed"),
		effect: fixOrBlock},
	Block: {from: nonTerminal(Blocked),
		guard: require(func(_ Task, in Input) bool { return in.Reason != "" }, "a block needs a reason"),
		effect: func(t *Task, in Input) State {
			return block(t, t.State, in.Reason, "")
		}},
	Resume: {from: []State{Blocked},
		guard: func(_ Task, in Input) error {
			if !in.Guard.CauseLifted || !in.Guard.Reconciled {
				return guardError("the cause is not lifted or the step not reconciled")
			}
			return nil
		},
		effect: func(t *Task, _ Input) State {
			next := t.ResumeState
			t.ResumeState, t.BlockedReason = "", ""
			return next
		}},
	Cancel: {from: nonTerminal(),
		guard: func(_ Task, in Input) error {
			switch {
			case in.Guard.Published:
				return guardError("the publication already succeeded")
			case !in.Guard.ProcessesStopped:
				return guardError("the task's processes are not stopped")
			case !in.Guard.OperationSettled:
				return guardError("an integration operation is pending")
			}
			return nil
		},
		effect: func(t *Task, _ Input) State {
			t.ResumeState, t.BlockedReason = "", ""
			return Cancelled
		}},
}

// objectID matches a full SHA-1 or SHA-256 commit id.
var objectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Accepts reports whether the table accepts the event in the state,
// before its guard is evaluated.
func Accepts(s State, e Event) bool {
	r, ok := table[e]
	return ok && contains(r.from, s)
}

// Apply evaluates one event against a task and returns the task after
// the transition. An event the table does not accept in the task's
// state fails with ErrTransition, an unmet guard with ErrGuard, and a
// result about another revision than the current one with ErrStale;
// the task is returned unchanged in every failure.
func Apply(t Task, in Input, at time.Time) (Task, error) {
	r, ok := table[in.Event]
	if !ok || !contains(r.from, t.State) {
		return t, fmt.Errorf("%w: %s in %s", ErrTransition, in.Event, t.State)
	}
	if r.revision != nil {
		if current := r.revision(t); current == "" || in.Revision != current {
			return t, fmt.Errorf("%w: %s about %q, current revision is %q", ErrStale, in.Event, in.Revision, current)
		}
	}
	if r.guard != nil {
		if err := r.guard(t, in); err != nil {
			return t, fmt.Errorf("%s in %s: %w", in.Event, t.State, err)
		}
	}
	next := t
	next.State = r.effect(&next, in)
	next.Version++
	next.Reason, next.UpdatedAt = in.Reason, at
	return next, nil
}

// goTo is the effect of a row that only changes state.
func goTo(s State) func(*Task, Input) State {
	return func(*Task, Input) State { return s }
}

// require builds a guard from a predicate.
func require(holds func(Task, Input) bool, failure string) func(Task, Input) error {
	return func(t Task, in Input) error {
		if !holds(t, in) {
			return guardError(failure)
		}
		return nil
	}
}

// guardError wraps ErrGuard.
func guardError(failure string) error {
	return fmt.Errorf("%w: %s", ErrGuard, failure)
}

// validRevision guards an implementation or a correction.
func validRevision(_ Task, in Input) error {
	if !in.Guard.ArtifactValid {
		return guardError("the artifact is not valid")
	}
	if !objectID.MatchString(in.Revision) {
		return guardError("the revision is not a commit")
	}
	return nil
}

// freezeRevision freezes the revision to test. A new revision
// invalidates the previous reviews and tests: they were about another
// revision.
func freezeRevision(t *Task, in Input) State {
	t.HeadSHA, t.ApprovedSHA = in.Revision, ""
	return Testing
}

// fixOrBlock handles a needed correction: an entry into FIXING while
// fix cycles remain, a block that resumes in the current state
// otherwise.
func fixOrBlock(t *Task, in Input) State {
	if t.FixCycles >= t.MaxFixCycles {
		return block(t, t.State, ReasonFixCyclesExhausted, in.Reason)
	}
	t.FixCycles++
	t.ApprovedSHA, t.ResultSHA = "", ""
	return Fixing
}

// block records the continuation of a blocked task.
func block(t *Task, resume State, reason, detail string) State {
	if detail != "" {
		reason += ": " + detail
	}
	t.ResumeState, t.BlockedReason = resume, reason
	return Blocked
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
