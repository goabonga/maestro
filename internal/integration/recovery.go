// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/goabonga/maestro/internal/handoff"
	"github.com/goabonga/maestro/internal/provision"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/testrun"
	"github.com/goabonga/maestro/internal/worktree"
)

// Action is what recovery decides for one operation from its observed
// state.
type Action string

// The recovery actions. Every action but ActionRetest and ActionBlock
// is applied by Recover through the journal's own methods.
const (
	// ActionFinalize commits an operation whose target already holds its
	// proven, tested result: the publication happened before the crash.
	ActionFinalize Action = "finalize"
	// ActionRecordResult records APPLIED from a valid durable result
	// reference the journal does not know yet.
	ActionRecordResult Action = "record-result"
	// ActionRetest leaves an applied operation for its tests to be run
	// again in a new clean clone of the same result.
	ActionRetest Action = "retest"
	// ActionRetryPublication retries the atomic publication of a tested
	// operation once, from its integration base.
	ActionRetryPublication Action = "retry-publication"
	// ActionRebuild builds the candidate of an integration again from its
	// persisted inputs, on its integration base.
	ActionRebuild Action = "rebuild"
	// ActionRollBack confirms the rollback of a failed operation.
	ActionRollBack Action = "roll-back"
	// ActionAbandon fails and rolls back an unfinished operation that is
	// never published.
	ActionAbandon Action = "abandon"
	// ActionRefreshView brings a stale, clean integration view to the
	// published commit.
	ActionRefreshView Action = "refresh-view"
	// ActionBlock leaves the operation as it is: nothing is rewritten or
	// deleted, and the diagnostic says why.
	ActionBlock Action = "block"
)

// ErrRecoveryBlocked is wrapped by the error of a blocking decision.
var ErrRecoveryBlocked = errors.New("recovery blocked")

// recoveryMaxSteps bounds the decisions taken in a row for one
// operation: recording a result or rebuilding a candidate leads to one
// more decision, nothing loops.
const recoveryMaxSteps = 4

// Decision is the recovery decision for one operation.
type Decision struct {
	OperationID string
	Type        Type
	// From is the journaled state the decision was taken in.
	From   State
	Action Action
	// ResultRef is the commit the durable result reference holds, empty
	// when it does not exist or for a publication.
	ResultRef string
	// Target is the commit the operation's target reference holds: the
	// integration branch of the canonical repository, or for PUBLISH
	// the published branch of the user repository, empty when absent.
	Target string
	// Applied reports that Recover applied the action.
	Applied bool
	// To is the journaled state after the decision.
	To State
	// Diagnostic explains the decision.
	Diagnostic string
	// Err is the cause of a blocking decision; it wraps
	// ErrRecoveryBlocked.
	Err error
}

// Blocked reports a decision that needs a human: the operation was left
// untouched.
func (d Decision) Blocked() bool {
	return d.Action == ActionBlock
}

// decided returns d with action and its diagnostic.
func (d Decision) decided(action Action, format string, args ...any) Decision {
	d.Action, d.Diagnostic, d.Err = action, fmt.Sprintf(format, args...), nil
	return d
}

// block returns d as a blocking decision with its diagnostic and cause.
func (d Decision) block(cause error, format string, args ...any) Decision {
	d.Action, d.Diagnostic = ActionBlock, fmt.Sprintf(format, args...)
	if cause != nil {
		d.Diagnostic += ": " + cause.Error()
		d.Err = fmt.Errorf("%w: operation %s: %w", ErrRecoveryBlocked, d.OperationID, cause)
	} else {
		d.Err = fmt.Errorf("%w: operation %s: %s", ErrRecoveryBlocked, d.OperationID, d.Diagnostic)
	}
	return d
}

// Recover reconciles every operation of a project from its observed
// state after a restart and returns the decisions taken, in order. The
// caller holds the project lock and starts no worker before it returns.
//
// The integration view is first refreshed when the integration branch
// holds the result of a committed operation, then FAILED operations are
// rolled back, then the unfinished INTEGRATE and SYNC operations are
// decided from the journal, their durable result reference, the
// integration branch and their candidate, and finally the unfinished
// PUBLISH operations from the published branch of the user repository.
// Each decision is applied through the journal's methods, except
// ActionRetest, left for the caller to run the tests in a new clone of
// the same result, and ActionBlock, which changes nothing. A crash never
// turns an operation into a success: an operation is finalized only
// when its target already holds its proven and tested result, and an
// abandoned operation is never published. The error reports a failure
// to read the journal; every other problem is a blocking decision.
func (s Store) Recover(project worktree.Project, runtime provision.RuntimePaths) ([]Decision, error) {
	ops, err := s.List(project.ID)
	if err != nil {
		return nil, err
	}
	var decisions []Decision
	if d, ok := s.recoveryView(project, ops); ok {
		decisions = append(decisions, d)
	}
	phases := []func(Operation) bool{
		func(op Operation) bool { return op.State == Failed },
		func(op Operation) bool { return recoveryInFlight(op) && op.Type != Publish },
		func(op Operation) bool { return recoveryInFlight(op) && op.Type == Publish },
	}
	for _, phase := range phases {
		for _, op := range ops {
			if !phase(op) {
				continue
			}
			taken, err := s.recoveryOne(project, op.ID, runtime)
			decisions = append(decisions, taken...)
			if err != nil {
				return decisions, err
			}
		}
		if ops, err = s.List(project.ID); err != nil {
			return decisions, err
		}
	}
	return decisions, nil
}

// recoveryInFlight reports an operation neither terminal nor failed.
func recoveryInFlight(op Operation) bool {
	return !op.State.Terminal() && op.State != Failed
}

// DecideRecovery returns the recovery decision for one operation of the
// project without applying it: it only reads the journal and Git. A
// terminal operation has no decision and is refused with ErrTransition.
func (s Store) DecideRecovery(project worktree.Project, id string) (Decision, error) {
	ops, err := s.List(project.ID)
	if err != nil {
		return Decision{}, err
	}
	for _, op := range ops {
		if op.ID != id {
			continue
		}
		if op.State.Terminal() {
			return Decision{}, fmt.Errorf("%w: operation %s is %s", ErrTransition, id, op.State)
		}
		return s.recoveryDecide(project, op, ops)
	}
	return Decision{}, fmt.Errorf("%w: %s in project %s", ErrNotFound, id, project.ID)
}

// recoveryOne decides and applies the recovery of one operation, and
// decides again after an action that leaves it unfinished but further
// along, a bounded number of times.
func (s Store) recoveryOne(project worktree.Project, id string, runtime provision.RuntimePaths) ([]Decision, error) {
	var taken []Decision
	for step := 0; step < recoveryMaxSteps; step++ {
		ops, err := s.List(project.ID)
		if err != nil {
			return taken, err
		}
		var op Operation
		for _, candidate := range ops {
			if candidate.ID == id {
				op = candidate
			}
		}
		if op.ID == "" || op.State.Terminal() {
			return taken, nil
		}
		d, err := s.recoveryDecide(project, op, ops)
		if err != nil {
			return taken, err
		}
		d, err = s.recoveryApply(project, op, ops, d, runtime)
		taken = append(taken, d)
		if err != nil {
			return taken, err
		}
		if !d.Applied || d.To.Terminal() || (d.Action != ActionRecordResult && d.Action != ActionRebuild) {
			return taken, nil
		}
	}
	return taken, nil
}

// recoveryDecide returns the decision for one unfinished operation.
func (s Store) recoveryDecide(project worktree.Project, op Operation, ops []Operation) (Decision, error) {
	d := Decision{OperationID: op.ID, Type: op.Type, From: op.State, To: op.State}
	switch op.Type {
	case Integrate, Sync:
		return s.recoveryCanonical(project, op, ops, d)
	case Publish:
		return recoveryPublication(project, op, d), nil
	}
	return d.block(nil, "unknown operation type %q", op.Type), nil
}

// recoveryCanonical decides an INTEGRATE or SYNC operation, whose
// target is the integration branch of the canonical repository and
// whose durable proof is its result reference.
func (s Store) recoveryCanonical(project worktree.Project, op Operation, ops []Operation, d Decision) (Decision, error) {
	repository := project.Repository()
	base := op.IntegrationBaseSHA
	branch, err := publicationBranch(project)
	if err != nil {
		return d.block(err, "the integration branch cannot be read"), nil
	}
	d.Target = branch
	ref, hasRef, err := ReadResultRef(repository, op.ID)
	if err != nil {
		return d.block(err, "the result reference cannot be read"), nil
	}
	if hasRef {
		d.ResultRef = ref
		if blocked, ok := recoveryCheckProof(project, op, ref, d); !ok {
			return blocked, nil
		}
	} else if op.ResultSHA != "" {
		return d.block(nil, "the journal records the result %s without its durable reference", op.ResultSHA), nil
	}
	published := false
	if hasRef {
		if published, err = recoveryContains(repository, branch, ref); err != nil {
			return d.block(err, "the integration branch cannot be compared with the result %s", ref), nil
		}
	}
	stale, err := recoveryStaleBase(repository, op, ops, branch)
	if err != nil {
		return d.block(err, "the integration branch %s cannot be compared with the base %s", branch, base), nil
	}
	unexpected := func() Decision {
		return d.block(nil, "the integration branch is at %s, neither the base %s nor the result %q of the operation nor a committed result",
			branch, base, ref)
	}

	if op.State == Failed {
		switch {
		case published:
			return d.block(ErrPublished, "the integration branch at %s holds the result %s of a failed operation", branch, ref), nil
		case branch == base || stale:
			return d.decided(ActionRollBack, "failed operation, integration branch at %s without its result: roll back and keep the candidate and diagnostics", branch), nil
		}
		return unexpected(), nil
	}

	abandoned, why, err := s.recoveryAbandoned(op, ops)
	if err != nil {
		return d.block(err, "the task of the operation cannot be read"), nil
	}
	if abandoned {
		switch {
		case published:
			return d.block(nil, "the operation is abandoned (%s) but the integration branch at %s holds its result %s", why, branch, ref), nil
		case branch == base || stale:
			return d.decided(ActionAbandon, "the operation is abandoned (%s): it is rolled back and never published", why), nil
		}
		return unexpected(), nil
	}

	if !hasRef {
		switch {
		case branch == base && op.Type == Sync:
			return d.decided(ActionAbandon, "the sync was interrupted before its imported commit was pinned: it is rolled back, run the sync again"), nil
		case branch == base:
			return s.recoveryRebuildDecision(project, op, d)
		case stale:
			return d.decided(ActionAbandon, "the integration branch moved to the committed result %s: a new base requires a new operation", branch), nil
		}
		return unexpected(), nil
	}

	switch {
	case branch == ref:
		if op.State != Tested {
			return d.block(nil, "the integration branch holds the result %s of an operation in %s, not %s", ref, op.State, Tested), nil
		}
		if err := s.recoveryEvidence(op); err != nil {
			return d.block(err, "the integration branch holds the result %s but its evidence is not valid", ref), nil
		}
		return d.decided(ActionFinalize, "the publication of %s succeeded before the crash: finalize", ref), nil
	case published:
		return d.block(nil, "the integration branch at %s descends from the result %s of an unfinished operation", branch, ref), nil
	case branch == base:
		switch op.State {
		case Started:
			return d.decided(ActionRecordResult, "valid result reference at %s not in the journal: record APPLIED from the proof", ref), nil
		case Applied:
			return d.decided(ActionRetest, "no test evidence for %s: run the tests again in a new clean clone of the same result", ref), nil
		case Tested:
			if err := s.recoveryEvidence(op); err != nil {
				return d.block(err, "the evidence of the tested result %s is not valid", ref), nil
			}
			return d.decided(ActionRetryPublication, "tested result %s, integration branch at its base: retry the atomic publication once", ref), nil
		}
		return d.block(nil, "a result reference exists for an operation in %s", op.State), nil
	case stale:
		return d.decided(ActionAbandon, "the integration branch moved to the committed result %s: a new base requires a new operation", branch), nil
	}
	return unexpected(), nil
}

// recoveryCheckProof checks that the durable result reference at ref is
// the proof of op: for an integration, a commit built from its frozen
// base, tree and metadata; for a sync, its frozen imported commit,
// descending from its base. The journal must record the same result,
// if any. It returns a blocking decision and false otherwise.
func recoveryCheckProof(project worktree.Project, op Operation, ref string, d Decision) (Decision, bool) {
	if op.ResultSHA != "" && op.ResultSHA != ref {
		return d.block(ErrInvalidResult, "the journal records the result %s, the result reference holds %s", op.ResultSHA, ref), false
	}
	if op.Type == Integrate {
		if _, _, err := ProveResult(project, op); err != nil {
			return d.block(err, "the result reference does not prove the operation"), false
		}
		return d, true
	}
	if ref != op.SourceHeadSHA {
		return d.block(ErrInvalidResult, "the result reference holds %s, the frozen imported commit is %s", ref, op.SourceHeadSHA), false
	}
	descends, err := recoveryIsAncestor(project.Repository(), op.IntegrationBaseSHA, ref)
	if err != nil || !descends {
		return d.block(errors.Join(ErrInvalidResult, err), "the imported commit %s does not descend from the base %s", ref, op.IntegrationBaseSHA), false
	}
	return d, true
}

// recoveryRebuildDecision decides an integration interrupted before its
// result reference, with the integration branch at its base. A
// candidate worktree left by the interrupted build is reconciled: a
// clean one, detached at the base, is removed before the rebuild; one
// holding anything else is kept for diagnosis and the operation is
// abandoned without publication.
func (s Store) recoveryRebuildDecision(project worktree.Project, op Operation, d Decision) (Decision, error) {
	path, err := CandidateWorktree(project, op.ID)
	if err != nil {
		return d.block(err, "the candidate worktree cannot be named"), nil
	}
	exists, clean, detail := recoveryCandidateState(project, path, op.IntegrationBaseSHA)
	if exists && !clean {
		return d.decided(ActionAbandon, "the interrupted candidate at %s is kept for diagnosis (%s): the operation is rolled back without publication", path, detail), nil
	}
	return d.decided(ActionRebuild, "no result reference, integration branch at the base %s: rebuild the candidate from the persisted inputs", op.IntegrationBaseSHA), nil
}

// recoveryPublication decides an unfinished PUBLISH operation from the
// published branch of the user repository: at the frozen target, the
// publication happened and is finalized once the target is proven to be
// a committed, tested result; at the old value, it did not and is
// abandoned until the user publishes again; anything else blocks.
func recoveryPublication(project worktree.Project, op Operation, d Decision) Decision {
	current, err := recoveryUserBranch(project.UserRepository)
	if err != nil {
		return d.block(err, "the published branch of the user repository cannot be read")
	}
	d.Target = current
	old, target := op.IntegrationBaseSHA, op.SourceHeadSHA
	if strings.Trim(old, "0") == "" {
		old = ""
	}
	switch {
	case !shaID.MatchString(target):
		return d.block(ErrInvalid, "the publication has no frozen target")
	case op.ResultSHA != "" && op.ResultSHA != target:
		return d.block(ErrInvalidResult, "the journal records %s, the frozen target is %s", op.ResultSHA, target)
	case op.State == Failed && current == old:
		return d.decided(ActionRollBack, "failed publication, %s of the user repository untouched: roll back", IntegrationBranch)
	case op.State == Failed:
		return d.block(nil, "failed publication but %s of the user repository is at %s, not at its old value %q", IntegrationBranch, current, old)
	case current == old && op.State != Applied:
		return d.decided(ActionAbandon, "%s of the user repository is still at its old value %q: the publication is rolled back, publish again", IntegrationBranch, old)
	case current == target && (op.State == Started || op.State == Applied):
		return d.decided(ActionFinalize, "%s of the user repository holds the target %s: finalize the publication", IntegrationBranch, target)
	}
	return d.block(nil, "publication in %s with %s of the user repository at %q, expected the old value %q or the target %s",
		op.State, IntegrationBranch, current, old, target)
}

// recoveryApply applies a decision through the journal's methods and
// returns it with its outcome. An action that fails without changing
// the operation becomes a blocking decision carrying the failure; one
// that moved the operation, such as a rebuild ending in FAILED or a
// publication committed before its view refresh failed, is applied and
// its diagnostic carries the failure. The error reports a failure to
// read the journal.
func (s Store) recoveryApply(project worktree.Project, op Operation, ops []Operation, d Decision, runtime provision.RuntimePaths) (Decision, error) {
	var err error
	switch d.Action {
	case ActionBlock, ActionRetest:
		return d, nil
	case ActionRecordResult:
		if op.Type == Integrate {
			_, err = s.ApplyIntegration(project, op.ID, d.ResultRef)
		} else {
			_, err = s.RecordResult(op.ID, d.ResultRef)
		}
	case ActionFinalize, ActionRetryPublication:
		switch op.Type {
		case Integrate:
			_, err = s.Publish(project, op.ID)
		case Sync:
			err = s.recoverySyncPublish(project, op, d.Action == ActionRetryPublication)
		case Publish:
			err = s.recoveryPublishFinish(project, op, ops)
		}
	case ActionRebuild:
		err = s.recoveryRebuild(project, op, runtime)
	case ActionRollBack:
		if op.Type == Integrate {
			_, err = s.RollBackIntegration(project, op.ID, op.Error)
		} else {
			_, err = s.RollBack(op.ID, "recovery: target untouched")
		}
	case ActionAbandon:
		if op.Type == Integrate {
			_, err = s.RollBackIntegration(project, op.ID, "recovery: "+d.Diagnostic)
		} else if _, err = s.Fail(op.ID, "recovery: "+d.Diagnostic); err == nil {
			_, err = s.RollBack(op.ID, "recovery: target untouched")
		}
	default:
		return d.block(nil, "unknown recovery action %q", d.Action), nil
	}
	after, getErr := s.Get(op.ID)
	if getErr != nil {
		return d, getErr
	}
	if after.Version == op.Version {
		if err == nil {
			err = errors.New("the operation did not change")
		}
		return d.block(err, "%s failed", d.Action), nil
	}
	d.Applied, d.To = true, after.State
	if err != nil {
		d.Diagnostic += "; " + err.Error()
	}
	return d, nil
}

// recoveryRebuild prunes the worktrees of the canonical repository,
// removes a clean candidate left by the interrupted build and builds
// the candidate again from the operation's persisted inputs.
func (s Store) recoveryRebuild(project worktree.Project, op Operation, runtime provision.RuntimePaths) error {
	repository := project.Repository()
	if _, err := repoGit(repository, "", nil, "worktree", "prune"); err != nil {
		return err
	}
	path, err := CandidateWorktree(project, op.ID)
	if err != nil {
		return err
	}
	exists, clean, detail := recoveryCandidateState(project, path, op.IntegrationBaseSHA)
	if exists {
		if !clean {
			return fmt.Errorf("%w: %s (%s)", ErrCandidateWorktree, path, detail)
		}
		// Not forced: Git refuses to remove a worktree holding changes.
		if _, err := repoGit(repository, "", nil, "worktree", "remove", path); err != nil {
			return err
		}
	}
	_, err = s.BuildCandidate(project, op.ID, runtime)
	return err
}

// recoverySyncPublish finalizes a tested sync whose integration branch
// holds its imported commit or, on a retry, first moves the branch from
// its base to that commit with one compare-and-swap, after checking the
// integration view. The view is then refreshed.
func (s Store) recoverySyncPublish(project worktree.Project, op Operation, retry bool) error {
	if retry {
		if _, _, err := publicationCheckView(project, op.IntegrationBaseSHA); err != nil {
			return err
		}
		if _, err := repoGit(project.Repository(), "", nil, "update-ref", IntegrationBranch, op.ResultSHA, op.IntegrationBaseSHA); err != nil {
			return fmt.Errorf("%w: %w", ErrBranchMoved, err)
		}
	}
	if _, err := s.Commit(op.ID, Evidence{}); err != nil {
		return err
	}
	if err := RefreshIntegrationView(project); err != nil {
		return fmt.Errorf("operation %s committed: %w", op.ID, err)
	}
	return nil
}

// recoveryPublishFinish records the result of a publication whose user
// branch holds its target, then commits it with the evidence of the
// committed operations it publishes.
func (s Store) recoveryPublishFinish(project worktree.Project, op Operation, ops []Operation) error {
	evidence, err := recoveryPublishEvidence(project, op, ops)
	if err != nil {
		return err
	}
	if op.State == Started {
		if _, err := s.RecordResult(op.ID, op.SourceHeadSHA); err != nil {
			return err
		}
	}
	_, err = s.Commit(op.ID, evidence)
	return err
}

// recoveryPublishEvidence returns the evidence of a publication: its
// target must be the result of a committed, tested INTEGRATE or SYNC
// operation of the project, and the evidence gathers that of every
// committed INTEGRATE or SYNC operation whose result the publication
// brings to the user repository.
func recoveryPublishEvidence(project worktree.Project, op Operation, ops []Operation) (Evidence, error) {
	target := op.SourceHeadSHA
	proven := false
	for _, other := range ops {
		if other.Type != Publish && other.State == Committed && other.ResultSHA == target && len(other.TestReportIDs) > 0 {
			proven = true
		}
	}
	if !proven {
		return Evidence{}, fmt.Errorf("%w: %s is not the result of a committed, tested operation", ErrGuard, target)
	}
	args := []string{"rev-list", target}
	if old := op.IntegrationBaseSHA; strings.Trim(old, "0") != "" {
		args = append(args, "^"+old)
	}
	out, err := repoGit(project.Repository(), "", nil, append(args, "--")...)
	if err != nil {
		return Evidence{}, err
	}
	published := map[string]bool{}
	for _, commit := range strings.Fields(out) {
		published[commit] = true
	}
	var evidence Evidence
	for _, other := range ops {
		if other.Type != Publish && other.State == Committed && published[other.ResultSHA] {
			evidence.TestReportIDs = unionIDs(evidence.TestReportIDs, other.TestReportIDs)
			evidence.ReviewArtifactIDs = unionIDs(evidence.ReviewArtifactIDs, other.ReviewArtifactIDs)
			evidence.ApprovalArtifactIDs = unionIDs(evidence.ApprovalArtifactIDs, other.ApprovalArtifactIDs)
		}
	}
	return evidence, nil
}

// recoveryEvidence checks the durable evidence of a tested operation:
// at least one test report, every report accepted, bound to the
// operation's result and passed. An integration's reports must belong
// to its task and the configuration snapshot it runs on, which must be
// the operation's own, and the task must be validating that result: its
// approvals still hold.
func (s Store) recoveryEvidence(op Operation) error {
	if len(op.TestReportIDs) == 0 {
		return fmt.Errorf("%w: no test report", ErrGuard)
	}
	var current task.Task
	if op.Type == Integrate {
		var err error
		if current, err = (task.Store{DB: s.DB}).Get(op.TaskID); err != nil {
			return err
		}
		switch {
		case current.ConfigID != op.ConfigID:
			return fmt.Errorf("%w: operation %s was prepared under %s, task %s runs on %s",
				handoff.ErrConfig, op.ID, op.ConfigID, op.TaskID, current.ConfigID)
		case current.State != task.Validating || current.ResultSHA != op.ResultSHA:
			return fmt.Errorf("%w: task %s is %s on %q, not validating %s", ErrGuard, op.TaskID, current.State, current.ResultSHA, op.ResultSHA)
		}
	}
	for _, id := range op.TestReportIDs {
		stored, err := handoff.Load(s.DB, id)
		if err != nil {
			return fmt.Errorf("%w: test report %s: %w", ErrGuard, id, err)
		}
		var payload handoff.TestReportPayload
		if op.Type == Integrate {
			payload, err = testrun.ForTask(stored.Document, op.TaskID, current.ConfigID, op.ResultSHA)
		} else {
			payload, err = testrun.ForRevision(stored.Document, op.ResultSHA)
		}
		if err != nil {
			return fmt.Errorf("%w: test report %s: %w", ErrGuard, id, err)
		}
		if !testrun.Passed(payload) {
			return fmt.Errorf("%w: test report %s did not pass", ErrGuard, id)
		}
	}
	return nil
}

// recoveryAbandoned reports an operation that must never be published:
// one superseded by another operation, or an integration whose task was
// cancelled.
func (s Store) recoveryAbandoned(op Operation, ops []Operation) (bool, string, error) {
	for _, other := range ops {
		if other.SupersedesOperationID == op.ID {
			return true, "superseded by operation " + other.ID, nil
		}
	}
	if op.Type != Integrate {
		return false, "", nil
	}
	current, err := (task.Store{DB: s.DB}).Get(op.TaskID)
	if err != nil {
		return false, "", err
	}
	if current.State == task.Cancelled {
		return true, "task " + op.TaskID + " is cancelled", nil
	}
	return false, "", nil
}

// recoveryStaleBase reports an integration branch that left the
// operation's base for the result of another committed INTEGRATE or
// SYNC operation descending from that base: the operation can never be
// published on it.
func recoveryStaleBase(repository string, op Operation, ops []Operation, branch string) (bool, error) {
	if branch == op.IntegrationBaseSHA {
		return false, nil
	}
	committed := false
	for _, other := range ops {
		if other.ID != op.ID && other.Type != Publish && other.State == Committed && other.ResultSHA == branch {
			committed = true
		}
	}
	if !committed {
		return false, nil
	}
	return recoveryIsAncestor(repository, op.IntegrationBaseSHA, branch)
}

// recoveryContains reports whether branch holds result: it is the
// result or descends from it.
func recoveryContains(repository, branch, result string) (bool, error) {
	if branch == result {
		return true, nil
	}
	return recoveryIsAncestor(repository, result, branch)
}

// recoveryIsAncestor reports whether ancestor is reachable from
// descendant in repository.
func recoveryIsAncestor(repository, ancestor, descendant string) (bool, error) {
	if !shaID.MatchString(ancestor) || !shaID.MatchString(descendant) {
		return false, fmt.Errorf("%w: %q and %q must be object ids", ErrInvalid, ancestor, descendant)
	}
	_, err := repoGit(repository, "", nil, "merge-base", "--is-ancestor", ancestor, descendant)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	}
	return false, err
}

// recoveryView decides the integration view: when the integration
// branch holds the result of a committed INTEGRATE or SYNC operation
// and the view is missing or behind it, the view is refreshed; a view
// holding an unexplained change blocks. Nothing is replayed.
func (s Store) recoveryView(project worktree.Project, ops []Operation) (Decision, bool) {
	branch, err := publicationBranch(project)
	if err != nil {
		return Decision{}, false
	}
	var head *Operation
	for i := range ops {
		if ops[i].Type != Publish && ops[i].State == Committed && ops[i].ResultSHA == branch {
			head = &ops[i]
		}
	}
	if head == nil {
		return Decision{}, false
	}
	d := Decision{OperationID: head.ID, Type: head.Type, From: Committed, To: Committed, Target: branch}
	viewHead, exists, err := publicationCheckView(project, branch)
	switch {
	case err != nil:
		return d.block(err, "the integration view cannot be refreshed to the published %s", branch), true
	case exists && viewHead == branch:
		return Decision{}, false
	}
	d = d.decided(ActionRefreshView, "committed operation, integration view at %q: refresh it to the published %s without replay", viewHead, branch)
	if err := RefreshIntegrationView(project); err != nil {
		return d.block(err, "the integration view cannot be refreshed"), true
	}
	d.Applied = true
	return d, true
}

// recoveryCandidateState inspects the candidate worktree of an
// operation at path. It reports whether something exists there and
// whether it is a clean worktree of the canonical repository, detached
// at base with no change, untracked or ignored file; otherwise detail
// says what was found.
func recoveryCandidateState(project worktree.Project, path, base string) (bool, bool, string) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, ""
	}
	if err != nil {
		return true, false, err.Error()
	}
	if !info.IsDir() {
		return true, false, "not a directory"
	}
	common, err := publicationViewGit(path, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel")
	if err != nil {
		return true, false, "not a worktree"
	}
	lines := strings.Split(strings.TrimSpace(common), "\n")
	if len(lines) != 2 || !publicationSamePath(lines[0], project.Repository()) || !publicationSamePath(lines[1], path) {
		return true, false, "not a worktree of the canonical repository"
	}
	if _, err := publicationViewGit(path, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		return true, false, "on a branch"
	}
	head, err := publicationViewGit(path, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(head) != base {
		return true, false, "not at the base " + base
	}
	status, err := publicationViewGit(path, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return true, false, err.Error()
	}
	if status = strings.TrimSpace(status); status != "" {
		return true, false, "changes: " + strings.ReplaceAll(status, "\n", "; ")
	}
	return true, true, ""
}

// recoveryUserBranch returns the commit the published branch of the
// user repository holds, empty when it does not exist. It only reads:
// no hook, file-system monitor or optional lock of the user repository
// is involved.
func recoveryUserBranch(gitDir string) (string, error) {
	cmd := exec.Command("git", "-c", "core.hooksPath="+os.DevNull, "-c", "core.fsmonitor=false", "--no-optional-locks",
		"rev-parse", "--verify", "--quiet", "--end-of-options", IntegrationBranch) // #nosec G204 -- fixed git verb and reference name
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_DIR=" + gitDir,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && strings.TrimSpace(stderr.String()) == "" {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	sha := strings.TrimSpace(string(out))
	if !shaID.MatchString(sha) {
		return "", fmt.Errorf("%w: %s of the user repository is at %q", ErrInvalid, IntegrationBranch, sha)
	}
	return sha, nil
}
