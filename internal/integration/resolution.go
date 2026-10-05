// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goabonga/maestro/internal/provision"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/worktree"
)

// Errors of the conflict resolution.
var (
	// ErrNotResolvable reports an operation a resolution cannot follow:
	// neither a failed integration that conflicted nor a rejected
	// resolution of such a conflict, on the same task and base.
	ErrNotResolvable = errors.New("the operation has no conflict to resolve")
	// ErrResolutionWorktree reports a resolution worktree already present
	// for the operation the resolution follows.
	ErrResolutionWorktree = errors.New("resolution worktree already exists")
	// ErrResolutionNotReproduced reports a conflict that the source
	// commits no longer produce in the resolution worktree.
	ErrResolutionNotReproduced = errors.New("the conflict is not reproduced")
	// ErrResolutionPending reports an operation already followed by a
	// resolution attempt: each attempt follows the latest one.
	ErrResolutionPending = errors.New("a resolution already follows the operation")
	// ErrResolutionsExhausted reports a task whose resolutions on the
	// integration base have been rejected as often as allowed.
	ErrResolutionsExhausted = errors.New("conflict resolutions are exhausted")
	// ErrResolutionTask reports a task that is not in conflict on the
	// integration base of the resolution.
	ErrResolutionTask = errors.New("the task is not in conflict on this base")
	// ErrResolutionIncomplete reports a resolution worktree with
	// uncommitted changes, unresolved paths or no committed resolution.
	ErrResolutionIncomplete = errors.New("the resolution is not committed")
	// ErrResolutionBase reports a resolved commit that does not have the
	// integration base as its only parent.
	ErrResolutionBase = errors.New("the resolution is not a single commit on the integration base")
	// ErrResolutionPath reports a resolved commit touching a path that
	// no source commit of the task touches.
	ErrResolutionPath = errors.New("the resolution touches a path outside the task delta")
)

// resolutionEventReject is the journal event of a rejected resolution.
// It keeps the operation ROLLED_BACK and is what the rejections of an
// integration base are counted by.
const resolutionEventReject = "reject-resolution"

// resolutionMaxChain bounds the walk from a resolution back to its
// conflict: every attempt adds one operation to the chain.
const resolutionMaxChain = 2*task.MaxConflictFailures + 2

// Resolution is a prepared resolution worktree: the source commits of
// the conflicted integration applied on its base in the author worker's
// private repository, stopped on the conflict for the author to resolve.
type Resolution struct {
	// OperationID is the operation the resolution follows: the
	// conflicted integration, or the last rejected resolution.
	OperationID string
	// ConflictOperationID is the integration that conflicted.
	ConflictOperationID string
	// Worker is the author worker whose repository holds the worktree.
	Worker string
	// Path is the resolution worktree.
	Path string
	// Branch is the branch of the worker repository the author commits
	// the resolution on, starting at Base.
	Branch string
	// Base is the integration base of the conflict.
	Base string
	// Applied are the source commits applied cleanly before the conflict.
	Applied []string
	// Conflict is the source commit left conflicting and its paths.
	Conflict CandidateConflict
	// Remaining are the source commits still to apply after Conflict,
	// oldest first: the resolution holds the whole delta.
	Remaining []string
}

// ResolutionBranch returns the branch of the author worker's repository
// that holds the resolution following operation id.
func ResolutionBranch(id string) (string, error) {
	if !operationID.MatchString(id) {
		return "", fmt.Errorf("%w: %q is not an operation id", ErrInvalid, id)
	}
	return "maestro/resolution-" + id, nil
}

// ResolutionWorktree returns the path of the resolution worktree that
// follows operation id, under the storage root of the named worker:
// worktrees/<worker>/resolutions/<id>.
func ResolutionWorktree(project worktree.Project, workerName, id string) (string, error) {
	if !operationID.MatchString(id) {
		return "", fmt.Errorf("%w: %q is not an operation id", ErrInvalid, id)
	}
	worker, ok, err := project.Worker(workerName)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: worker %s has no repository", ErrInvalid, workerName)
	}
	return filepath.Join(project.Dir, "worktrees", worker.Name, "resolutions", id), nil
}

// PrepareResolution provisions the resolution of a conflict in the
// private repository of its author, the worker of the conflicted
// integration. id is the operation the resolution follows: the
// integration whose candidate conflicted, or the last rejected
// resolution of that conflict.
//
// The task must be MERGE_CONFLICT on the operation's integration base
// with resolutions left, and no other attempt may already follow id. A
// failed conflicted integration is first rolled back: the integration
// branch never moved. The integration base and the source commits are
// then copied from the canonical repository into the worker repository,
// a branch is created at the base and checked out in the resolution
// worktree, and the source commits are applied in order with
// cherry-pick --no-commit until the conflict, which is left in the
// worktree for the author. The task branch is never touched.
func (s Store) PrepareResolution(project worktree.Project, id string) (Resolution, error) {
	prev, root, err := s.resolutionFollowed(project, id)
	if err != nil {
		return Resolution{}, err
	}
	worker, ok, err := project.Worker(root.WorkerID)
	if err != nil {
		return Resolution{}, err
	}
	if !ok {
		return Resolution{}, fmt.Errorf("%w: the author %s has no repository", ErrInvalid, root.WorkerID)
	}
	branch, err := ResolutionBranch(prev.ID)
	if err != nil {
		return Resolution{}, err
	}
	path, err := ResolutionWorktree(project, worker.Name, prev.ID)
	if err != nil {
		return Resolution{}, err
	}
	if _, err := os.Lstat(path); err == nil {
		return Resolution{}, fmt.Errorf("%w: %s", ErrResolutionWorktree, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Resolution{}, err
	}
	if prev.State == Failed {
		if _, err := s.RollBackIntegration(project, prev.ID, prev.Error); err != nil {
			return Resolution{}, err
		}
	}

	base, head := root.IntegrationBaseSHA, root.SourceHeadSHA
	if _, err := repoGit(worker.Dir, "", nil, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", project.Repository(),
		base+":refs/heads/"+branch, head+":refs/maestro/resolutions/"+prev.ID+"/source"); err != nil {
		return Resolution{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Resolution{}, err
	}
	if _, err := repoGit(worker.Dir, "", nil, "worktree", "add", "--quiet", path, branch); err != nil {
		return Resolution{}, err
	}
	resolution := Resolution{
		OperationID: prev.ID, ConflictOperationID: root.ID, Worker: worker.Name,
		Path: path, Branch: branch, Base: base,
	}
	for i, commit := range root.SourceCommits {
		_, pickErr := candidateGit(path, "cherry-pick", "--no-commit", "--end-of-options", commit)
		if pickErr == nil {
			resolution.Applied = append(resolution.Applied, commit)
			continue
		}
		paths, err := candidateConflicts(path)
		if err != nil {
			return resolution, errors.Join(pickErr, err)
		}
		if len(paths) == 0 {
			return resolution, pickErr
		}
		resolution.Conflict = CandidateConflict{Commit: commit, Paths: paths}
		resolution.Remaining = append([]string(nil), root.SourceCommits[i+1:]...)
		return resolution, nil
	}
	return resolution, fmt.Errorf("%w: every source commit applies on %s in %s", ErrResolutionNotReproduced, base, path)
}

// ImportResolution imports the resolution the author committed in the
// resolution worktree that follows operation id, verifies it and builds
// its candidate as a new INTEGRATE operation superseding id.
//
// The resolution worktree must be clean and on its branch, which must
// have moved past the base. Its commit is copied into the canonical
// repository under refs/maestro/resolutions/<id>/proposal, then must
// have the integration base as its only parent, change no runtime path
// and touch only paths that the source commits of the conflicted
// integration touch. The new operation, with its own id and attemptID,
// freezes the resolution as a one-commit chain on the base, and the
// task's configuration snapshot; metadata, when nil, is the conflicted
// integration's. BuildCandidate then builds it, leaving it APPLIED for
// the tests and the review of the resolved diff. The task stays
// MERGE_CONFLICT: the proposal is provisional until AcceptResolution.
func (s Store) ImportResolution(project worktree.Project, id, attemptID string, metadata *CommitMetadata, runtime provision.RuntimePaths) (Operation, error) {
	prev, root, err := s.resolutionFollowed(project, id)
	if err != nil {
		return Operation{}, err
	}
	if prev.State != RolledBack {
		return Operation{}, fmt.Errorf("%w: operation %s is %s, not rolled back", ErrNotResolvable, id, prev.State)
	}
	base := root.IntegrationBaseSHA
	branch, err := publicationBranch(project)
	if err != nil {
		return Operation{}, err
	}
	if branch != base {
		return Operation{}, fmt.Errorf("%w: the integration branch is at %s, not at the conflict base %s", ErrStaleIntegrationBase, branch, base)
	}
	proposal, err := resolutionCommitted(project, root.WorkerID, prev.ID, base)
	if err != nil {
		return Operation{}, err
	}
	repository := project.Repository()
	worker, _, err := project.Worker(root.WorkerID)
	if err != nil {
		return Operation{}, err
	}
	if _, err := repoGit(repository, "", nil, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", worker.Dir,
		"+"+proposal+":refs/maestro/resolutions/"+prev.ID+"/proposal"); err != nil {
		return Operation{}, err
	}
	if err := resolutionVerify(repository, root, proposal, runtime); err != nil {
		return Operation{}, err
	}
	current, err := task.Store{DB: s.DB}.Get(root.TaskID)
	if err != nil {
		return Operation{}, err
	}
	if metadata == nil {
		metadata = root.CommitMetadata
	}
	op, err := s.Prepare(Operation{
		Type: Integrate, ProjectID: project.ID, ConfigID: current.ConfigID, TaskID: root.TaskID,
		WorkerID: root.WorkerID, AttemptID: attemptID, SupersedesOperationID: prev.ID,
		TaskBaseSHA: base, IntegrationBaseSHA: base, SourceHeadSHA: proposal,
		SourceCommits: []string{proposal}, CommitMetadata: metadata,
	})
	if err != nil {
		return Operation{}, err
	}
	return s.BuildCandidate(project, op.ID, runtime)
}

// RejectResolution records the rejection of the resolution operation
// id: its build, tests or review failed, or a human refused it. The
// operation is failed with reason and rolled back unless it already is,
// then, in one SQLite transaction, the rejection is journaled on the
// operation and the task goes through its reject-resolution event: it
// stays MERGE_CONFLICT for another proposal after the first rejection on
// an integration base and blocks after the second. A resolution is
// rejected once; nothing is deleted.
func (s Store) RejectResolution(project worktree.Project, id, reason string) (Operation, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Operation{}, fmt.Errorf("%w: a rejection carries its reason", ErrInvalid)
	}
	op, root, err := s.resolutionChain(project, id)
	if err != nil {
		return Operation{}, err
	}
	if op.ID == root.ID {
		return op, fmt.Errorf("%w: operation %s is the conflicted integration, not a resolution", ErrNotResolvable, id)
	}
	if rejected, err := s.resolutionRejected(id); err != nil {
		return op, err
	} else if rejected {
		return op, fmt.Errorf("%w: resolution %s is already rejected", ErrTransition, id)
	}
	tasks := task.Store{DB: s.DB, Now: s.Now}
	reject := task.Input{Event: task.RejectResolution, Base: op.IntegrationBaseSHA, Revision: op.SourceHeadSHA,
		Reason: "resolution " + id + " rejected: " + reason}
	current, err := tasks.Get(op.TaskID)
	if err != nil {
		return op, err
	}
	if current.ConflictBase != op.IntegrationBaseSHA {
		return op, fmt.Errorf("%w: task %s is in conflict on %q, not on %s", ErrResolutionTask, op.TaskID, current.ConflictBase, op.IntegrationBaseSHA)
	}
	if _, err := task.Apply(current, reject, s.now()); err != nil {
		return op, err
	}
	if op.State != RolledBack {
		if op, err = s.RollBackIntegration(project, id, "resolution rejected: "+reason); err != nil {
			return op, err
		}
	}

	at := s.now()
	next := op
	next.Version, next.UpdatedAt = op.Version+1, at
	tx, err := s.DB.Begin()
	if err != nil {
		return op, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE operations SET version = ?, updated_at = ? WHERE id = ? AND state = ? AND version = ?`,
		next.Version, stamp(at), id, string(op.State), op.Version)
	if err != nil {
		return op, fmt.Errorf("reject resolution %s: %w", id, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return op, err
	} else if changed != 1 {
		return op, fmt.Errorf("%w: %s changed concurrently", ErrTransition, id)
	}
	if err := record(tx, Record{OperationID: id, Event: resolutionEventReject, From: op.State, To: op.State, Reason: reason, At: at}); err != nil {
		return op, err
	}
	if _, err := tasks.TransitionTx(tx, op.TaskID, reject); err != nil {
		return op, err
	}
	if err := tx.Commit(); err != nil {
		return op, err
	}
	return next, nil
}

// AcceptResolution returns the task of a tested resolution operation
// id to integration: in one SQLite transaction, the task goes through
// its resolve-conflict event with guard, which carries the validity of
// the proposal, the tests and review of the resolved diff and whether a
// human approval of the candidate is required, then through its
// build-candidate event with the operation's result. The task is then
// VALIDATING that result, which Publish publishes.
func (s Store) AcceptResolution(project worktree.Project, id string, guard task.Guard) (task.Task, error) {
	op, root, err := s.resolutionChain(project, id)
	if err != nil {
		return task.Task{}, err
	}
	if op.ID == root.ID {
		return task.Task{}, fmt.Errorf("%w: operation %s is the conflicted integration, not a resolution", ErrNotResolvable, id)
	}
	if op.State != Tested {
		return task.Task{}, fmt.Errorf("%w: a resolution is accepted in %s, not %s", ErrTransition, Tested, op.State)
	}
	tasks := task.Store{DB: s.DB, Now: s.Now}
	current, err := tasks.Get(op.TaskID)
	if err != nil {
		return task.Task{}, err
	}
	if current.ConflictBase != op.IntegrationBaseSHA {
		return current, fmt.Errorf("%w: task %s is in conflict on %q, not on %s", ErrResolutionTask, op.TaskID, current.ConflictBase, op.IntegrationBaseSHA)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return current, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tasks.TransitionTx(tx, op.TaskID, task.Input{
		Event: task.ResolveConflict, Revision: op.ResultSHA, Base: op.IntegrationBaseSHA, Guard: guard,
		Reason: "resolution " + id + " accepted",
	}); err != nil {
		return current, err
	}
	next, err := tasks.TransitionTx(tx, op.TaskID, task.Input{
		Event: task.BuildCandidate, Revision: op.ResultSHA, Reason: "candidate of resolution " + id,
	})
	if err != nil {
		return current, err
	}
	if err := tx.Commit(); err != nil {
		return current, err
	}
	return next, nil
}

// RejectedResolutions counts, from the journal, the rejected
// resolutions of a task on one integration base.
func (s Store) RejectedResolutions(taskID, base string) (int, error) {
	var count int
	err := s.DB.QueryRow(`SELECT COUNT(DISTINCT o.id) FROM operations o
		JOIN operation_events e ON e.operation_id = o.id
		WHERE o.task_id = ? AND o.integration_base_sha = ? AND e.event = ?`,
		taskID, base, resolutionEventReject).Scan(&count)
	return count, err
}

// resolutionFollowed returns the operation id a new resolution attempt
// follows and the conflicted integration at the root of its chain,
// after checking that the attempt may start: id is the conflicted
// integration, failed or rolled back, or a rejected resolution; no
// other operation follows it yet; the task is MERGE_CONFLICT on the
// base with fewer rejections than the limit, by its counter and by the
// journal.
func (s Store) resolutionFollowed(project worktree.Project, id string) (Operation, Operation, error) {
	prev, root, err := s.resolutionChain(project, id)
	if err != nil {
		return Operation{}, Operation{}, err
	}
	if prev.ID == root.ID {
		if prev.State != Failed && prev.State != RolledBack {
			return prev, root, fmt.Errorf("%w: the conflicted integration %s is %s", ErrNotResolvable, id, prev.State)
		}
	} else if rejected, err := s.resolutionRejected(id); err != nil {
		return prev, root, err
	} else if !rejected {
		return prev, root, fmt.Errorf("%w: resolution %s is not rejected", ErrResolutionPending, id)
	}
	ops, err := s.List(project.ID)
	if err != nil {
		return prev, root, err
	}
	for _, other := range ops {
		if other.SupersedesOperationID == id {
			return prev, root, fmt.Errorf("%w: operation %s follows %s", ErrResolutionPending, other.ID, id)
		}
	}
	current, err := task.Store{DB: s.DB}.Get(root.TaskID)
	if err != nil {
		return prev, root, err
	}
	rejected, err := s.RejectedResolutions(root.TaskID, root.IntegrationBaseSHA)
	if err != nil {
		return prev, root, err
	}
	if current.ConflictBase == root.IntegrationBaseSHA {
		rejected = max(rejected, current.ConflictFailures)
	}
	switch {
	case rejected >= task.MaxConflictFailures:
		return prev, root, fmt.Errorf("%w: %d resolutions of task %s were rejected on %s",
			ErrResolutionsExhausted, rejected, root.TaskID, root.IntegrationBaseSHA)
	case current.State != task.MergeConflict || current.ConflictBase != root.IntegrationBaseSHA:
		return prev, root, fmt.Errorf("%w: task %s is %s on %q, not in conflict on %s",
			ErrResolutionTask, root.TaskID, current.State, current.ConflictBase, root.IntegrationBaseSHA)
	}
	return prev, root, nil
}

// resolutionChain returns operation id and the integration whose
// candidate conflicted at the root of its chain of superseding
// operations. Every operation of the chain is an integration of the
// project, of the same task and on the same integration base.
func (s Store) resolutionChain(project worktree.Project, id string) (Operation, Operation, error) {
	op, err := s.Get(id)
	if err != nil {
		return Operation{}, Operation{}, err
	}
	cur := op
	for range resolutionMaxChain {
		switch {
		case cur.Type != Integrate || cur.ProjectID != project.ID:
			return op, Operation{}, fmt.Errorf("%w: operation %s is not an integration of project %s", ErrNotResolvable, cur.ID, project.ID)
		case cur.TaskID != op.TaskID || cur.IntegrationBaseSHA != op.IntegrationBaseSHA:
			return op, Operation{}, fmt.Errorf("%w: operation %s belongs to another task or base", ErrNotResolvable, cur.ID)
		case strings.HasPrefix(cur.Error, ErrCandidateConflict.Error()+":"):
			return op, cur, nil
		case cur.SupersedesOperationID == "":
			return op, Operation{}, fmt.Errorf("%w: no conflicted integration precedes operation %s", ErrNotResolvable, id)
		}
		if cur, err = s.Get(cur.SupersedesOperationID); err != nil {
			return op, Operation{}, err
		}
	}
	return op, Operation{}, fmt.Errorf("%w: the chain of operation %s is too long", ErrNotResolvable, id)
}

// resolutionRejected reports whether the rejection of operation id is
// journaled.
func (s Store) resolutionRejected(id string) (bool, error) {
	var count int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM operation_events WHERE operation_id = ? AND event = ?`,
		id, resolutionEventReject).Scan(&count)
	return count > 0, err
}

// resolutionCommitted returns the resolution the author committed in
// the resolution worktree following operation id: the worktree must be
// clean, with no unresolved path, and checked out on its branch, which
// must have left base.
func resolutionCommitted(project worktree.Project, workerName, id, base string) (string, error) {
	path, err := ResolutionWorktree(project, workerName, id)
	if err != nil {
		return "", err
	}
	branch, err := ResolutionBranch(id)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(path); err != nil {
		return "", fmt.Errorf("%w: no resolution worktree: %w", ErrResolutionIncomplete, err)
	}
	status, err := candidateGit(path, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) != "" {
		return "", fmt.Errorf("%w: %s has uncommitted changes:\n%s", ErrResolutionIncomplete, path, status)
	}
	ref, err := candidateGit(path, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || ref != "refs/heads/"+branch {
		return "", fmt.Errorf("%w: %s is not on %s", ErrResolutionIncomplete, path, branch)
	}
	head, err := candidateGit(path, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	if !shaID.MatchString(head) {
		return "", fmt.Errorf("%w: %s is at %q", ErrResolutionIncomplete, path, head)
	}
	if head == base {
		return "", fmt.Errorf("%w: %s holds no commit on the base %s", ErrResolutionIncomplete, branch, base)
	}
	return head, nil
}

// resolutionVerify checks, in the canonical repository, the resolution
// of the conflicted integration root: one commit with the integration
// base as its only parent, changing no runtime path and touching only
// paths that a source commit of root touches.
func resolutionVerify(repository string, root Operation, resolution string, runtime provision.RuntimePaths) error {
	base := root.IntegrationBaseSHA
	parents, err := repoGit(repository, "", nil, "rev-list", "--parents", "-n", "1", "--end-of-options", resolution)
	if err != nil {
		return err
	}
	if fields := strings.Fields(parents); len(fields) != 2 || fields[0] != resolution || fields[1] != base {
		return fmt.Errorf("%w: %s has parents %v, expected only %s", ErrResolutionBase, resolution, fields[min(1, len(fields)):], base)
	}
	if _, err := ValidateSourceChain(repository, base, resolution, runtime); err != nil {
		return err
	}
	declared := map[string]bool{}
	for _, commit := range root.SourceCommits {
		paths, err := resolutionPaths(repository, commit+"^", commit)
		if err != nil {
			return err
		}
		for _, path := range paths {
			declared[path] = true
		}
	}
	touched, err := resolutionPaths(repository, base, resolution)
	if err != nil {
		return err
	}
	var outside []string
	for _, path := range touched {
		if !declared[path] {
			outside = append(outside, fmt.Sprintf("%q", path))
		}
	}
	if len(outside) > 0 {
		return fmt.Errorf("%w: %s touches %s", ErrResolutionPath, resolution, strings.Join(outside, ", "))
	}
	return nil
}

// resolutionPaths lists the paths that differ between two commits of
// the repository, without rename detection.
func resolutionPaths(repository, from, to string) ([]string, error) {
	out, err := repoGit(repository, "", nil, "diff-tree", "-r", "--no-renames", "--name-only", "-z", from, to, "--")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, path := range strings.Split(out, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}
