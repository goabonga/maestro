// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/provision"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/worktree"
)

// resolutionFixture is a conflicted integration of the task t1: its
// second source commit conflicts on base.txt, the task is
// MERGE_CONFLICT on the integration base and the author w1 has a
// private repository.
type resolutionFixture struct {
	candidateFixture
	store    Store
	conflict Operation
}

// newResolutionFixture builds the conflicted integration.
func newResolutionFixture(t *testing.T) resolutionFixture {
	t.Helper()
	f := candidateRepository(t, "base\n",
		[2]string{"notes.txt", "one\n"},
		[2]string{"base.txt", "task\n"},
		[2]string{"extra.txt", "extra\n"})
	if _, _, err := (worktree.Store{Base: t.TempDir()}).AddWorker(f.project, "w1"); err != nil {
		t.Fatal(err)
	}
	store := resolutionJournal(t)
	op := candidateOperation(t, store, f)
	failed, err := store.BuildCandidate(f.project, op.ID, candidateRuntime)
	if !errors.Is(err, ErrCandidateConflict) {
		t.Fatalf("err=%v", err)
	}
	if _, err := (task.Store{DB: store.DB}).Transition("t1", task.Input{Event: task.Conflict, Base: f.base, Reason: "conflict"}); err != nil {
		t.Fatal(err)
	}
	return resolutionFixture{candidateFixture: f, store: store, conflict: failed}
}

// resolutionJournal returns the journal of openJournal with the task
// t1 dated, so that it can go through transitions.
func resolutionJournal(t *testing.T) Store {
	t.Helper()
	store := openJournal(t)
	at := stamp(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if _, err := store.DB.Exec(`UPDATE tasks SET created_at = ?, updated_at = ? WHERE task_id = 't1'`, at, at); err != nil {
		t.Fatal(err)
	}
	return store
}

// resolve plays the author: it resolves base.txt with content in the
// resolution worktree, applies the remaining source commits, then
// commits the whole resolution.
func resolve(t *testing.T, r Resolution, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.Path, "base.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, r.Path, false, "", "add", "base.txt")
	for _, commit := range r.Remaining {
		gitIn(t, r.Path, false, "", "cherry-pick", "--no-commit", commit)
	}
	gitIn(t, r.Path, false, "", "commit", "-q", "-m", "resolve the conflict")
}

// resolutionTask returns the stored task t1.
func resolutionTask(t *testing.T, store Store) task.Task {
	t.Helper()
	current, err := task.Store{DB: store.DB}.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func TestPrepareResolutionReproducesTheConflictInTheAuthorRepository(t *testing.T) {
	f := newResolutionFixture(t)

	r, err := f.store.PrepareResolution(f.project, f.conflict.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(f.project.Dir, "worktrees", "w1", "resolutions", f.conflict.ID)
	if r.Path != wantPath || r.Worker != "w1" || r.Base != f.base || r.OperationID != f.conflict.ID || r.ConflictOperationID != f.conflict.ID {
		t.Fatalf("resolution %+v", r)
	}
	if strings.Join(r.Applied, " ") != f.chain[0] || r.Conflict.Commit != f.chain[1] ||
		strings.Join(r.Conflict.Paths, ",") != "base.txt" || strings.Join(r.Remaining, " ") != f.chain[2] {
		t.Fatalf("resolution %+v", r)
	}
	if head := gitIn(t, r.Path, false, "", "rev-parse", "HEAD"); head != f.base {
		t.Fatalf("the resolution worktree is at %s", head)
	}
	if branch := gitIn(t, r.Path, false, "", "symbolic-ref", "HEAD"); branch != "refs/heads/"+r.Branch {
		t.Fatalf("branch %s", branch)
	}
	content, err := os.ReadFile(filepath.Join(r.Path, "base.txt"))
	if err != nil || !strings.Contains(string(content), "<<<<<<<") {
		t.Fatalf("base.txt %q, %v", content, err)
	}
	if unmerged := gitIn(t, r.Path, false, "", "diff", "--name-only", "--diff-filter=U"); unmerged != "base.txt" {
		t.Fatalf("unmerged %q", unmerged)
	}
	if staged := gitIn(t, r.Path, false, "", "diff", "--cached", "--name-only", "--diff-filter=A"); staged != "notes.txt" {
		t.Fatalf("staged %q", staged)
	}
	// The worktree belongs to the author's private repository, not to
	// the canonical one.
	common := gitIn(t, r.Path, false, "", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if !publicationSamePath(common, f.project.WorkerRepository("w1")) {
		t.Fatalf("common dir %s", common)
	}
	if stored, _ := f.store.Get(f.conflict.ID); stored.State != RolledBack {
		t.Fatalf("conflicted operation %s", stored.State)
	}
	if head := candidateIntegrationHead(t, f.candidateFixture); head != f.base {
		t.Fatalf("the integration branch moved to %s", head)
	}
	if _, err := f.store.PrepareResolution(f.project, f.conflict.ID); !errors.Is(err, ErrResolutionWorktree) {
		t.Fatalf("a second worktree was prepared: %v", err)
	}
}

func TestImportResolutionBuildsANewCandidateOperation(t *testing.T) {
	f := newResolutionFixture(t)
	r, err := f.store.PrepareResolution(f.project, f.conflict.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ImportResolution(f.project, f.conflict.ID, "a2", nil, candidateRuntime); !errors.Is(err, ErrResolutionIncomplete) {
		t.Fatalf("an unresolved conflict was imported: %v", err)
	}
	resolve(t, r, "base and task\n")
	resolved := gitIn(t, r.Path, false, "", "rev-parse", "HEAD")

	op, err := f.store.ImportResolution(f.project, f.conflict.ID, "a2", nil, candidateRuntime)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case op.ID == f.conflict.ID || op.SupersedesOperationID != f.conflict.ID || op.AttemptID != "a2":
		t.Fatalf("operation %+v", op)
	case op.State != Applied || op.TaskBaseSHA != f.base || op.IntegrationBaseSHA != f.base:
		t.Fatalf("operation %+v", op)
	case op.SourceHeadSHA != resolved || strings.Join(op.SourceCommits, " ") != resolved:
		t.Fatalf("operation %+v", op)
	case op.CommitMetadata == nil || op.CommitMetadata.Message != f.conflict.CommitMetadata.Message:
		t.Fatalf("metadata %+v", op.CommitMetadata)
	}
	repo := f.project.Repository()
	if parents := gitIn(t, repo, true, "", "rev-list", "--parents", "-n", "1", op.ResultSHA); parents != op.ResultSHA+" "+f.base {
		t.Fatalf("parents %q", parents)
	}
	if tree := gitIn(t, repo, true, "", "rev-parse", resolved+"^{tree}"); tree != op.CandidateTreeSHA {
		t.Fatalf("tree %s, resolution tree %s", op.CandidateTreeSHA, tree)
	}
	if content := gitIn(t, repo, true, "", "show", op.ResultSHA+":base.txt"); content != "base and task" {
		t.Fatalf("base.txt %q", content)
	}
	if proposal := gitIn(t, repo, true, "", "rev-parse", "refs/maestro/resolutions/"+f.conflict.ID+"/proposal"); proposal != resolved {
		t.Fatalf("proposal %s", proposal)
	}
	if sha, ok, err := ProveResult(f.project, op); !ok || err != nil || sha != op.ResultSHA {
		t.Fatalf("proof sha=%s ok=%v err=%v", sha, ok, err)
	}
	if head := candidateIntegrationHead(t, f.candidateFixture); head != f.base {
		t.Fatalf("the integration branch moved to %s", head)
	}
	if state := resolutionTask(t, f.store).State; state != task.MergeConflict {
		t.Fatalf("task %s", state)
	}
	if _, err := f.store.ImportResolution(f.project, f.conflict.ID, "a3", nil, candidateRuntime); !errors.Is(err, ErrResolutionPending) {
		t.Fatalf("a second attempt followed the same operation: %v", err)
	}
	if _, err := f.store.PrepareResolution(f.project, op.ID); !errors.Is(err, ErrResolutionPending) {
		t.Fatalf("a pending resolution was followed: %v", err)
	}
}

func TestImportResolutionVerifiesBaseAndPaths(t *testing.T) {
	for name, tc := range map[string]struct {
		author func(t *testing.T, r Resolution)
		want   error
	}{
		"two commits": {func(t *testing.T, r Resolution) {
			resolve(t, r, "resolved\n")
			if err := os.WriteFile(filepath.Join(r.Path, "notes.txt"), []byte("two\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitIn(t, r.Path, false, "", "commit", "-q", "-am", "more")
		}, ErrResolutionBase},
		"undeclared path": {func(t *testing.T, r Resolution) {
			if err := os.WriteFile(filepath.Join(r.Path, "README.md"), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitIn(t, r.Path, false, "", "add", "README.md")
			resolve(t, r, "resolved\n")
		}, ErrResolutionPath},
		"runtime path": {func(t *testing.T, r Resolution) {
			if err := os.WriteFile(filepath.Join(r.Path, "CLAUDE.md"), []byte("runtime\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitIn(t, r.Path, false, "", "add", "-f", "CLAUDE.md")
			resolve(t, r, "resolved\n")
		}, provision.ErrRuntimePath},
		"dirty worktree": {func(t *testing.T, r Resolution) {
			resolve(t, r, "resolved\n")
			if err := os.WriteFile(filepath.Join(r.Path, "notes.txt"), []byte("dirty\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, ErrResolutionIncomplete},
		"detached head": {func(t *testing.T, r Resolution) {
			resolve(t, r, "resolved\n")
			gitIn(t, r.Path, false, "", "checkout", "-q", "--detach")
		}, ErrResolutionIncomplete},
	} {
		t.Run(name, func(t *testing.T) {
			f := newResolutionFixture(t)
			r, err := f.store.PrepareResolution(f.project, f.conflict.ID)
			if err != nil {
				t.Fatal(err)
			}
			tc.author(t, r)
			if _, err := f.store.ImportResolution(f.project, f.conflict.ID, "a2", nil, candidateRuntime); !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
			ops, err := f.store.List(f.project.ID)
			if err != nil || len(ops) != 1 {
				t.Fatalf("a refused resolution prepared an operation: %d, %v", len(ops), err)
			}
		})
	}
}

func TestTwoRejectedResolutionsBlockTheTask(t *testing.T) {
	f := newResolutionFixture(t)
	r, err := f.store.PrepareResolution(f.project, f.conflict.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolve(t, r, "first\n")
	first, err := f.store.ImportResolution(f.project, f.conflict.ID, "a2", nil, candidateRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RejectResolution(f.project, f.conflict.ID, "no"); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("the conflicted integration was rejected as a resolution: %v", err)
	}
	if _, err := f.store.RejectResolution(f.project, first.ID, " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a rejection without reason: %v", err)
	}
	rejected, err := f.store.RejectResolution(f.project, first.ID, "the review asked for changes")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.State != RolledBack {
		t.Fatalf("rejected %+v", rejected)
	}
	if current := resolutionTask(t, f.store); current.State != task.MergeConflict || current.ConflictFailures != 1 {
		t.Fatalf("task %s with %d failures", current.State, current.ConflictFailures)
	}
	if n, err := f.store.RejectedResolutions("t1", f.base); err != nil || n != 1 {
		t.Fatalf("rejected %d, %v", n, err)
	}
	if _, err := f.store.RejectResolution(f.project, first.ID, "again"); !errors.Is(err, ErrTransition) {
		t.Fatalf("a resolution was rejected twice: %v", err)
	}
	got := events(t, f.store, first.ID)
	if last := got[len(got)-1]; last != "reject-resolution:ROLLED_BACK>ROLLED_BACK" {
		t.Fatalf("events %v", got)
	}

	// The second attempt follows the rejected one, in its own worktree.
	r2, err := f.store.PrepareResolution(f.project, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Path == r.Path || r2.OperationID != first.ID || r2.ConflictOperationID != f.conflict.ID || r2.Conflict.Commit != f.chain[1] {
		t.Fatalf("second resolution %+v", r2)
	}
	resolve(t, r2, "second\n")
	second, err := f.store.ImportResolution(f.project, first.ID, "a3", nil, candidateRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || second.SupersedesOperationID != first.ID {
		t.Fatalf("second %+v", second)
	}
	if _, err := f.store.RejectResolution(f.project, second.ID, "tests failed"); err != nil {
		t.Fatal(err)
	}
	current := resolutionTask(t, f.store)
	if current.State != task.Blocked || current.ResumeState != task.MergeConflict ||
		!strings.HasPrefix(current.BlockedReason, task.ReasonConflictUnresolved) {
		t.Fatalf("task %+v", current)
	}
	if n, err := f.store.RejectedResolutions("t1", f.base); err != nil || n != 2 {
		t.Fatalf("rejected %d, %v", n, err)
	}
	if _, err := f.store.PrepareResolution(f.project, second.ID); !errors.Is(err, ErrResolutionsExhausted) {
		t.Fatalf("a third resolution was prepared: %v", err)
	}
	if head := candidateIntegrationHead(t, f.candidateFixture); head != f.base {
		t.Fatalf("the integration branch moved to %s", head)
	}
}

func TestAcceptedResolutionIsValidatedAndPublished(t *testing.T) {
	f := newResolutionFixture(t)
	r, err := f.store.PrepareResolution(f.project, f.conflict.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolve(t, r, "resolved\n")
	op, err := f.store.ImportResolution(f.project, f.conflict.ID, "a2", nil, candidateRuntime)
	if err != nil {
		t.Fatal(err)
	}
	verified := task.Guard{ProposalValid: true, ResolutionVerified: true, HumanGateRequired: true}
	if _, err := f.store.AcceptResolution(f.project, op.ID, verified); !errors.Is(err, ErrTransition) {
		t.Fatalf("an untested resolution was accepted: %v", err)
	}
	op = tested(t, f.store, f.project, op)
	if _, err := f.store.AcceptResolution(f.project, op.ID, verified); !errors.Is(err, task.ErrGuard) {
		t.Fatalf("a resolution was accepted without its human approval: %v", err)
	}
	if current := resolutionTask(t, f.store); current.State != task.MergeConflict {
		t.Fatalf("a refused acceptance changed the task: %s", current.State)
	}
	approval := task.Input{Event: task.ApproveResolution, Revision: op.ResultSHA, Guard: task.Guard{Trigger: task.Human, ResolutionVerified: true}}
	if _, err := (task.Store{DB: f.store.DB}).Transition("t1", approval); err != nil {
		t.Fatal(err)
	}
	validating, err := f.store.AcceptResolution(f.project, op.ID, verified)
	if err != nil {
		t.Fatal(err)
	}
	if validating.State != task.Validating || validating.ResultSHA != op.ResultSHA {
		t.Fatalf("task %+v", validating)
	}
	committed, err := f.store.Publish(f.project, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committed.State != Committed || candidateIntegrationHead(t, f.candidateFixture) != op.ResultSHA {
		t.Fatalf("committed %+v", committed)
	}
	if state := resolutionTask(t, f.store).State; state != task.Done {
		t.Fatalf("task %s", state)
	}
}

func TestResolutionRefusesOperationsWithoutConflict(t *testing.T) {
	f := candidateRepository(t, "base\n", [2]string{"main.go", "package main\n"})
	if _, _, err := (worktree.Store{Base: t.TempDir()}).AddWorker(f.project, "w1"); err != nil {
		t.Fatal(err)
	}
	store := openJournal(t)
	op := candidateOperation(t, store, f)
	if _, err := store.PrepareResolution(f.project, op.ID); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("a prepared integration was resolved: %v", err)
	}
	if _, err := store.Fail(op.ID, "tests failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareResolution(f.project, op.ID); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("a failure without conflict was resolved: %v", err)
	}
	other := worktree.Project{ID: "other", Dir: f.project.Dir}
	if _, err := store.PrepareResolution(other, op.ID); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("a foreign operation was resolved: %v", err)
	}
	if _, err := ResolutionWorktree(f.project, "w1", "../x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err=%v", err)
	}
	if _, err := ResolutionBranch("x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestPrepareResolutionNeedsTheTaskInConflict(t *testing.T) {
	f := candidateRepository(t, "base\n", [2]string{"base.txt", "task\n"})
	if _, _, err := (worktree.Store{Base: t.TempDir()}).AddWorker(f.project, "w1"); err != nil {
		t.Fatal(err)
	}
	store := resolutionJournal(t)
	op := candidateOperation(t, store, f)
	if _, err := store.BuildCandidate(f.project, op.ID, candidateRuntime); !errors.Is(err, ErrCandidateConflict) {
		t.Fatalf("err=%v", err)
	}
	// The task is still INTEGRATING: no conflict was reported to it.
	if _, err := store.PrepareResolution(f.project, op.ID); !errors.Is(err, ErrResolutionTask) {
		t.Fatalf("err=%v", err)
	}
	if stored, _ := store.Get(op.ID); stored.State != Failed {
		t.Fatalf("a refused preparation rolled back the operation: %s", stored.State)
	}
	if _, err := os.Stat(filepath.Join(f.project.Dir, "worktrees", "w1", "resolutions")); !os.IsNotExist(err) {
		t.Fatalf("a refused preparation left a worktree: %v", err)
	}
}
