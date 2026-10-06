// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goabonga/maestro/internal/agent"
	"github.com/goabonga/maestro/internal/budget"
	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/handoff"
	"github.com/goabonga/maestro/internal/provision"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/testrun"
	"github.com/goabonga/maestro/internal/turn"
	"github.com/goabonga/maestro/internal/worktree"
)

// handoffPaths are the runtime paths of the handoff documents, refused
// in a task's commits like the provisioned files.
var handoffPaths = provision.RuntimePaths{".maestro/"}

// job is one step of a task: a worker turn, with its format repair,
// or a test run.
type job struct {
	e       *Engine
	project worktree.Project
	task    task.Task
	worker  Worker
	session Session
	role    Role
	kind    handoff.Kind
	// worktree is where the turn runs, once checked out.
	worktree string
	// interval is the open active interval of the step.
	interval *budget.Interval
	// exceeded is the time budget the step exceeded when its interval
	// was stopped.
	exceeded error
}

// run drives one worker turn on the task, from its admission to the
// task's transition.
func (j *job) run(ctx context.Context) error {
	u, admitted, err := j.admit("")
	if err != nil || !admitted {
		return err
	}
	if j.task.State == task.New {
		next, err := j.e.tasks().Transition(j.task.ID, task.Input{Event: task.Assign,
			Reason: fmt.Sprintf("assigned to worker %s, turn %s", j.worker.Name, u.ID),
			Guard:  task.Guard{AssignmentAvailable: true}})
		if err != nil {
			return j.abandon(u, "the task did not take the assignment: "+err.Error(), err)
		}
		j.task = next
	}
	if started, err := j.startClock(&u); err != nil || !started {
		return err
	}
	worktree, err := j.session.Checkout(j.task, revision(j.task))
	if err != nil {
		return j.fail(u, turn.Failed, "check out the task: "+err.Error())
	}
	j.worktree = worktree
	prompt, err := j.prompt(u)
	if err != nil {
		return j.fail(u, turn.Failed, "prepare the prompt: "+err.Error())
	}
	u, ended, err := j.exchange(ctx, u, prompt)
	if err != nil || ended {
		return err
	}
	return j.settle(ctx, u)
}

// admit prepares a turn of the job's worker on the task, a first turn
// or the retry of a previous one: the turn is stored, one turn of the
// task's budget is reserved for it, and the worker takes the
// assignment. A reservation over budget blocks the task. It reports
// whether the turn can go ahead.
func (j *job) admit(previous string) (turn.Turn, bool, error) {
	var u turn.Turn
	var err error
	if previous == "" {
		u, err = j.e.turns().Create(j.task.ID, j.worker.Agent, j.task.ConfigID)
	} else {
		u, err = j.e.turns().Retry(previous)
	}
	if err != nil {
		return u, false, err
	}
	if _, err := j.e.budgets().Reserve(j.task.ID, j.worker.Agent, u.ID); err != nil {
		if _, terr := j.e.turns().Transition(u.ID, turn.Failed, err.Error()); terr != nil {
			return u, false, errors.Join(err, terr)
		}
		return u, false, j.blockBudget(err)
	}
	assigned, err := j.e.workers().Transition(j.project.ID, j.worker.Name, Input{Event: Assign,
		Assignment: Assignment{TaskID: j.task.ID, Role: j.role, TurnID: u.ID},
		Reason:     fmt.Sprintf("%s turn %s of task %s", j.role, u.ID, j.task.ID),
		Guard:      Guard{AssignmentPersisted: true}})
	if err != nil {
		_, terr := j.e.turns().Transition(u.ID, turn.Interrupted, "the worker did not take the assignment: "+err.Error())
		if errors.Is(err, ErrTransition) {
			// The worker changed concurrently: the task waits for
			// another one.
			return u, false, terr
		}
		return u, false, errors.Join(err, terr)
	}
	j.worker = assigned
	return u, true, nil
}

// stop ends a step the engine stops driving on an error, the context's
// or a store's: its active interval is closed, the turn its worker is
// still BUSY with is aborted and a task left waiting for input is
// blocked. What it cannot apply stays for the next Drive to recover.
func (j *job) stop(cause error) error {
	var errs []error
	if err := j.stopClock(); err != nil {
		errs = append(errs, err)
	}
	if j.worker.Name == "" {
		return errors.Join(errs...)
	}
	w, err := j.e.workers().Get(j.project.ID, j.worker.Name)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if w.Assignment == nil || w.Assignment.TaskID != j.task.ID {
		return errors.Join(errs...)
	}
	reason := "the engine stopped driving the turn: " + cause.Error()
	switch w.State {
	case Busy:
		errs = append(errs, j.e.abort(j.project, w, reason))
	case WaitingInput:
		errs = append(errs, j.block(fmt.Sprintf("turn %s of worker %s: %s", w.Assignment.TurnID, w.Name, reason)))
	}
	return errors.Join(errs...)
}

// abandon interrupts a turn no prompt was sent in and releases its
// worker. A cause that is a refused transition, the task or the worker
// having changed concurrently, is not an error of the engine.
func (j *job) abandon(u turn.Turn, reason string, cause error) error {
	var errs []error
	if _, err := j.e.turns().Transition(u.ID, turn.Interrupted, reason); err != nil {
		errs = append(errs, err)
	}
	if _, err := j.e.workers().Transition(j.project.ID, j.worker.Name, Input{Event: AcceptTurn, Reason: reason,
		Guard: Guard{RightsRevoked: true, Reconciled: true}}); err != nil {
		errs = append(errs, err)
	}
	if !errors.Is(cause, task.ErrTransition) && !errors.Is(cause, task.ErrGuard) &&
		!errors.Is(cause, budget.ErrExceeded) && !errors.Is(cause, budget.ErrClockRegression) {
		errs = append(errs, cause)
	}
	return errors.Join(errs...)
}

// startClock opens the active interval of the step. An interval a
// crash left open is charged first. A task out of time is blocked and
// the turn, if any, abandoned. It reports whether the step can go
// ahead.
func (j *job) startClock(u *turn.Turn) (bool, error) {
	interval, err := j.e.budgets().Start(j.task.ID)
	if errors.Is(err, budget.ErrIntervalOpen) {
		if _, err = j.e.budgets().Recover(j.task.ID); err == nil {
			interval, err = j.e.budgets().Start(j.task.ID)
		}
	}
	if err == nil {
		j.interval = interval
		return true, nil
	}
	if u != nil {
		if aerr := j.abandon(*u, err.Error(), err); aerr != nil {
			return false, errors.Join(err, aerr)
		}
	}
	return false, j.blockBudget(err)
}

// stopClock closes the active interval of the step. A time budget it
// finds exceeded is kept in j.exceeded; any other failure is returned.
func (j *job) stopClock() error {
	if j.interval == nil {
		return nil
	}
	interval := j.interval
	j.interval = nil
	_, err := interval.Stop()
	if errors.Is(err, budget.ErrExceeded) || errors.Is(err, budget.ErrClockRegression) {
		j.exceeded = err
		return nil
	}
	return err
}

// blockBudget blocks the task on a budget failure; any other error is
// returned as is.
func (j *job) blockBudget(cause error) error {
	if !errors.Is(cause, budget.ErrExceeded) && !errors.Is(cause, budget.ErrClockRegression) {
		return cause
	}
	blocked, err := j.e.budgets().Block(j.task.ID, cause)
	if err != nil {
		return refused(err)
	}
	j.task = blocked
	return nil
}

// block moves the task to BLOCKED with its continuation.
func (j *job) block(reason string) error {
	blocked, err := j.e.tasks().Transition(j.task.ID, task.Input{Event: task.Block, Reason: reason})
	if err != nil {
		return refused(err)
	}
	j.task = blocked
	return nil
}

// refused drops a transition the task refused because it changed
// concurrently, cancelled or blocked meanwhile: the engine has nothing
// left to apply to it.
func refused(err error) error {
	if errors.Is(err, task.ErrTransition) || errors.Is(err, task.ErrStale) {
		return nil
	}
	return err
}

// exchange sends a prompt in an admitted turn and follows the turn
// until the session detects its end or an input request, or the turn's
// bound passes. A completed turn has its rights revoked before it is
// returned VALIDATING, so the agent cannot change the worktree while its
// handoff is read; any other end is applied to the turn, the worker and
// the task, and reported as ended.
func (j *job) exchange(ctx context.Context, u turn.Turn, prompt string) (turn.Turn, bool, error) {
	u, err := j.e.turns().Transition(u.ID, turn.Running, "prompt sent to worker "+j.worker.Name)
	if err != nil {
		return u, true, err
	}
	if err := j.session.Send(prompt); err != nil {
		return u, true, j.fail(u, turn.Failed, "send the prompt: "+err.Error())
	}
	for {
		switch detection := j.session.Poll(); detection {
		case agent.DetectCompleted:
			if err := j.session.Settle(); err != nil {
				return u, true, j.fail(u, turn.Failed, "revoke the turn's rights: "+err.Error())
			}
			u, err := j.e.turns().Transition(u.ID, turn.Validating, "the agent ended its turn")
			return u, err != nil, err
		case agent.DetectWaitingInput:
			return u, true, j.waitInput(u)
		case agent.DetectFailed:
			return u, true, j.fail(u, turn.Failed, "the agent's turn failed")
		case agent.DetectInterrupted:
			return u, true, j.fail(u, turn.Interrupted, "the agent's turn was interrupted")
		}
		expired, found, err := j.e.turns().Expire(u.ID)
		if err != nil {
			return u, true, err
		}
		if found {
			return expired, true, j.fail(expired, turn.Failed, expired.Reason)
		}
		if err := j.e.wait(ctx); err != nil {
			return u, true, err
		}
	}
}

// waitInput applies an input request: the turn and the worker wait,
// the worker keeping the assignment as the continuation, and the task
// is blocked. The input wait stays bounded: Drive fails the worker once
// it expires.
func (j *job) waitInput(u turn.Turn) error {
	reason := fmt.Sprintf("turn %s of worker %s waits for input", u.ID, j.worker.Name)
	if _, err := j.e.turns().Transition(u.ID, turn.WaitingInput, "the agent asks for input"); err != nil {
		return err
	}
	if _, err := j.e.workers().Transition(j.project.ID, j.worker.Name, Input{Event: RequestInput, Reason: reason,
		Guard: Guard{InputRecognized: true}}); err != nil {
		return err
	}
	if err := j.stopClock(); err != nil {
		return err
	}
	return j.block(reason)
}

// fail ends a turn on an error: the turn ends in the given state, the
// worker fails, its session is closed and the task is blocked.
func (j *job) fail(u turn.Turn, end turn.State, cause string) error {
	reason := fmt.Sprintf("turn %s of worker %s: %s", u.ID, j.worker.Name, cause)
	var errs []error
	if !u.State.Terminal() {
		if _, err := j.e.turns().Transition(u.ID, end, cause); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := j.e.workers().Transition(j.project.ID, j.worker.Name, Input{Event: Fail, Reason: reason}); err != nil {
		errs = append(errs, err)
	}
	if err := endSession(j.session, j.worker.Name); err != nil {
		errs = append(errs, err)
	}
	if err := j.stopClock(); err != nil {
		errs = append(errs, err)
	}
	if err := j.block(reason); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// release frees the worker of a completed turn, whose rights exchange
// revoked.
func (j *job) release(reason string) error {
	freed, err := j.e.workers().Transition(j.project.ID, j.worker.Name, Input{Event: AcceptTurn, Reason: reason,
		Guard: Guard{RightsRevoked: true, Reconciled: true}})
	if err != nil {
		return err
	}
	j.worker = freed
	return nil
}

// assignment is what the turn's handoff document must answer.
func (j *job) assignment(u turn.Turn) handoff.Assignment {
	return handoff.Assignment{Kind: j.kind, TaskID: j.task.ID, TurnID: u.ID, AttemptID: u.AttemptID,
		WorkerID: j.worker.Name, ConfigID: j.task.ConfigID, TaskBaseSHA: j.task.BaseSHA}
}

// settle decides on the handoff of a completed turn: accepted when
// valid, one format repair when missing or invalid, blocked otherwise.
func (j *job) settle(ctx context.Context, u turn.Turn) error {
	assignment := j.assignment(u)
	verdict, err := handoff.EndTurn(j.worktree, assignment)
	if err != nil {
		return j.fail(u, turn.Failed, "read the handoff: "+err.Error())
	}
	if verdict.Outcome == handoff.Accepted {
		if verdict, err = j.verify(verdict); err != nil {
			return j.fail(u, turn.Failed, "verify the handoff: "+err.Error())
		}
	}
	switch verdict.Outcome {
	case handoff.Accepted:
		return j.accept(u, verdict)
	case handoff.RepairRequested:
		return j.repair(ctx, u, assignment, verdict)
	}
	return j.reject(u, verdict.Reason)
}

// verify checks an accepted document against Git and the task: the
// commits of an implementation and the revision and diff of a review.
// A document that does not hold asks for a repair, a commit touching a
// runtime path blocks the task, and a failure to verify is returned.
func (j *job) verify(verdict handoff.Verdict) (handoff.Verdict, error) {
	problem, err := j.check(verdict)
	switch {
	case err != nil:
		return verdict, err
	case problem == nil:
		return verdict, nil
	case errors.Is(problem, provision.ErrRuntimePath):
		return handoff.Verdict{Outcome: handoff.Blocked, Reason: problem.Error()}, nil
	}
	snapshot, err := handoff.Capture(j.worktree)
	if err != nil {
		return verdict, err
	}
	return handoff.Verdict{Outcome: handoff.RepairRequested, Reason: problem.Error(), Snapshot: snapshot}, nil
}

// check returns the problem of an accepted document, if any, or the
// error that kept it from being checked.
func (j *job) check(verdict handoff.Verdict) (problem error, err error) {
	switch payload := verdict.Payload.(type) {
	case handoff.ImplementationPayload:
		if err := handoff.VerifyCommits(j.worktree, verdict.Envelope, payload); err != nil {
			if errors.Is(err, handoff.ErrInvalid) {
				return err, nil
			}
			return nil, err
		}
		runtime := append(append(provision.RuntimePaths{}, handoffPaths...), j.session.RuntimePaths()...)
		if err := provision.CheckCommits(j.worktree, j.task.BaseSHA, payload.SourceSHA, runtime); err != nil {
			if errors.Is(err, provision.ErrRuntimePath) {
				return err, nil
			}
			return nil, err
		}
		imported, err := j.e.Projects.Import(j.project, worktree.Worker{Name: j.worker.Name, Dir: j.worker.Repository}, j.task.Branch)
		if err != nil {
			return nil, err
		}
		if imported != payload.SourceSHA {
			return fmt.Errorf("%w: the task branch is at %s, the document names %s", handoff.ErrInvalid, imported, payload.SourceSHA), nil
		}
	case handoff.ReviewPayload:
		if payload.ReviewedSHA != j.task.HeadSHA || verdict.Envelope.SourceHeadSHA != j.task.HeadSHA {
			return fmt.Errorf("%w: the review is about %s, the revision under review is %s",
				handoff.ErrInvalid, payload.ReviewedSHA, j.task.HeadSHA), nil
		}
		digest, err := diffDigest(j.project.Repository(), j.task.BaseSHA, j.task.HeadSHA)
		if err != nil {
			return nil, err
		}
		if payload.DiffDigest != digest {
			return fmt.Errorf("%w: the review's diff digest %s is not the digest %s of the reviewed diff",
				handoff.ErrInvalid, payload.DiffDigest, digest), nil
		}
	}
	return nil, nil
}

// repair asks for the single format repair of a turn's handoff, in a
// new turn and attempt of the same worker, then decides on it: any
// change to the code or a second invalid document blocks the task.
func (j *job) repair(ctx context.Context, u turn.Turn, failed handoff.Assignment, verdict handoff.Verdict) error {
	reason := "invalid handoff: " + verdict.Reason
	if _, err := j.e.turns().Transition(u.ID, turn.Failed, reason); err != nil {
		return err
	}
	if err := j.release(fmt.Sprintf("turn %s ended without a valid handoff", u.ID)); err != nil {
		return errors.Join(err, j.endStep(fmt.Sprintf("turn %s: %s; the worker could not be released", u.ID, reason)))
	}
	r, admitted, err := j.admit(u.ID)
	if err != nil || !admitted {
		return errors.Join(err, j.stopClock())
	}
	prompt, err := j.repairPrompt(r, failed, verdict.Reason)
	if err != nil {
		return j.fail(r, turn.Failed, "prepare the repair prompt: "+err.Error())
	}
	r, ended, err := j.exchange(ctx, r, prompt)
	if err != nil || ended {
		return err
	}
	repaired, err := handoff.EndRepair(j.worktree, failed, j.assignment(r), verdict.Snapshot)
	if err != nil {
		return j.fail(r, turn.Failed, "read the repaired handoff: "+err.Error())
	}
	if repaired.Outcome == handoff.Accepted {
		if repaired, err = j.verify(repaired); err != nil {
			return j.fail(r, turn.Failed, "verify the repaired handoff: "+err.Error())
		}
	}
	if repaired.Outcome == handoff.Accepted {
		return j.accept(r, repaired)
	}
	return j.reject(r, "format repair failed: "+repaired.Reason)
}

// reject ends a turn whose handoff cannot be accepted: the turn fails,
// the worker is released and the task is blocked.
func (j *job) reject(u turn.Turn, cause string) error {
	reason := fmt.Sprintf("turn %s of worker %s: %s", u.ID, j.worker.Name, cause)
	if _, err := j.e.turns().Transition(u.ID, turn.Failed, cause); err != nil {
		return err
	}
	if err := j.release(reason); err != nil {
		return err
	}
	return j.endStep(reason)
}

// endStep closes the step's interval and blocks the task.
func (j *job) endStep(reason string) error {
	if err := j.stopClock(); err != nil {
		return err
	}
	return j.block(reason)
}

// accept persists a valid handoff, ends the turn, releases the worker,
// then applies the task's transition.
func (j *job) accept(u turn.Turn, verdict handoff.Verdict) error {
	stored, err := handoff.Accept(j.e.DB, verdict.Envelope, verdict.Document)
	if errors.Is(err, handoff.ErrDuplicate) {
		return j.reject(u, err.Error())
	}
	if err != nil {
		return err
	}
	artifact := stored.Envelope.ArtifactID
	if _, err := j.e.turns().Transition(u.ID, turn.Succeeded, fmt.Sprintf("%s %s accepted", j.kind, artifact)); err != nil {
		return err
	}
	if err := j.release(fmt.Sprintf("turn %s accepted", u.ID)); err != nil {
		return errors.Join(err, j.endStep(fmt.Sprintf("turn %s: the worker could not be released", u.ID)))
	}
	if err := j.stopClock(); err != nil {
		return err
	}
	if j.exceeded != nil {
		return j.blockBudget(j.exceeded)
	}
	switch payload := verdict.Payload.(type) {
	case handoff.PlanPayload:
		return j.transition(task.Input{Event: task.AcceptPlan, Reason: "plan " + artifact + " accepted",
			Guard: task.Guard{PlanValid: true}})
	case handoff.ImplementationPayload:
		event := task.Implement
		if j.task.State == task.Fixing {
			event = task.Fix
		}
		return j.transition(task.Input{Event: event, Revision: payload.SourceSHA,
			Reason: fmt.Sprintf("implementation %s accepted at %s", artifact, payload.SourceSHA),
			Guard:  task.Guard{ArtifactValid: true}})
	case handoff.ReviewPayload:
		if payload.Verdict == "approve" {
			return j.transition(task.Input{Event: task.Approve, Revision: j.task.HeadSHA,
				Reason: "review " + artifact + " approves", Guard: task.Guard{AllReviewsApproved: true}})
		}
		references := make([]string, 0, len(payload.Issues))
		for _, issue := range payload.Issues {
			references = append(references, issue.ID)
		}
		if err := j.requestFix(references, []string{artifact}, u.ID, u.AttemptID); err != nil {
			return err
		}
		return j.transition(task.Input{Event: task.RequestChanges, Revision: j.task.HeadSHA,
			Reason: fmt.Sprintf("review %s requests changes: %s", artifact, strings.Join(references, ", "))})
	}
	return fmt.Errorf("no transition for a %s handoff", j.kind)
}

// transition applies an event to the task.
func (j *job) transition(in task.Input) error {
	next, err := j.e.tasks().Transition(j.task.ID, in)
	if err != nil {
		return refused(err)
	}
	j.task = next
	return nil
}

// requestFix produces the FIX_REQUEST of a correction about to start,
// before the task enters FIXING; nothing is produced when no fix cycle
// remains, the task then being blocked.
func (j *job) requestFix(references, inputs []string, turnID, attemptID string) error {
	if j.task.FixCycles >= j.task.MaxFixCycles {
		return nil
	}
	payload, err := json.Marshal(handoff.FixRequestPayload{Revision: j.task.HeadSHA, References: references})
	if err != nil {
		return err
	}
	document, err := json.Marshal(handoff.Envelope{
		SchemaVersion: handoff.SchemaVersion, ArtifactID: "fix-" + newRunID(), Kind: handoff.FixRequest,
		TaskID: j.task.ID, TurnID: turnID, AttemptID: attemptID, WorkerID: producer, ConfigID: j.task.ConfigID,
		InputArtifactIDs: inputs, Payload: payload,
	})
	if err != nil {
		return err
	}
	envelope, _, err := handoff.Decode(document)
	if err != nil {
		return err
	}
	_, err = handoff.Accept(j.e.DB, envelope, document)
	return err
}

// test runs the configured tests on the revision under test, keeps one
// report per command, and moves the task to REVIEWING when they pass,
// to FIXING with a fix request when they fail.
func (j *job) test() error {
	if started, err := j.startClock(nil); err != nil || !started {
		return err
	}
	snapshot, err := config.LoadSnapshot(j.e.DB, j.task.ConfigID)
	if err != nil {
		return errors.Join(err, j.stopClock())
	}
	commands := testrun.Commands(snapshot.Config)
	switch {
	case len(commands) == 0:
		return j.endStep(testrun.ErrNoCommands.Error())
	case j.e.Tester == nil:
		return j.endStep("no confined test runner on this host")
	}
	runID := newRunID()
	clone := filepath.Join(j.project.Dir, "tests", j.task.ID, runID)
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		return errors.Join(err, j.stopClock())
	}
	run, err := j.e.Tester.Run(j.project.Repository(), j.task.HeadSHA, clone, commands)
	if err != nil {
		return j.endStep(fmt.Sprintf("run the tests on %s: %v", j.task.HeadSHA, err))
	}
	turnID := "tests-" + runID
	stored, err := testrun.Accept(j.e.DB, run, testrun.Identity{ArtifactPrefix: turnID, TaskID: j.task.ID,
		TurnID: turnID, AttemptID: runID, WorkerID: producer, ConfigID: j.task.ConfigID})
	if err != nil {
		return errors.Join(err, j.stopClock())
	}
	if err := j.stopClock(); err != nil {
		return err
	}
	if j.exceeded != nil {
		return j.blockBudget(j.exceeded)
	}
	var reports, failed []string
	for i, report := range stored {
		reports = append(reports, report.Envelope.ArtifactID)
		if !run.Results[i].Passed() {
			failed = append(failed, report.Envelope.ArtifactID)
		}
	}
	if run.Passed() {
		_ = os.RemoveAll(clone)
		return j.transition(task.Input{Event: task.TestsPass, Revision: j.task.HeadSHA,
			Reason: fmt.Sprintf("tests passed on %s: %s", j.task.HeadSHA, strings.Join(reports, ", "))})
	}
	if len(failed) == 0 {
		failed = reports
	}
	if err := j.requestFix(failed, reports, turnID, runID); err != nil {
		return err
	}
	return j.transition(task.Input{Event: task.TestsFail, Revision: j.task.HeadSHA,
		Reason: fmt.Sprintf("tests failed on %s: %s; clone kept at %s", j.task.HeadSHA, strings.Join(failed, ", "), clone)})
}
