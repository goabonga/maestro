// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/worktree"
)

// gitIn runs git in dir, or against the bare repository dir when bare,
// with an isolated configuration, and returns its trimmed output.
func gitIn(t *testing.T, dir string, bare bool, input string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test",
	}
	if bare {
		cmd.Env = append(cmd.Env, "GIT_DIR="+dir)
	}
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// canonicalProject imports a one-commit user repository and returns the
// project and the imported commit.
func canonicalProject(t *testing.T) (worktree.Project, string) {
	t.Helper()
	user := t.TempDir()
	gitIn(t, user, false, "", "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(user, "README.md"), []byte("project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, user, false, "", "add", "README.md")
	gitIn(t, user, false, "", "commit", "-q", "-m", "feat: initial")
	project, _, err := worktree.Store{Base: t.TempDir()}.Init(user)
	if err != nil {
		t.Fatal(err)
	}
	return project, gitIn(t, project.Repository(), true, "", "rev-parse", "refs/heads/maestro/integration")
}

// writeTree stores a tree holding one file with content in the bare
// repository and returns its id.
func writeTree(t *testing.T, repository, content string) string {
	t.Helper()
	blob := gitIn(t, repository, true, content, "hash-object", "-w", "--stdin")
	return gitIn(t, repository, true, "100644 blob "+blob+"\tREADME.md\n", "mktree")
}

// startedIntegration prepares and starts an integration on base and
// records its candidate tree.
func startedIntegration(t *testing.T, store Store, base, tree string) Operation {
	t.Helper()
	in := integrationInputs()
	in.IntegrationBaseSHA = base
	op, err := store.Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(op.ID, ""); err != nil {
		t.Fatal(err)
	}
	op, err = store.RecordCandidate(op.ID, "", tree)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestResultRefIsNamedAfterTheOperation(t *testing.T) {
	id := newID()
	ref, err := ResultRef(id)
	if err != nil || ref != "refs/maestro/operations/"+id+"/result" {
		t.Fatalf("ref=%q err=%v", ref, err)
	}
	for _, bad := range []string{"", "../heads/main", id + "/x", strings.ToUpper(id)} {
		if _, err := ResultRef(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestBuildResultIsReproducibleAndValid(t *testing.T) {
	project, base := canonicalProject(t)
	repository := project.Repository()
	want := Expected{Base: base, Tree: writeTree(t, repository, "changed\n"), Metadata: *frozenMetadata()}
	first, err := BuildResult(repository, want)
	if err != nil {
		t.Fatal(err)
	}
	again, err := BuildResult(repository, want)
	if err != nil || again != first {
		t.Fatalf("rebuilt %s, first %s, err %v", again, first, err)
	}
	if err := ValidateResult(repository, first, want); err != nil {
		t.Fatal(err)
	}
	if parents := gitIn(t, repository, true, "", "rev-list", "--parents", "-n", "1", first); parents != first+" "+base {
		t.Fatalf("parents %q", parents)
	}
	if author := gitIn(t, repository, true, "", "log", "-1", "--format=%an <%ae> %ad", "--date=iso-strict", first); author != "Ada Lovelace <ada@example.com> 2026-10-05T12:30:00+02:00" {
		t.Fatalf("author %q", author)
	}
}

func TestValidateResultRefusesAnyOtherCommit(t *testing.T) {
	project, base := canonicalProject(t)
	repository := project.Repository()
	tree := writeTree(t, repository, "changed\n")
	want := Expected{Base: base, Tree: tree, Metadata: *frozenMetadata()}
	good, err := BuildResult(repository, want)
	if err != nil {
		t.Fatal(err)
	}
	other := want
	other.Tree = writeTree(t, repository, "other\n")
	wrongTree, err := BuildResult(repository, other)
	if err != nil {
		t.Fatal(err)
	}
	other = want
	other.Base = good
	wrongBase, err := BuildResult(repository, other)
	if err != nil {
		t.Fatal(err)
	}
	other = want
	other.Metadata.Message = "feat: something else"
	wrongMessage, err := BuildResult(repository, other)
	if err != nil {
		t.Fatal(err)
	}
	other = want
	other.Metadata.Committer.When = other.Metadata.Committer.When.Add(1e9)
	wrongDate, err := BuildResult(repository, other)
	if err != nil {
		t.Fatal(err)
	}
	other = want
	other.Metadata.Author.Email = "eve@example.com"
	wrongAuthor, err := BuildResult(repository, other)
	if err != nil {
		t.Fatal(err)
	}
	// A merge of the base and the expected result carries the source in
	// its history: that proves nothing.
	merge := gitIn(t, repository, true, "feat: add a flag\n", "commit-tree", tree, "-p", base, "-p", good)
	raw := gitIn(t, repository, true, "", "cat-file", "commit", good)
	header, message, _ := strings.Cut(raw, "\n\n")
	encoded := gitIn(t, repository, true, header+"\nencoding UTF-8\n\n"+message+"\n", "hash-object", "-t", "commit", "-w", "--stdin")
	for name, sha := range map[string]string{
		"wrong tree": wrongTree, "wrong base": wrongBase, "wrong message": wrongMessage,
		"wrong date": wrongDate, "wrong author": wrongAuthor, "two parents": merge,
		"extra header": encoded, "a tree": tree, "not an id": "HEAD",
	} {
		if err := ValidateResult(repository, sha, want); !errors.Is(err, ErrInvalidResult) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestResultRefIsCreatedOnlyOnce(t *testing.T) {
	project, base := canonicalProject(t)
	repository := project.Repository()
	id := newID()
	if _, ok, err := ReadResultRef(repository, id); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	result, err := BuildResult(repository, Expected{Base: base, Tree: writeTree(t, repository, "x\n"), Metadata: *frozenMetadata()})
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateResultRef(repository, id, result); err != nil {
		t.Fatal(err)
	}
	if err := CreateResultRef(repository, id, result); err != nil {
		t.Fatalf("repeating the same creation: %v", err)
	}
	if err := CreateResultRef(repository, id, base); !errors.Is(err, ErrResultRefConflict) {
		t.Fatalf("overwrite: %v", err)
	}
	if sha, ok, err := ReadResultRef(repository, id); !ok || err != nil || sha != result {
		t.Fatalf("sha=%s ok=%v err=%v", sha, ok, err)
	}
}

func TestApplyIntegrationPublishesTheProofBeforeTheJournal(t *testing.T) {
	project, base := canonicalProject(t)
	store := openJournal(t)
	op := startedIntegration(t, store, base, writeTree(t, project.Repository(), "changed\n"))
	if _, ok, err := ProveResult(project, op); ok || err != nil {
		t.Fatalf("a proof before the result: ok=%v err=%v", ok, err)
	}
	want, err := op.expected()
	if err != nil {
		t.Fatal(err)
	}
	result, err := BuildResult(project.Repository(), want)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := store.ApplyIntegration(project, op.ID, result)
	if err != nil {
		t.Fatal(err)
	}
	if applied.State != Applied || applied.ResultSHA != result {
		t.Fatalf("applied %+v", applied)
	}
	ref, _ := ResultRef(op.ID)
	if got := gitIn(t, project.Repository(), true, "", "rev-parse", ref); got != result {
		t.Fatalf("%s at %s", ref, got)
	}
	if _, err := store.MarkTested(op.ID, Evidence{TestReportIDs: []string{"r1"}}); err != nil {
		t.Fatal(err)
	}
	committed, err := store.Commit(op.ID, Evidence{})
	if err != nil {
		t.Fatal(err)
	}
	// The proof outlives the finalization.
	if sha, ok, err := ProveResult(project, committed); !ok || err != nil || sha != result {
		t.Fatalf("sha=%s ok=%v err=%v", sha, ok, err)
	}
	if integration := gitIn(t, project.Repository(), true, "", "rev-parse", "refs/heads/maestro/integration"); integration != base {
		t.Fatalf("the integration branch moved to %s", integration)
	}
}

func TestApplyIntegrationRefusesAForeignResult(t *testing.T) {
	project, base := canonicalProject(t)
	store := openJournal(t)
	op := startedIntegration(t, store, base, writeTree(t, project.Repository(), "changed\n"))
	want, _ := op.expected()
	want.Tree = writeTree(t, project.Repository(), "other\n")
	foreign, err := BuildResult(project.Repository(), want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyIntegration(project, op.ID, foreign); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("err=%v", err)
	}
	if _, ok, err := ReadResultRef(project.Repository(), op.ID); ok || err != nil {
		t.Fatalf("a reference was created for a foreign result: ok=%v err=%v", ok, err)
	}
	if stored, _ := store.Get(op.ID); stored.State != Started || stored.ResultSHA != "" {
		t.Fatalf("stored %+v", stored)
	}
}

func TestCrashAfterTheReferenceIsRecoveredFromTheProof(t *testing.T) {
	project, base := canonicalProject(t)
	store := openJournal(t)
	op := startedIntegration(t, store, base, writeTree(t, project.Repository(), "changed\n"))
	want, _ := op.expected()
	result, err := BuildResult(project.Repository(), want)
	if err != nil {
		t.Fatal(err)
	}
	// The daemon dies after creating the reference, before the journal.
	if err := CreateResultRef(project.Repository(), op.ID, result); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(op.ID)
	if err != nil || stored.State != Started || stored.ResultSHA != "" {
		t.Fatalf("a crash changed the journal: %+v %v", stored, err)
	}
	proven, ok, err := ProveResult(project, stored)
	if err != nil || !ok || proven != result {
		t.Fatalf("proven=%s ok=%v err=%v", proven, ok, err)
	}
	applied, err := store.ApplyIntegration(project, op.ID, proven)
	if err != nil || applied.State != Applied || applied.ResultSHA != result {
		t.Fatalf("applied=%+v err=%v", applied, err)
	}
}

func TestInconsistentProofNeverSucceeds(t *testing.T) {
	project, base := canonicalProject(t)
	store := openJournal(t)
	op := startedIntegration(t, store, base, writeTree(t, project.Repository(), "changed\n"))
	want, _ := op.expected()
	result, err := BuildResult(project.Repository(), want)
	if err != nil {
		t.Fatal(err)
	}
	// The reference already points at the base: it is never rewritten.
	if err := CreateResultRef(project.Repository(), op.ID, base); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ProveResult(project, op); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("an inconsistent proof: %v", err)
	}
	if _, err := store.ApplyIntegration(project, op.ID, result); !errors.Is(err, ErrResultRefConflict) {
		t.Fatalf("err=%v", err)
	}
	if sha, _, _ := ReadResultRef(project.Repository(), op.ID); sha != base {
		t.Fatalf("the reference was rewritten to %s", sha)
	}
	if stored, _ := store.Get(op.ID); stored.State != Started || stored.ResultSHA != "" {
		t.Fatalf("stored %+v", stored)
	}
}
