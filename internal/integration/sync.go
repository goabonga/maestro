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

	"github.com/goabonga/maestro/internal/testrun"
	"github.com/goabonga/maestro/internal/worktree"
)

// Errors of a sync.
var (
	// ErrSyncSource reports a source branch that is not a valid branch
	// name or does not exist in the user repository.
	ErrSyncSource = errors.New("invalid sync source")
	// ErrSyncUpToDate reports a source already at the integration head:
	// there is nothing to sync.
	ErrSyncUpToDate = errors.New("the integration is already at the source")
	// ErrSyncDiverged reports a source that does not descend from the
	// integration head. It is never reset or merged implicitly.
	ErrSyncDiverged = errors.New("the source does not descend from the integration head")
	// ErrSyncUntested reports a sync that cannot be tested: no test
	// command is configured or no test runner is available. The journal
	// commits a sync only once it is tested.
	ErrSyncUntested = errors.New("the sync cannot be tested")
	// ErrSyncTestsFailed reports a configured test that did not pass on
	// the imported commit.
	ErrSyncTestsFailed = errors.New("the tests failed on the imported commit")
	// ErrSyncConcurrent reports an integration head moved by someone
	// else during the sync: the compare-and-swap refused to advance it.
	ErrSyncConcurrent = errors.New("the integration head changed during the sync")
)

const (
	// syncIntegrationRef is the integration branch a sync advances.
	syncIntegrationRef = "refs/heads/maestro/integration"
	// syncRefPrefix is the namespace of the references the imported
	// commits land under, one per operation.
	syncRefPrefix = "refs/maestro/sync/"
	// syncWorker is the worker a sync operation is journaled under: the
	// daemon itself, no agent takes part in a sync.
	syncWorker = "daemon"
)

// SyncTester runs test commands against one exact revision of a
// repository, in a new private clone; *testrun.Runner is one.
type SyncTester interface {
	Run(repository, sha, clone string, commands []testrun.Command) (testrun.Run, error)
}

// Syncer imports frozen commits of a project's user repository into its
// canonical repository and advances the integration branch to them.
type Syncer struct {
	// Store journals the SYNC operations; its database also keeps the
	// test reports.
	Store Store
	// Tester runs the configured tests on the imported commit; nil
	// refuses every sync with ErrSyncUntested.
	Tester SyncTester
}

// SyncRequest asks to sync a project from one branch of its user
// repository, under a configuration snapshot and its test commands.
type SyncRequest struct {
	Project  worktree.Project
	Branch   string
	ConfigID string
	Tests    []testrun.Command
}

// SyncPlan is a prepared sync: the SHA the source branch pointed to
// when it was read, frozen for the whole operation, and the integration
// head it advances from.
type SyncPlan struct {
	// Operation is the SYNC operation, journaled in PREPARED.
	Operation   Operation
	Project     worktree.Project
	Branch      string
	PreviousSHA string
	SyncedSHA   string
	// Diagnostics name the uncommitted work of the user repository that
	// the sync leaves out.
	Diagnostics []string
	Tests       []testrun.Command
}

// Prepare freezes the commit the source branch of the user repository
// points to now, checks it, and journals the SYNC operation in
// PREPARED with the integration head it advances from and the frozen
// SHA, before any Git mutation. Only commits are read: uncommitted
// changes, untracked files and the index of the user repository stay
// out and are reported as diagnostics. The source must exist and
// descend from the current integration head; a divergence is refused
// with ErrSyncDiverged and both SHAs, and nothing is journaled.
func (s Syncer) Prepare(request SyncRequest) (SyncPlan, error) {
	if s.Tester == nil {
		return SyncPlan{}, fmt.Errorf("%w: no test runner is available", ErrSyncUntested)
	}
	if len(request.Tests) == 0 {
		return SyncPlan{}, fmt.Errorf("%w: no test command is configured", ErrSyncUntested)
	}
	ref, err := syncBranchRef(request.Branch)
	if err != nil {
		return SyncPlan{}, err
	}
	user := request.Project.UserRepository
	if !filepath.IsAbs(user) {
		return SyncPlan{}, fmt.Errorf("%w: the user repository %q is not an absolute path", ErrInvalid, user)
	}
	synced, err := syncUserGit(user, "", "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	synced = strings.TrimSpace(synced)
	if err != nil || !shaID.MatchString(synced) {
		return SyncPlan{}, fmt.Errorf("%w: branch %s does not exist in the user repository", ErrSyncSource, request.Branch)
	}
	previous, err := syncIntegrationHead(request.Project.Repository())
	if err != nil {
		return SyncPlan{}, err
	}
	if synced == previous {
		return SyncPlan{}, fmt.Errorf("%w: branch %s and the integration are both at %s", ErrSyncUpToDate, request.Branch, synced)
	}
	// A source descending from the integration head reaches it, so the
	// user repository holds it: the check needs no import.
	descends, err := syncIsAncestor(func(args ...string) (string, error) { return syncUserGit(user, "", args...) }, previous, synced)
	if err != nil {
		return SyncPlan{}, err
	}
	if !descends {
		return SyncPlan{}, syncDiverged(request.Branch, synced, previous)
	}
	op, err := s.Store.Prepare(Operation{
		Type: Sync, ProjectID: request.Project.ID, ConfigID: request.ConfigID, WorkerID: syncWorker,
		AttemptID: newID(), IntegrationBaseSHA: previous, SourceHeadSHA: synced,
	})
	if err != nil {
		return SyncPlan{}, err
	}
	return SyncPlan{
		Operation: op, Project: request.Project, Branch: request.Branch, PreviousSHA: previous, SyncedSHA: synced,
		Diagnostics: syncDiagnostics(user), Tests: append([]testrun.Command(nil), request.Tests...),
	}, nil
}

// Run carries a prepared sync to its end. It imports the frozen commit
// from the user repository into the canonical repository under
// refs/maestro/sync/<operation-id>, without hooks and without writing to
// the user repository, checks again that it descends from the previous
// integration head, pins it in the operation's durable reference and
// records it as the result; then it runs the configured tests on that
// commit, keeps their reports, and once they all pass, advances the
// integration branch by compare-and-swap from the previous head and
// commits the operation. A failure before the integration branch moves
// fails and rolls the operation back; the branch is never reset, and the
// imported commit and the test clone stay for diagnosis. Tasks already
// started keep their base.
func (s Syncer) Run(plan SyncPlan) (Operation, error) {
	id := plan.Operation.ID
	op, err := s.Store.Start(id, "sync from branch "+plan.Branch)
	if err != nil {
		return op, err
	}
	if err := s.syncApply(plan); err != nil {
		return s.syncAbort(id, err)
	}
	repository := plan.Project.Repository()
	_, err = repoGit(repository, "", nil, "update-ref", syncIntegrationRef, plan.SyncedSHA, plan.PreviousSHA)
	if err != nil {
		current, readErr := syncIntegrationHead(repository)
		if readErr == nil && current != plan.PreviousSHA {
			err = fmt.Errorf("%w: expected %s, found %s", ErrSyncConcurrent, plan.PreviousSHA, current)
		}
		return s.syncAbort(id, err)
	}
	// The integration branch has moved: the operation is never rolled
	// back from here, only committed.
	return s.Store.Commit(id, Evidence{})
}

// syncApply imports the frozen commit, records it as the result of the
// started operation and marks the operation tested once every test
// passed on it.
func (s Syncer) syncApply(plan SyncPlan) error {
	id, repository := plan.Operation.ID, plan.Project.Repository()
	ref := syncRefPrefix + id
	if _, err := repoGit(repository, "", nil, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head",
		"--no-recurse-submodules", "--no-auto-maintenance", "--", plan.Project.UserRepository,
		plan.SyncedSHA+":"+ref); err != nil {
		return fmt.Errorf("import %s: %w", plan.SyncedSHA, err)
	}
	imported, err := repoGit(repository, "", nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || strings.TrimSpace(imported) != plan.SyncedSHA {
		return fmt.Errorf("%w: %s does not hold %s", ErrInvalidResult, ref, plan.SyncedSHA)
	}
	descends, err := syncIsAncestor(func(args ...string) (string, error) { return repoGit(repository, "", nil, args...) },
		plan.PreviousSHA, plan.SyncedSHA)
	if err != nil {
		return err
	}
	if !descends {
		return syncDiverged(plan.Branch, plan.SyncedSHA, plan.PreviousSHA)
	}
	tree, err := repoGit(repository, "", nil, "rev-parse", "--verify", "--quiet", plan.SyncedSHA+"^{tree}")
	if err != nil {
		return err
	}
	if _, err := s.Store.RecordCandidate(id, ref, strings.TrimSpace(tree)); err != nil {
		return err
	}
	if err := CreateResultRef(repository, id, plan.SyncedSHA); err != nil {
		return err
	}
	op, err := s.Store.RecordResult(id, plan.SyncedSHA)
	if err != nil {
		return err
	}
	reports, err := s.syncTest(plan, op)
	if err != nil {
		return err
	}
	_, err = s.Store.MarkTested(id, Evidence{TestReportIDs: reports})
	return err
}

// syncTest runs the configured tests on the imported commit in a clone
// under the operation's directory, keeps one report per command, and
// returns their ids once every command passed. The clone is removed
// after a success and kept after a failure.
func (s Syncer) syncTest(plan SyncPlan, op Operation) ([]string, error) {
	dir := filepath.Join(plan.Project.Dir, "operations", op.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	clone := filepath.Join(dir, "tests")
	run, err := s.Tester.Run(plan.Project.Repository(), plan.SyncedSHA, clone, plan.Tests)
	if err != nil {
		return nil, fmt.Errorf("run the tests: %w", err)
	}
	stored, err := testrun.Accept(s.Store.DB, run, testrun.Identity{
		ArtifactPrefix: "sync-" + op.ID, TaskID: op.ID, TurnID: op.ID, AttemptID: op.AttemptID,
		WorkerID: op.WorkerID, ConfigID: op.ConfigID,
	})
	if err != nil {
		return nil, fmt.Errorf("keep the test reports: %w", err)
	}
	reports := make([]string, 0, len(stored))
	for _, report := range stored {
		reports = append(reports, report.Envelope.ArtifactID)
	}
	var failed []string
	for _, result := range run.Results {
		if !result.Passed() {
			failed = append(failed, result.Name)
		}
	}
	if len(failed) > 0 || !run.Passed() {
		return nil, fmt.Errorf("%w: %s on %s; reports %s, clone kept at %s", ErrSyncTestsFailed,
			strings.Join(failed, ", "), plan.SyncedSHA, strings.Join(reports, ", "), clone)
	}
	_ = os.RemoveAll(clone)
	return reports, nil
}

// syncAbort fails and rolls back an operation whose integration branch
// has not moved, and returns the cause.
func (s Syncer) syncAbort(id string, cause error) (Operation, error) {
	op, err := s.Store.Fail(id, cause.Error())
	if err != nil {
		return op, errors.Join(cause, err)
	}
	if op, err = s.Store.RollBack(id, "the integration branch was not moved"); err != nil {
		return op, errors.Join(cause, err)
	}
	return op, cause
}

// syncDiverged reports a source that does not descend from the
// integration head, with both SHAs.
func syncDiverged(branch, synced, previous string) error {
	return fmt.Errorf("%w: branch %s at %s does not descend from the integration head %s; "+
		"prepare a branch reconciled with the integration and sync again", ErrSyncDiverged, branch, synced, previous)
}

// syncBranchRef validates a branch name given by the user and returns
// its full reference name.
func syncBranchRef(branch string) (string, error) {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " \t\n\x00") {
		return "", fmt.Errorf("%w: %q is not a branch name", ErrSyncSource, branch)
	}
	ref := "refs/heads/" + branch
	cmd := exec.Command("git", "check-ref-format", ref) // #nosec G204 -- fixed verb; the name was screened for options and whitespace and is only checked
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %q is not a branch name", ErrSyncSource, branch)
	}
	return ref, nil
}

// syncIntegrationHead returns the commit at the head of the integration
// branch of the canonical repository.
func syncIntegrationHead(repository string) (string, error) {
	out, err := repoGit(repository, "", nil, "rev-parse", "--verify", "--quiet", syncIntegrationRef+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("read the integration head: %w", err)
	}
	head := strings.TrimSpace(out)
	if !shaID.MatchString(head) {
		return "", fmt.Errorf("read the integration head: %q is not an object id", head)
	}
	return head, nil
}

// syncIsAncestor reports whether ancestor is reachable from descendant,
// running Git through run. A missing object means it is not.
func syncIsAncestor(run func(...string) (string, error), ancestor, descendant string) (bool, error) {
	_, err := run("merge-base", "--is-ancestor", ancestor, descendant)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	}
	// merge-base exits 128 on an object the repository does not hold:
	// the descendant cannot reach it.
	if _, missing := run("cat-file", "-e", ancestor+"^{commit}"); missing != nil {
		return false, nil
	}
	return false, err
}

// syncDiagnostics reports the uncommitted work of the user repository
// that a sync leaves out: staged changes, unstaged changes and untracked
// files of its main working tree. It only reads the repository. Nothing
// is reported for a repository without a working tree. Content filters
// configured in the user repository are never run: when one is
// configured, unstaged changes are not inspected and the diagnostic says
// so.
func syncDiagnostics(user string) []string {
	if filepath.Base(user) != ".git" {
		return nil
	}
	workTree := filepath.Dir(user)
	git := func(args ...string) (string, error) { return syncUserGit(user, workTree, args...) }
	var diagnostics []string
	notice := func(what string) {
		diagnostics = append(diagnostics, what+" in the user repository are not synced: only commits are")
	}
	if _, err := git("diff-index", "--cached", "--quiet", "HEAD", "--"); err != nil {
		notice("staged changes of the index")
	}
	if filters, _ := git("config", "--get-regexp", `^filter\.`); strings.TrimSpace(filters) != "" {
		diagnostics = append(diagnostics,
			"unstaged changes were not inspected: the user repository configures content filters, which a sync never runs")
	} else if _, err := git("diff-files", "--quiet", "--"); err != nil {
		notice("unstaged changes of the working tree")
	}
	if untracked, err := git("ls-files", "--others", "--exclude-standard", "--directory", "--no-empty-directory", "-z"); err == nil && untracked != "" {
		notice("untracked files")
	}
	return diagnostics
}

// syncUserGit runs one read-only Git command on the user repository,
// with its working tree when one is given, under a controlled
// configuration: no system or global configuration, no hooks, no
// file-system monitor and no optional locks, so nothing is written to
// the repository and none of its executable settings run.
func syncUserGit(repository, workTree string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"--no-optional-locks",
	}, args...)
	cmd := exec.Command("git", full...) // #nosec G204 -- fixed git verbs; revisions are validated object ids and checked reference names
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_DIR=" + repository,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
	}
	if workTree != "" {
		cmd.Env = append(cmd.Env, "GIT_WORK_TREE="+workTree)
		cmd.Dir = workTree
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
