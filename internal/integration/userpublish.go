// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/goabonga/maestro/internal/worktree"
)

// UserPublishRef is the only reference Maestro writes in a user
// repository: the published integration branch.
const UserPublishRef = "refs/heads/maestro/integration"

// canonicalIntegrationRef is the private integration branch of the
// canonical repository.
const canonicalIntegrationRef = "refs/heads/maestro/integration"

// publicationWorker is the worker id journaled for a publication: the
// daemon itself, no agent worker takes part in it.
const publicationWorker = "maestro-svc"

// Errors of the publication to the user repository.
var (
	// ErrPublishUntested reports an integration head that no committed,
	// tested INTEGRATE or SYNC operation produced.
	ErrPublishUntested = errors.New("the integration head is not the result of a committed operation")
	// ErrPublishCheckedOut reports a published branch checked out in a
	// worktree of the user repository.
	ErrPublishCheckedOut = errors.New("the published branch is checked out in the user repository")
	// ErrPublishDiverged reports a published branch the integration head
	// does not descend from.
	ErrPublishDiverged = errors.New("the published branch has diverged from the integration")
	// ErrPublishConcurrent reports a published branch changed by someone
	// else during a publication.
	ErrPublishConcurrent = errors.New("the published branch changed concurrently")
)

// Publication is the outcome of a publication to the user repository.
type Publication struct {
	// Operation is the journaled PUBLISH operation; it is empty when the
	// user repository was already up to date.
	Operation Operation
	// Previous is the commit the published branch held before, empty
	// when the branch was created.
	Previous string
	// Published is the integration head the branch now holds.
	Published string
	// UpToDate reports that the branch already held it.
	UpToDate bool
}

// publicationLocks serializes the publications of each project in the
// daemon, so an unfinished publication is never reconciled while it
// still runs.
var publicationLocks sync.Map

// userPublishBeforeUpdate runs between the checks and the conditional
// update of the published branch; tests use it to change the branch
// concurrently.
var userPublishBeforeUpdate = func() {}

// PublishToUser exports the head of the project's private integration
// branch to the single reference refs/heads/maestro/integration of its
// user repository, never touching the user's current branch, index or
// worktree. The head is frozen when the PUBLISH operation is prepared
// and must be the result of a committed INTEGRATE or SYNC operation,
// whose test reports, reviews and approvals become the evidence of the
// publication. The branch is created when absent and otherwise only
// fast-forwarded: a divergence is refused with both commits, as is a
// branch checked out in any worktree of the user repository. Objects
// are transferred first, without creating any reference or FETCH_HEAD,
// then the branch is updated conditionally on the old value read
// before; a concurrent change is refused. No hook of the user
// repository runs. An unfinished publication of the project left by a
// crash is reconciled first.
func (s Store) PublishToUser(project worktree.Project) (Publication, error) {
	lock, _ := publicationLocks.LoadOrStore(project.ID, &sync.Mutex{})
	mu, _ := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	if err := s.publicationReconcile(project); err != nil {
		return Publication{}, err
	}
	target, err := publicationHead(project)
	if err != nil {
		return Publication{}, err
	}
	ops, err := s.List(project.ID)
	if err != nil {
		return Publication{}, err
	}
	var head *Operation
	for i := range ops {
		if ops[i].State == Committed && ops[i].Type != Publish && ops[i].ResultSHA == target {
			head = &ops[i]
		}
	}
	if head == nil {
		return Publication{}, fmt.Errorf("%w: %s", ErrPublishUntested, target)
	}
	previous, err := userPublishRead(project.UserRepository)
	if err != nil {
		return Publication{}, err
	}
	if previous == target {
		return Publication{Previous: previous, Published: target, UpToDate: true}, nil
	}
	if err := publicationCheck(project, previous, target); err != nil {
		return Publication{}, err
	}

	old := previous
	if old == "" {
		old = strings.Repeat("0", len(target))
	}
	op, err := s.Prepare(Operation{
		Type: Publish, ProjectID: project.ID, ConfigID: head.ConfigID, WorkerID: publicationWorker,
		AttemptID: newID(), IntegrationBaseSHA: old, SourceHeadSHA: target, SourceCommits: []string{target},
	})
	if err != nil {
		return Publication{}, err
	}
	crashHook(crashUserPrepared)
	if op, err = s.Start(op.ID, "publish to "+UserPublishRef+" of "+project.UserRepository); err != nil {
		return Publication{}, err
	}
	crashHook(crashUserStarted)
	if err := userPublishTransfer(project, target); err != nil {
		return Publication{}, s.publicationAbandon(op, err)
	}
	crashHook(crashUserTransferred)
	if err := publicationCheckedOut(project.UserRepository); err != nil {
		return Publication{}, s.publicationAbandon(op, err)
	}
	userPublishBeforeUpdate()
	if _, err := userPublishGit(project.UserRepository, "update-ref", "-m", "maestro publish", UserPublishRef, target, old); err != nil {
		current, readErr := userPublishRead(project.UserRepository)
		if readErr == nil && current != previous {
			err = fmt.Errorf("%w: %s is at %s, expected %s", ErrPublishConcurrent, UserPublishRef, publicationName(current), publicationName(previous))
		}
		return Publication{}, s.publicationAbandon(op, err)
	}
	crashHook(crashUserBranch)
	op, err = s.publicationFinish(project, op)
	if err != nil {
		return Publication{}, err
	}
	return Publication{Operation: op, Previous: previous, Published: target}, nil
}

// publicationHead returns the head of the canonical integration branch.
func publicationHead(project worktree.Project) (string, error) {
	out, err := repoGit(project.Repository(), "", nil, "rev-parse", "--verify", canonicalIntegrationRef+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("read the integration head of project %s: %w", project.ID, err)
	}
	return strings.TrimSpace(out), nil
}

// publicationCheck refuses a publication of target over previous that
// would not be a creation or a fast-forward, or whose branch is checked
// out in the user repository.
func publicationCheck(project worktree.Project, previous, target string) error {
	if previous != "" {
		_, missing := repoGit(project.Repository(), "", nil, "cat-file", "-e", previous+"^{commit}")
		if missing != nil {
			return fmt.Errorf("%w: %s is at %s, unknown to the integration at %s", ErrPublishDiverged, UserPublishRef, previous, target)
		}
		if _, err := repoGit(project.Repository(), "", nil, "merge-base", "--is-ancestor", previous, target); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return fmt.Errorf("%w: %s is at %s, which the integration at %s does not descend from", ErrPublishDiverged, UserPublishRef, previous, target)
			}
			return err
		}
	}
	return publicationCheckedOut(project.UserRepository)
}

// publicationCheckedOut refuses a published branch checked out in any
// worktree of the user repository, the main one included.
func publicationCheckedOut(gitDir string) error {
	out, err := userPublishGit(gitDir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return err
	}
	path := ""
	for _, field := range strings.Split(out, "\x00") {
		if value, ok := strings.CutPrefix(field, "worktree "); ok {
			path = value
		}
		if field == "branch "+UserPublishRef {
			return fmt.Errorf("%w: %s is checked out at %s", ErrPublishCheckedOut, UserPublishRef, path)
		}
	}
	return nil
}

// publicationAbandon fails and rolls back a publication whose branch
// was not moved, and returns cause.
func (s Store) publicationAbandon(op Operation, cause error) error {
	if err := s.publicationRollBack(op, cause.Error()); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// publicationRollBack journals a publication whose branch was not
// moved as FAILED with its cause, then ROLLED_BACK.
func (s Store) publicationRollBack(op Operation, cause string) error {
	if op.State != Failed {
		if _, err := s.Fail(op.ID, cause); err != nil {
			return err
		}
		crashHook(crashUserFailed)
	}
	_, err := s.RollBack(op.ID, UserPublishRef+" left untouched")
	return err
}

// publicationFinish records the result of a publication whose branch
// holds its target, then commits it with the evidence of the committed
// operations it publishes.
func (s Store) publicationFinish(project worktree.Project, op Operation) (Operation, error) {
	var err error
	if op.State == Started {
		if op, err = s.RecordResult(op.ID, op.SourceHeadSHA); err != nil {
			return op, err
		}
		crashHook(crashUserApplied)
	}
	evidence, err := s.publicationEvidence(project, op)
	if err != nil {
		return op, err
	}
	return s.Commit(op.ID, evidence)
}

// publicationEvidence gathers the test reports, reviews and approvals
// of the committed INTEGRATE and SYNC operations whose result the
// publication brings to the user repository: the commits reachable from
// its target and not from the branch's previous value.
func (s Store) publicationEvidence(project worktree.Project, op Operation) (Evidence, error) {
	args := []string{"rev-list", op.SourceHeadSHA}
	if strings.Trim(op.IntegrationBaseSHA, "0") != "" {
		args = append(args, "^"+op.IntegrationBaseSHA)
	}
	out, err := repoGit(project.Repository(), "", nil, append(args, "--")...)
	if err != nil {
		return Evidence{}, err
	}
	published := map[string]bool{}
	for _, commit := range strings.Fields(out) {
		published[commit] = true
	}
	ops, err := s.List(project.ID)
	if err != nil {
		return Evidence{}, err
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

// publicationReconcile settles the unfinished publications of a project
// against its user repository: a branch at the recorded target means
// the update happened and the operation is finalized; a branch still at
// the recorded old value means it did not and the operation is rolled
// back; any other value blocks the publication without rewriting
// anything.
func (s Store) publicationReconcile(project worktree.Project) error {
	ops, err := s.List(project.ID)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.Type != Publish || op.State.Terminal() {
			continue
		}
		if op.State == Applied {
			if _, err := s.publicationFinish(project, op); err != nil {
				return err
			}
			continue
		}
		current, err := userPublishRead(project.UserRepository)
		if err != nil {
			return err
		}
		old := op.IntegrationBaseSHA
		if strings.Trim(old, "0") == "" {
			old = ""
		}
		switch {
		case op.State == Failed:
			if err := s.publicationRollBack(op, op.Error); err != nil {
				return err
			}
		case op.State == Prepared:
			if err := s.publicationRollBack(op, "publication interrupted before the transfer"); err != nil {
				return err
			}
		case current == op.SourceHeadSHA:
			if _, err := s.publicationFinish(project, op); err != nil {
				return err
			}
		case current == old:
			if err := s.publicationRollBack(op, "publication interrupted before the reference update"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unfinished publication %s expected %s at %s or %s, found %s",
				ErrPublishConcurrent, op.ID, UserPublishRef, publicationName(old), op.SourceHeadSHA, publicationName(current))
		}
	}
	return nil
}

// publicationName renders a branch value, absent included.
func publicationName(sha string) string {
	if sha == "" {
		return "nothing"
	}
	return sha
}

// userPublishRead returns the commit the published branch holds in the
// user repository, empty when it does not exist.
func userPublishRead(gitDir string) (string, error) {
	out, err := userPublishGit(gitDir, "rev-parse", "--verify", "--quiet", UserPublishRef)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// userPublishTransfer copies into the user repository the objects of
// target from the canonical repository, without creating any reference
// or FETCH_HEAD and without running automatic maintenance.
func userPublishTransfer(project worktree.Project, target string) error {
	_, err := userPublishGit(project.UserRepository, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head",
		"--no-recurse-submodules", "--no-auto-maintenance", "--refmap=",
		project.Repository(), target)
	if err != nil {
		return fmt.Errorf("transfer %s to the user repository: %w", target, err)
	}
	return nil
}

// userPublishGit runs one Git command against the user repository's
// Git directory with a controlled configuration: no system or global
// configuration, no hooks, no file system monitor, no signing, and
// only the local file transport. Arguments are fixed verbs, validated
// object ids and paths owned by Maestro, never task text.
func userPublishGit(gitDir string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "commit.gpgSign=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.file.allow=always",
		"-c", "uploadpack.allowReachableSHA1InWant=true",
	}, args...)
	cmd := exec.Command("git", full...) // #nosec G204 -- fixed git verbs; object ids are validated and paths belong to the project
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_DIR=" + gitDir,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
