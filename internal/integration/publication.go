// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/goabonga/maestro/internal/handoff"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/testrun"
	"github.com/goabonga/maestro/internal/worktree"
)

// IntegrationBranch is the reference of the canonical repository that
// designates the last published integration.
const IntegrationBranch = "refs/heads/maestro/integration"

// Errors of the publication.
var (
	// ErrTestsFailed reports test reports of which at least one failed:
	// the operation is rolled back and nothing is published.
	ErrTestsFailed = errors.New("integration tests failed")
	// ErrBranchMoved reports an integration branch that is no longer at
	// the operation's integration base: the publication is refused and
	// the operation is not committed.
	ErrBranchMoved = errors.New("the integration branch moved")
	// ErrPublished reports a rollback refused because the integration
	// branch already holds the operation's result.
	ErrPublished = errors.New("the operation result is published")
	// ErrIntegrationView reports an unexplained change in the detached
	// integration view, which forbids publishing and refreshing it.
	ErrIntegrationView = errors.New("unexplained change in the integration view")
)

// IntegrationView returns the path of the project's detached view of
// the published integration: worktrees/integration under the project
// directory. Only the daemon writes there.
func IntegrationView(project worktree.Project) string {
	return filepath.Join(project.Dir, "worktrees", "integration")
}

// RecordTestReports attaches the accepted TEST_REPORT artifacts
// reportIDs to an applied integration. Every report must belong to the
// operation's task, have been produced under the configuration snapshot
// the task runs on, which must be the operation's own, and have tested
// the operation's result_sha exactly; otherwise nothing changes. When
// every report passed, the operation moves to TESTED with the reports as
// evidence. When one failed, the operation moves to FAILED with the
// failing reports named in its error, then to ROLLED_BACK once the
// integration branch is confirmed not to hold the result, and the call
// returns ErrTestsFailed: the reports, the result reference and the
// candidate are kept for diagnosis.
func (s Store) RecordTestReports(project worktree.Project, id string, reportIDs []string) (Operation, error) {
	op, err := s.Get(id)
	if err != nil {
		return Operation{}, err
	}
	if op.Type != Integrate {
		return op, fmt.Errorf("%w: operation %s is a %s, not an integration", ErrInvalid, id, op.Type)
	}
	if op.State != Applied {
		return op, fmt.Errorf("%w: test reports are recorded in %s, not %s", ErrTransition, Applied, op.State)
	}
	if len(reportIDs) == 0 {
		return op, fmt.Errorf("%w: no test report", ErrGuard)
	}
	current, err := task.Store{DB: s.DB}.Get(op.TaskID)
	if err != nil {
		return op, err
	}
	if current.ConfigID != op.ConfigID {
		return op, fmt.Errorf("%w: operation %s was prepared under %s, task %s runs on %s",
			handoff.ErrConfig, id, op.ConfigID, op.TaskID, current.ConfigID)
	}
	var failed []string
	for _, reportID := range reportIDs {
		stored, err := handoff.Load(s.DB, reportID)
		if errors.Is(err, sql.ErrNoRows) {
			return op, fmt.Errorf("%w: test report %s is not accepted", ErrGuard, reportID)
		}
		if err != nil {
			return op, err
		}
		payload, err := testrun.ForTask(stored.Document, op.TaskID, current.ConfigID, op.ResultSHA)
		if err != nil {
			return op, fmt.Errorf("%w: test report %s: %w", ErrGuard, reportID, err)
		}
		if !testrun.Passed(payload) {
			failed = append(failed, reportID)
		}
	}
	if len(failed) == 0 {
		return s.MarkTested(id, Evidence{TestReportIDs: reportIDs})
	}
	cause := fmt.Sprintf("tests failed on %s: %s", op.ResultSHA, strings.Join(failed, ", "))
	rolledBack, err := s.RollBackIntegration(project, id, cause)
	if err != nil {
		return rolledBack, err
	}
	return rolledBack, fmt.Errorf("%w: %s", ErrTestsFailed, cause)
}

// RollBackIntegration abandons an unfinished integration: it moves the
// operation to FAILED with cause, unless it already failed, then to
// ROLLED_BACK once the integration branch of the canonical repository
// is confirmed not to hold the operation's result. The branch is never
// reset: a failed integration has not moved it. A branch at the result
// is refused with ErrPublished and leaves the operation FAILED for
// recovery. Nothing is deleted: the result reference, the candidate and
// the diagnostics stay available.
func (s Store) RollBackIntegration(project worktree.Project, id, cause string) (Operation, error) {
	op, err := s.Get(id)
	if err != nil {
		return Operation{}, err
	}
	if op.Type != Integrate {
		return op, fmt.Errorf("%w: operation %s is a %s, not an integration", ErrInvalid, id, op.Type)
	}
	if op.State != Failed {
		if op, err = s.Fail(id, cause); err != nil {
			return op, err
		}
		crashHook(crashRollBackFailed)
	}
	branch, err := publicationBranch(project)
	if err != nil {
		return op, err
	}
	if op.ResultSHA != "" && branch == op.ResultSHA {
		return op, fmt.Errorf("%w: %s is at the result %s of operation %s", ErrPublished, IntegrationBranch, branch, id)
	}
	return s.RollBack(id, fmt.Sprintf("%s at %s, result %q not published", IntegrationBranch, branch, op.ResultSHA))
}

// Publish publishes a tested integration and finalizes it.
//
// Before any Git mutation it proves the result from its durable
// reference, checks that the task is VALIDATING that result on the
// operation's configuration snapshot, and that the integration view
// holds no unexplained change. It then moves the integration branch of
// the canonical repository from the operation's integration base to its
// result with one compare-and-swap update-ref: a branch moved meanwhile
// is refused with ErrBranchMoved and the operation stays TESTED. Once
// the branch holds the result, the operation becomes COMMITTED and the
// task DONE in one SQLite transaction. Finally the integration view is
// refreshed to the published commit; a refresh error is returned with
// the committed operation.
//
// A crash between the update-ref and the SQLite transaction leaves the
// branch at the result, the durable result reference valid and the
// operation TESTED. Calling Publish again recognizes this state from the
// durable reference: it skips the update-ref and only finalizes.
func (s Store) Publish(project worktree.Project, id string) (Operation, error) {
	op, err := s.Get(id)
	if err != nil {
		return Operation{}, err
	}
	if op.Type != Integrate {
		return op, fmt.Errorf("%w: operation %s is a %s, not an integration", ErrInvalid, id, op.Type)
	}
	if op.State != Tested {
		return op, fmt.Errorf("%w: an integration is published from %s, not %s", ErrTransition, Tested, op.State)
	}
	proven, ok, err := ProveResult(project, op)
	switch {
	case err != nil:
		return op, err
	case !ok || proven != op.ResultSHA:
		return op, fmt.Errorf("%w: the result reference of %s does not hold %s", ErrInvalidResult, id, op.ResultSHA)
	}
	tasks := task.Store{DB: s.DB, Now: s.Now}
	current, err := tasks.Get(op.TaskID)
	if err != nil {
		return op, err
	}
	if current.ConfigID != op.ConfigID {
		return op, fmt.Errorf("%w: operation %s was prepared under %s, task %s runs on %s",
			handoff.ErrConfig, id, op.ConfigID, op.TaskID, current.ConfigID)
	}
	pass := task.Input{
		Event: task.ValidationPass, Revision: op.ResultSHA,
		Guard: task.Guard{Published: true}, Reason: "operation " + id + " published",
	}
	if _, err := task.Apply(current, pass, s.now()); err != nil {
		return op, err
	}

	branch, err := publicationBranch(project)
	if err != nil {
		return op, err
	}
	switch branch {
	case op.ResultSHA:
		// Published before a crash: only the finalization is missing.
	case op.IntegrationBaseSHA:
		if _, _, err := publicationCheckView(project, branch); err != nil {
			return op, err
		}
		if _, err := repoGit(project.Repository(), "", nil, "update-ref", IntegrationBranch, op.ResultSHA, op.IntegrationBaseSHA); err != nil {
			if now, readErr := publicationBranch(project); readErr != nil || now != op.ResultSHA {
				return op, fmt.Errorf("%w: %s is no longer at %s: %w", ErrBranchMoved, IntegrationBranch, op.IntegrationBaseSHA, err)
			}
		}
	default:
		return op, fmt.Errorf("%w: %s is at %s, the integration base of %s is %s",
			ErrBranchMoved, IntegrationBranch, branch, id, op.IntegrationBaseSHA)
	}

	crashHook(crashPublishBranch)
	committed, err := s.publicationFinalize(tasks, op, pass)
	if err != nil {
		return op, err
	}
	crashHook(crashPublishCommitted)
	if err := RefreshIntegrationView(project); err != nil {
		return committed, fmt.Errorf("operation %s committed: %w", id, err)
	}
	return committed, nil
}

// publicationFinalize moves a tested operation to COMMITTED and its
// task through pass to DONE in one SQLite transaction: both happen, or
// neither does. The operation must still be at the state and version
// that were read.
func (s Store) publicationFinalize(tasks task.Store, op Operation, pass task.Input) (Operation, error) {
	if !Allowed(op.Type, op.State, Committed) {
		return op, fmt.Errorf("%w: %s %s from %s to %s", ErrTransition, op.Type, op.ID, op.State, Committed)
	}
	if len(op.TestReportIDs) == 0 {
		return op, fmt.Errorf("%w: no test report", ErrGuard)
	}
	at := s.now()
	next := op
	next.State, next.Version, next.UpdatedAt = Committed, op.Version+1, at
	tx, err := s.DB.Begin()
	if err != nil {
		return op, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE operations SET state = ?, version = ?, updated_at = ?
		WHERE id = ? AND state = ? AND version = ?`,
		string(next.State), next.Version, stamp(at), op.ID, string(op.State), op.Version)
	if err != nil {
		return op, fmt.Errorf("commit operation %s: %w", op.ID, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return op, err
	} else if changed != 1 {
		return op, fmt.Errorf("%w: %s changed concurrently", ErrTransition, op.ID)
	}
	if err := record(tx, Record{
		OperationID: op.ID, Event: eventCommit, From: op.State, To: Committed,
		Reason: "published " + op.ResultSHA, At: at,
	}); err != nil {
		return op, err
	}
	if _, err := tasks.TransitionTx(tx, op.TaskID, pass); err != nil {
		return op, err
	}
	if err := tx.Commit(); err != nil {
		return op, err
	}
	return next, nil
}

// RefreshIntegrationView brings the detached integration view to the
// commit the integration branch designates, creating the view when it
// does not exist. An existing view must be a clean worktree of the
// canonical repository, detached at the published commit or one of its
// ancestors; any other state is an unexplained change, refused with
// ErrIntegrationView and left untouched.
func RefreshIntegrationView(project worktree.Project) error {
	target, err := publicationBranch(project)
	if err != nil {
		return err
	}
	view := IntegrationView(project)
	head, exists, err := publicationCheckView(project, target)
	if err != nil {
		return err
	}
	if !exists {
		if err := os.MkdirAll(filepath.Dir(view), 0o700); err != nil {
			return err
		}
		if _, err := repoGit(project.Repository(), "", nil, "worktree", "add", "--quiet", "--detach", view, target); err != nil {
			return err
		}
	} else if head != target {
		if _, err := publicationViewGit(view, "checkout", "--quiet", "--detach", target); err != nil {
			return err
		}
	}
	if head, _, err = publicationCheckView(project, target); err != nil {
		return err
	}
	if head != target {
		return fmt.Errorf("%w: the view is at %s after its refresh to %s", ErrIntegrationView, head, target)
	}
	return nil
}

// publicationCheckView inspects the integration view and returns its
// HEAD, or false when the view does not exist. An existing view must be
// a directory holding the top of a worktree of the canonical
// repository, with a detached HEAD at published or one of its
// ancestors, no change to tracked files or the index and no untracked
// or ignored file; otherwise it fails with ErrIntegrationView.
func publicationCheckView(project worktree.Project, published string) (string, bool, error) {
	view := IntegrationView(project)
	info, err := os.Lstat(view)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.IsDir() {
		return "", true, fmt.Errorf("%w: %s is not a directory", ErrIntegrationView, view)
	}
	common, err := publicationViewGit(view, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel")
	if err != nil {
		return "", true, fmt.Errorf("%w: %s is not a worktree: %w", ErrIntegrationView, view, err)
	}
	lines := strings.Split(strings.TrimSpace(common), "\n")
	if len(lines) != 2 || !publicationSamePath(lines[0], project.Repository()) || !publicationSamePath(lines[1], view) {
		return "", true, fmt.Errorf("%w: %s is not a worktree of the canonical repository", ErrIntegrationView, view)
	}
	if _, err := publicationViewGit(view, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		return "", true, fmt.Errorf("%w: %s is on a branch, not detached", ErrIntegrationView, view)
	}
	out, err := publicationViewGit(view, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return "", true, fmt.Errorf("%w: %s has no commit checked out: %w", ErrIntegrationView, view, err)
	}
	head := strings.TrimSpace(out)
	if !shaID.MatchString(head) {
		return "", true, fmt.Errorf("%w: %s is at %q", ErrIntegrationView, view, head)
	}
	status, err := publicationViewGit(view, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return "", true, err
	}
	if status = strings.TrimSpace(status); status != "" {
		return "", true, fmt.Errorf("%w: %s has changes:\n%s", ErrIntegrationView, view, status)
	}
	if head != published {
		if _, err := repoGit(project.Repository(), "", nil, "merge-base", "--is-ancestor", head, published); err != nil {
			return "", true, fmt.Errorf("%w: %s is at %s, which is not an ancestor of the published %s",
				ErrIntegrationView, view, head, published)
		}
	}
	return head, true, nil
}

// publicationSamePath reports whether two paths name the same location
// once symbolic links are resolved.
func publicationSamePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// publicationBranch returns the commit the integration branch of the
// canonical repository points at.
func publicationBranch(project worktree.Project) (string, error) {
	out, err := repoGit(project.Repository(), "", nil, "for-each-ref", "--format=%(objectname)", IntegrationBranch)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if !shaID.MatchString(sha) {
		return "", fmt.Errorf("%w: the canonical repository has no %s", ErrInvalid, IntegrationBranch)
	}
	return sha, nil
}

// publicationViewGit runs one Git command in the integration view with
// the controlled configuration of repoGit: no system or global
// configuration, no hooks, no signing. Repository discovery stops at
// the view, so a missing view never reaches an enclosing repository.
func publicationViewGit(view string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "commit.gpgSign=false",
	}, args...)
	cmd := exec.Command("git", full...) // #nosec G204 -- fixed git verbs and validated object ids built by Maestro
	cmd.Dir = view
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_CEILING_DIRECTORIES=" + filepath.Dir(view),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
