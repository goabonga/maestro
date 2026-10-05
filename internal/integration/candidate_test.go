// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/provision"
	"github.com/goabonga/maestro/internal/worktree"
)

// candidateFixture is a canonical repository holding a task chain and
// an integration branch that moved past the task base.
type candidateFixture struct {
	user     string
	project  worktree.Project
	taskBase string
	base     string
	chain    []string
}

// candidateUserCommit writes name with content in the user repository,
// commits it and returns the commit id.
func candidateUserCommit(t *testing.T, user, name, content, message string) string {
	t.Helper()
	file := filepath.Join(user, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, user, false, "", "add", "-f", name)
	gitIn(t, user, false, "", "commit", "-q", "-m", message)
	return gitIn(t, user, false, "", "rev-parse", "HEAD")
}

// candidateRepository builds a user repository whose main branch holds
// the task base then the integration base, which adds base.txt with
// baseContent, and whose task branch applies each change of task on
// the task base in order. It imports it into a canonical repository.
func candidateRepository(t *testing.T, baseContent string, task ...[2]string) candidateFixture {
	t.Helper()
	user := t.TempDir()
	gitIn(t, user, false, "", "init", "-q", "-b", "main")
	taskBase := candidateUserCommit(t, user, "README.md", "project\n", "feat: initial")
	gitIn(t, user, false, "", "checkout", "-q", "-b", "task")
	var chain []string
	for i, change := range task {
		chain = append(chain, candidateUserCommit(t, user, change[0], change[1], "task step "+string(rune('a'+i))))
	}
	gitIn(t, user, false, "", "checkout", "-q", "main")
	base := candidateUserCommit(t, user, "base.txt", baseContent, "feat: move the integration")
	project, _, err := worktree.Store{Base: t.TempDir()}.Init(user)
	if err != nil {
		t.Fatal(err)
	}
	return candidateFixture{user: user, project: project, taskBase: taskBase, base: base, chain: chain}
}

// candidateOperation prepares an integration of the fixture's chain.
func candidateOperation(t *testing.T, store Store, f candidateFixture) Operation {
	t.Helper()
	in := integrationInputs()
	in.ProjectID = f.project.ID
	in.TaskBaseSHA, in.IntegrationBaseSHA = f.taskBase, f.base
	in.SourceCommits, in.SourceHeadSHA = f.chain, f.chain[len(f.chain)-1]
	op, err := store.Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// candidateIntegrationHead returns where the integration branch is.
func candidateIntegrationHead(t *testing.T, f candidateFixture) string {
	t.Helper()
	return gitIn(t, f.project.Repository(), true, "", "rev-parse", integrationBranch)
}

// candidateWorktreeExists reports whether the operation's candidate
// worktree is on disk.
func candidateWorktreeExists(t *testing.T, f candidateFixture, id string) bool {
	t.Helper()
	path, err := CandidateWorktree(f.project, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(path)
	return err == nil
}

var candidateRuntime = provision.RuntimePaths{"CLAUDE.md", ".claude/"}

func TestBuildCandidateAggregatesTheFullDelta(t *testing.T) {
	f := candidateRepository(t, "base\n",
		[2]string{"main.go", "package main\n"},
		[2]string{"util.go", "package util\n"},
		[2]string{"main.go", "package main\n\nfunc main() {}\n"})
	store := openJournal(t)
	op := candidateOperation(t, store, f)

	applied, err := store.BuildCandidate(f.project, op.ID, candidateRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if applied.State != Applied || applied.ResultSHA == "" || applied.CandidateTreeSHA == "" {
		t.Fatalf("applied %+v", applied)
	}
	repo := f.project.Repository()
	if parents := gitIn(t, repo, true, "", "rev-list", "--parents", "-n", "1", applied.ResultSHA); parents != applied.ResultSHA+" "+f.base {
		t.Fatalf("parents %q", parents)
	}
	if tree := gitIn(t, repo, true, "", "rev-parse", applied.ResultSHA+"^{tree}"); tree != applied.CandidateTreeSHA {
		t.Fatalf("result tree %s, candidate %s", tree, applied.CandidateTreeSHA)
	}
	files := gitIn(t, repo, true, "", "ls-tree", "--name-only", applied.ResultSHA)
	if files != "README.md\nbase.txt\nmain.go\nutil.go" {
		t.Fatalf("files %q", files)
	}
	// The correction of the last commit is part of the delta.
	if main := gitIn(t, repo, true, "", "show", applied.ResultSHA+":main.go"); main != "package main\n\nfunc main() {}" {
		t.Fatalf("main.go %q", main)
	}
	if sha, ok, err := ProveResult(f.project, applied); !ok || err != nil || sha != applied.ResultSHA {
		t.Fatalf("proof sha=%s ok=%v err=%v", sha, ok, err)
	}
	if head := candidateIntegrationHead(t, f); head != f.base {
		t.Fatalf("the integration branch moved to %s", head)
	}
	if candidateWorktreeExists(t, f, op.ID) {
		t.Fatal("the candidate worktree survived a success")
	}
	if _, err := os.Stat(filepath.Join(f.project.Dir, "operations", op.ID)); !os.IsNotExist(err) {
		t.Fatalf("the operation directory survived: %v", err)
	}
	if listed := gitIn(t, repo, true, "", "worktree", "list", "--porcelain"); strings.Contains(listed, op.ID) {
		t.Fatalf("the candidate worktree is still registered: %s", listed)
	}
	got := events(t, store, op.ID)
	want := []string{"prepare:>PREPARED", "start:PREPARED>STARTED", "record-candidate:STARTED>STARTED", "apply:STARTED>APPLIED"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("events %v", got)
	}
}

func TestBuildCandidateIsReproducibleFromAStartedOperation(t *testing.T) {
	f := candidateRepository(t, "base\n", [2]string{"main.go", "package main\n"})
	store := openJournal(t)
	first := candidateOperation(t, store, f)
	second := candidateOperation(t, store, f)
	if _, err := store.Start(second.ID, ""); err != nil {
		t.Fatal(err)
	}
	a, err := store.BuildCandidate(f.project, first.ID, candidateRuntime)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.BuildCandidate(f.project, second.ID, candidateRuntime)
	if err != nil {
		t.Fatal(err)
	}
	// Same frozen inputs, same commit.
	if a.ResultSHA != b.ResultSHA || a.CandidateTreeSHA != b.CandidateTreeSHA {
		t.Fatalf("results %s and %s", a.ResultSHA, b.ResultSHA)
	}
	if _, err := store.BuildCandidate(f.project, first.ID, candidateRuntime); !errors.Is(err, ErrTransition) {
		t.Fatalf("an applied operation was rebuilt: %v", err)
	}
}

func TestBuildCandidateConflictKeepsTheBranchAndFails(t *testing.T) {
	f := candidateRepository(t, "base\n",
		[2]string{"notes.txt", "one\n"},
		[2]string{"base.txt", "task\n"})
	store := openJournal(t)
	op := candidateOperation(t, store, f)

	failed, err := store.BuildCandidate(f.project, op.ID, candidateRuntime)
	if !errors.Is(err, ErrCandidateConflict) {
		t.Fatalf("err=%v", err)
	}
	var conflict *CandidateConflict
	if !errors.As(err, &conflict) || conflict.Commit != f.chain[1] || strings.Join(conflict.Paths, ",") != "base.txt" {
		t.Fatalf("conflict %+v", conflict)
	}
	if !strings.Contains(err.Error(), `"base.txt"`) {
		t.Fatalf("the error does not name the path: %v", err)
	}
	if failed.State != Failed || !strings.Contains(failed.Error, "base.txt") || failed.ResultSHA != "" || failed.CandidateTreeSHA != "" {
		t.Fatalf("failed %+v", failed)
	}
	if stored, _ := store.Get(op.ID); stored.State != Failed {
		t.Fatalf("stored %+v", stored)
	}
	if head := candidateIntegrationHead(t, f); head != f.base {
		t.Fatalf("the integration branch moved to %s", head)
	}
	if _, ok, err := ReadResultRef(f.project.Repository(), op.ID); ok || err != nil {
		t.Fatalf("a result reference after a conflict: ok=%v err=%v", ok, err)
	}
	// The candidate is kept for diagnosis, back at its base with no
	// application in progress.
	path, _ := CandidateWorktree(f.project, op.ID)
	if !candidateWorktreeExists(t, f, op.ID) {
		t.Fatal("the candidate worktree was not kept")
	}
	if status := gitIn(t, path, false, "", "status", "--porcelain"); status != "" {
		t.Fatalf("the candidate is not clean: %q", status)
	}
	if head := gitIn(t, path, false, "", "rev-parse", "HEAD"); head != f.base {
		t.Fatalf("candidate HEAD %s", head)
	}
	// A kept candidate is never reused.
	if _, err := store.BuildCandidate(f.project, op.ID, candidateRuntime); !errors.Is(err, ErrTransition) {
		t.Fatalf("a failed operation was rebuilt: %v", err)
	}
}

func TestBuildCandidateReportsAnEmptyCandidate(t *testing.T) {
	f := candidateRepository(t, "base\n",
		[2]string{"scratch.txt", "draft\n"},
		[2]string{"base.txt", "base\n"})
	// The second step reproduces base.txt as the integration base has
	// it; removing the draft leaves nothing of the task.
	gitIn(t, f.user, false, "", "checkout", "-q", "task")
	gitIn(t, f.user, false, "", "rm", "-q", "scratch.txt")
	gitIn(t, f.user, false, "", "commit", "-q", "-m", "task: drop the draft")
	f.chain = append(f.chain, gitIn(t, f.user, false, "", "rev-parse", "HEAD"))
	gitIn(t, f.project.Repository(), true, "", "fetch", "-q", f.user, "task:refs/heads/task")
	store := openJournal(t)
	op := candidateOperation(t, store, f)

	failed, err := store.BuildCandidate(f.project, op.ID, candidateRuntime)
	if !errors.Is(err, ErrEmptyCandidate) {
		t.Fatalf("err=%v", err)
	}
	if failed.State != Failed || failed.ResultSHA != "" || failed.CandidateTreeSHA != "" {
		t.Fatalf("failed %+v", failed)
	}
	if _, ok, err := ReadResultRef(f.project.Repository(), op.ID); ok || err != nil {
		t.Fatalf("an empty candidate was committed: ok=%v err=%v", ok, err)
	}
	if candidateWorktreeExists(t, f, op.ID) {
		t.Fatal("the empty candidate worktree was kept")
	}
	if head := candidateIntegrationHead(t, f); head != f.base {
		t.Fatalf("the integration branch moved to %s", head)
	}
}

func TestBuildCandidateRefusesAMovedIntegrationBase(t *testing.T) {
	f := candidateRepository(t, "base\n", [2]string{"main.go", "package main\n"})
	store := openJournal(t)
	op := candidateOperation(t, store, f)
	repo := f.project.Repository()
	gitIn(t, repo, true, "", "update-ref", integrationBranch, f.taskBase, f.base)

	failed, err := store.BuildCandidate(f.project, op.ID, candidateRuntime)
	if !errors.Is(err, ErrStaleIntegrationBase) {
		t.Fatalf("err=%v", err)
	}
	if failed.State != Failed {
		t.Fatalf("failed %+v", failed)
	}
	if candidateWorktreeExists(t, f, op.ID) {
		t.Fatal("a candidate was created on a stale base")
	}
}

func TestBuildCandidateRefusesCommitsMissingFromTheCanonicalRepository(t *testing.T) {
	f := candidateRepository(t, "base\n", [2]string{"main.go", "package main\n"})
	// A commit made after the import never reached the canonical
	// repository.
	gitIn(t, f.user, false, "", "checkout", "-q", "task")
	f.chain = append(f.chain, candidateUserCommit(t, f.user, "late.go", "package main\n", "task: late"))
	store := openJournal(t)
	op := candidateOperation(t, store, f)

	failed, err := store.BuildCandidate(f.project, op.ID, candidateRuntime)
	if !errors.Is(err, ErrSourceRevision) {
		t.Fatalf("err=%v", err)
	}
	if failed.State != Failed || candidateWorktreeExists(t, f, op.ID) {
		t.Fatalf("failed %+v", failed)
	}
}

func TestBuildCandidateRefusesAnInvalidChain(t *testing.T) {
	t.Run("runtime path", func(t *testing.T) {
		f := candidateRepository(t, "base\n", [2]string{"CLAUDE.md", "instructions\n"})
		store := openJournal(t)
		op := candidateOperation(t, store, f)
		failed, err := store.BuildCandidate(f.project, op.ID, candidateRuntime)
		if !errors.Is(err, ErrSourceChain) || !errors.Is(err, provision.ErrRuntimePath) {
			t.Fatalf("err=%v", err)
		}
		if failed.State != Failed || candidateWorktreeExists(t, f, op.ID) {
			t.Fatalf("failed %+v", failed)
		}
	})
	t.Run("not the frozen chain", func(t *testing.T) {
		f := candidateRepository(t, "base\n",
			[2]string{"main.go", "package main\n"},
			[2]string{"util.go", "package util\n"})
		f.chain = f.chain[1:]
		store := openJournal(t)
		op := candidateOperation(t, store, f)
		failed, err := store.BuildCandidate(f.project, op.ID, candidateRuntime)
		if !errors.Is(err, ErrSourceChain) {
			t.Fatalf("err=%v", err)
		}
		if failed.State != Failed || candidateWorktreeExists(t, f, op.ID) {
			t.Fatalf("failed %+v", failed)
		}
	})
}

func TestBuildCandidateRefusesForeignOperations(t *testing.T) {
	f := candidateRepository(t, "base\n", [2]string{"main.go", "package main\n"})
	store := openJournal(t)

	sync := syncInputs()
	sync.ProjectID = f.project.ID
	syncOp, err := store.Prepare(sync)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BuildCandidate(f.project, syncOp.ID, candidateRuntime); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a sync was built: %v", err)
	}

	in := integrationInputs()
	in.TaskBaseSHA, in.IntegrationBaseSHA = f.taskBase, f.base
	in.SourceCommits, in.SourceHeadSHA = f.chain, f.chain[0]
	other, err := store.Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BuildCandidate(f.project, other.ID, candidateRuntime); !errors.Is(err, ErrInvalid) {
		t.Fatalf("another project's operation was built: %v", err)
	}
	if stored, _ := store.Get(other.ID); stored.State != Prepared {
		t.Fatalf("a refused build changed the operation: %+v", stored)
	}

	if _, err := store.BuildCandidate(f.project, "missing", candidateRuntime); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}

func TestBuildCandidateRefusesALeftoverWorktree(t *testing.T) {
	f := candidateRepository(t, "base\n", [2]string{"main.go", "package main\n"})
	store := openJournal(t)
	op := candidateOperation(t, store, f)
	path, err := CandidateWorktree(f.project, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BuildCandidate(f.project, op.ID, candidateRuntime); !errors.Is(err, ErrCandidateWorktree) {
		t.Fatalf("err=%v", err)
	}
	if stored, _ := store.Get(op.ID); stored.State != Prepared {
		t.Fatalf("stored %+v", stored)
	}
}

func TestCandidateWorktreeIsUnderTheOperation(t *testing.T) {
	project := worktree.Project{ID: "p", Dir: "/data/projects/p"}
	id := "0b8f0a54-6c1e-4c1d-9b6a-2f0f6e1c9d3a"
	path, err := CandidateWorktree(project, id)
	if err != nil || path != "/data/projects/p/operations/"+id+"/candidate" {
		t.Fatalf("path=%s err=%v", path, err)
	}
	if _, err := CandidateWorktree(project, "../escape"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err=%v", err)
	}
}
