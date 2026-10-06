// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/goabonga/maestro/internal/provision"
	"github.com/goabonga/maestro/internal/worktree"
)

// Errors of the candidate build.
var (
	// ErrCandidateConflict reports a source commit that does not apply
	// cleanly on the integration base. The error is a *CandidateConflict
	// naming the commit and the conflicting paths.
	ErrCandidateConflict = errors.New("candidate conflict")
	// ErrEmptyCandidate reports a candidate whose tree equals the tree of
	// the integration base: the task changes nothing once applied, and a
	// human decides what becomes of it. No commit is built.
	ErrEmptyCandidate = errors.New("empty candidate")
	// ErrStaleIntegrationBase reports an integration branch that moved
	// away from the operation's frozen base: a new base requires a new
	// operation.
	ErrStaleIntegrationBase = errors.New("stale integration base")
	// ErrCandidateWorktree reports a candidate worktree already present
	// for the operation, kept from an earlier attempt.
	ErrCandidateWorktree = errors.New("candidate worktree already exists")
)

// integrationBranch is the private branch holding the last published
// integration in the canonical repository.
const integrationBranch = "refs/heads/maestro/integration"

// CandidateConflict is the error of a source commit that conflicts with
// the integration base. It wraps ErrCandidateConflict.
type CandidateConflict struct {
	// Commit is the source commit whose application conflicted.
	Commit string
	// Paths are the conflicting repository-relative paths, in index order.
	Paths []string
}

// Error names the commit and its conflicting paths.
func (c *CandidateConflict) Error() string {
	quoted := make([]string, len(c.Paths))
	for i, path := range c.Paths {
		quoted[i] = fmt.Sprintf("%q", path)
	}
	return fmt.Sprintf("%s: commit %s conflicts on %s", ErrCandidateConflict, c.Commit, strings.Join(quoted, ", "))
}

// Unwrap returns ErrCandidateConflict.
func (c *CandidateConflict) Unwrap() error {
	return ErrCandidateConflict
}

// CandidateWorktree returns the path of the detached candidate worktree
// of operation id: operations/<id>/candidate under the project's data
// directory.
func CandidateWorktree(project worktree.Project, id string) (string, error) {
	if !operationID.MatchString(id) {
		return "", fmt.Errorf("%w: %q is not an operation id", ErrInvalid, id)
	}
	return filepath.Join(project.Dir, "operations", id, "candidate"), nil
}

// BuildCandidate builds the result of a prepared or started integration
// from the full delta of its task, and leaves the operation APPLIED.
//
// The source chain task_base_sha..source_head_sha is validated again in
// the canonical repository, where its commits must already be present,
// and must be the frozen chain of the operation; the integration branch
// must still be at the frozen integration base. A detached candidate
// worktree is created on that base under operations/<id>/candidate, the
// ordered chain is applied with cherry-pick --no-commit and its tree is
// written and recorded. One commit is then built with the frozen
// metadata and the base as its only parent, its durable result
// reference is created, and only then is APPLIED recorded with the
// result. The integration branch is never moved.
//
// A conflict aborts the application, fails the operation with the
// conflicting paths and returns a *CandidateConflict. A candidate tree
// equal to the base tree fails the operation with ErrEmptyCandidate and
// is never committed. A chain that cannot be integrated, or a base the
// integration branch has left, fails the operation as well. The
// candidate worktree is removed after success and after an empty
// candidate; it is kept after any other failure for diagnosis.
func (s Store) BuildCandidate(project worktree.Project, id string, runtime provision.RuntimePaths) (Operation, error) {
	op, err := s.Get(id)
	if err != nil {
		return Operation{}, err
	}
	switch {
	case op.Type != Integrate:
		return op, fmt.Errorf("%w: operation %s is a %s", ErrInvalid, id, op.Type)
	case op.ProjectID != project.ID:
		return op, fmt.Errorf("%w: operation %s belongs to project %s", ErrInvalid, id, op.ProjectID)
	case op.State != Prepared && op.State != Started:
		return op, fmt.Errorf("%w: a candidate is built in %s or %s, not %s", ErrTransition, Prepared, Started, op.State)
	}
	path, err := CandidateWorktree(project, id)
	if err != nil {
		return op, err
	}
	if _, err := os.Lstat(path); err == nil {
		return op, fmt.Errorf("%w: %s", ErrCandidateWorktree, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return op, err
	}
	repository := project.Repository()

	if err := candidateCheckInputs(repository, op, runtime); err != nil {
		return s.candidateFail(op, err)
	}
	if op.State == Prepared {
		if op, err = s.Start(id, "build candidate"); err != nil {
			return op, err
		}
		crashHook(crashBuildStarted)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return s.candidateFail(op, err)
	}
	if _, err := repoGit(repository, "", nil, "worktree", "add", "--detach", "--quiet", path, op.IntegrationBaseSHA); err != nil {
		return s.candidateFail(op, err)
	}
	crashHook(crashBuildWorktree)
	tree, err := candidateApply(path, op.SourceCommits)
	if err != nil {
		return s.candidateFail(op, err)
	}
	crashHook(crashBuildChain)
	baseTree, err := candidateGit(path, "rev-parse", "--verify", "--end-of-options", op.IntegrationBaseSHA+"^{tree}")
	if err != nil {
		return s.candidateFail(op, err)
	}
	if tree == baseTree {
		failed, err := s.candidateFail(op, fmt.Errorf("%w: the tree %s of the task equals the integration base tree", ErrEmptyCandidate, tree))
		if removeErr := candidateRemove(repository, path, op.IntegrationBaseSHA); removeErr != nil {
			return failed, errors.Join(err, removeErr)
		}
		return failed, err
	}
	if op, err = s.RecordCandidate(id, "", tree); err != nil {
		return s.candidateFail(op, err)
	}
	crashHook(crashBuildRecorded)
	want, err := op.expected()
	if err != nil {
		return s.candidateFail(op, err)
	}
	result, err := BuildResult(repository, want)
	if err != nil {
		return s.candidateFail(op, err)
	}
	crashHook(crashBuildResult)
	if op, err = s.ApplyIntegration(project, id, result); err != nil {
		return s.candidateFail(op, err)
	}
	crashHook(crashBuildApplied)
	if err := candidateRemove(repository, path, result); err != nil {
		return op, fmt.Errorf("remove candidate worktree: %w", err)
	}
	return op, nil
}

// candidateCheckInputs checks, before any Git mutation, that the
// integration base is a commit still at the head of the integration
// branch and that the source chain is valid, present in repository and
// equal to the operation's frozen chain.
func candidateCheckInputs(repository string, op Operation, runtime provision.RuntimePaths) error {
	if err := chainCommitExists(repository, op.IntegrationBaseSHA); err != nil {
		return err
	}
	head, err := repoGit(repository, "", nil, "rev-parse", "--verify", "--quiet", "--end-of-options", integrationBranch)
	if err != nil {
		return fmt.Errorf("read the integration branch: %w", err)
	}
	if head = strings.TrimSpace(head); head != op.IntegrationBaseSHA {
		return fmt.Errorf("%w: the integration branch is at %s, not at the frozen base %s", ErrStaleIntegrationBase, head, op.IntegrationBaseSHA)
	}
	chain, err := ValidateSourceChain(repository, op.TaskBaseSHA, op.SourceHeadSHA, runtime)
	if err != nil {
		return err
	}
	if strings.Join(chain, " ") != strings.Join(op.SourceCommits, " ") {
		return fmt.Errorf("%w: the chain %s..%s is not the frozen chain of the operation", ErrSourceChain, op.TaskBaseSHA, op.SourceHeadSHA)
	}
	return nil
}

// candidateApply applies the ordered commits to the candidate worktree
// at path without committing, and returns the written tree. A conflict
// resets the candidate to its base and returns a *CandidateConflict.
func candidateApply(path string, commits []string) (string, error) {
	for _, commit := range commits {
		if !shaID.MatchString(commit) {
			return "", fmt.Errorf("%w: source commit %q is not an object id", ErrInvalid, commit)
		}
		_, pickErr := candidateGit(path, "cherry-pick", "--no-commit", "--end-of-options", commit)
		if pickErr == nil {
			continue
		}
		paths, err := candidateConflicts(path)
		if err != nil {
			return "", errors.Join(pickErr, err)
		}
		if len(paths) == 0 {
			return "", pickErr
		}
		// cherry-pick --no-commit leaves no sequencer state to abort:
		// the candidate's detached HEAD is still the base, so the index
		// and files are reset to it. No branch is involved.
		if _, err := candidateGit(path, "reset", "--hard", "--quiet", "HEAD"); err != nil {
			return "", errors.Join(&CandidateConflict{Commit: commit, Paths: paths}, err)
		}
		return "", &CandidateConflict{Commit: commit, Paths: paths}
	}
	tree, err := candidateGit(path, "write-tree")
	if err != nil {
		return "", err
	}
	return tree, nil
}

// candidateConflicts returns the unmerged paths of the candidate's
// index, each once, in index order.
func candidateConflicts(path string) ([]string, error) {
	out, err := candidateGit(path, "ls-files", "--unmerged", "-z")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range strings.Split(out, "\x00") {
		_, name, ok := strings.Cut(entry, "\t")
		if ok && (len(paths) == 0 || paths[len(paths)-1] != name) {
			paths = append(paths, name)
		}
	}
	return paths, nil
}

// candidateRemove checks the candidate worktree out at commit, whose
// tree its index already holds, so that nothing unsaved remains, then
// removes it without forcing, prunes its metadata and removes the
// emptied operation directory.
func candidateRemove(repository, path, commit string) error {
	if _, err := candidateGit(path, "checkout", "--detach", "--quiet", commit); err != nil {
		return err
	}
	if _, err := repoGit(repository, "", nil, "worktree", "remove", path); err != nil {
		return err
	}
	if _, err := repoGit(repository, "", nil, "worktree", "prune"); err != nil {
		return err
	}
	if err := os.Remove(filepath.Dir(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// candidateFail records cause as the failure of the operation, unless
// it is already terminal or failed, and returns the operation with
// cause.
func (s Store) candidateFail(op Operation, cause error) (Operation, error) {
	current, err := s.Get(op.ID)
	if err != nil {
		return op, errors.Join(cause, err)
	}
	if current.State.Terminal() || current.State == Failed {
		return current, cause
	}
	failed, err := s.Fail(op.ID, cause.Error())
	if err != nil {
		return current, errors.Join(cause, err)
	}
	return failed, cause
}

// candidateGit runs one Git command in the candidate worktree at path
// with a controlled configuration: no system or global configuration,
// no hooks, no signing and no file-system monitor, and only the search
// path inherited from the daemon. It returns the trimmed output.
func candidateGit(path string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "commit.gpgSign=false",
		"-c", "core.fsmonitor=false",
	}, args...)
	cmd := exec.Command("git", full...) // #nosec G204 -- fixed git verbs; revisions are validated full object ids
	cmd.Dir = path
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(string(out), "\n"), nil
}
