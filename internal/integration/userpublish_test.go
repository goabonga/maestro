// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/worktree"
)

// publishProject returns a canonical project, the working tree of its
// user repository and an operations store.
func publishProject(t *testing.T) (worktree.Project, string, Store) {
	t.Helper()
	project, _ := canonicalProject(t)
	return project, filepath.Dir(project.UserRepository), openJournal(t)
}

// advanceIntegration commits content on top of the canonical
// integration head, moves the branch there and journals the move as a
// committed SYNC operation whose test report is report. It returns the
// new head.
func advanceIntegration(t *testing.T, store Store, project worktree.Project, content, report string) string {
	t.Helper()
	repository := project.Repository()
	base := gitIn(t, repository, true, "", "rev-parse", "refs/heads/maestro/integration")
	tree := writeTree(t, repository, content)
	head := gitIn(t, repository, true, "feat: "+content, "commit-tree", tree, "-p", base)
	gitIn(t, repository, true, "", "update-ref", "refs/heads/maestro/integration", head, base)
	in := syncInputs()
	in.ProjectID, in.IntegrationBaseSHA = project.ID, base
	op, err := store.Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() (Operation, error){
		func() (Operation, error) { return store.Start(op.ID, "") },
		func() (Operation, error) { return store.RecordCandidate(op.ID, "", tree) },
		func() (Operation, error) { return store.RecordResult(op.ID, head) },
		func() (Operation, error) { return store.MarkTested(op.ID, Evidence{TestReportIDs: []string{report}}) },
		func() (Operation, error) { return store.Commit(op.ID, Evidence{}) },
	} {
		if _, err := step(); err != nil {
			t.Fatal(err)
		}
	}
	return head
}

// userBranch returns the published branch of the user repository, empty
// when it does not exist.
func userBranch(t *testing.T, project worktree.Project) string {
	t.Helper()
	sha, err := userPublishRead(project.UserRepository)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// publications returns the PUBLISH operations of the project.
func publications(t *testing.T, store Store, project worktree.Project) []Operation {
	t.Helper()
	ops, err := store.List(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var published []Operation
	for _, op := range ops {
		if op.Type == Publish {
			published = append(published, op)
		}
	}
	return published
}

func TestPublishCreatesThenFastForwardsTheUserBranch(t *testing.T) {
	project, user, store := publishProject(t)
	userHead := gitIn(t, user, false, "", "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(user, "README.md"), []byte("local edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := advanceIntegration(t, store, project, "one\n", "report-1")

	publication, err := store.PublishToUser(project)
	if err != nil {
		t.Fatal(err)
	}
	if publication.Previous != "" || publication.Published != first || publication.UpToDate {
		t.Fatalf("publication %+v", publication)
	}
	op := publication.Operation
	if op.Type != Publish || op.State != Committed || op.ResultSHA != first || op.SourceHeadSHA != first ||
		op.IntegrationBaseSHA != strings.Repeat("0", 40) || !reflect.DeepEqual(op.TestReportIDs, []string{"report-1"}) {
		t.Fatalf("operation %+v", op)
	}
	if got := events(t, store, op.ID); !reflect.DeepEqual(got, []string{
		"prepare:>PREPARED", "start:PREPARED>STARTED", "apply:STARTED>APPLIED", "commit:APPLIED>COMMITTED",
	}) {
		t.Fatalf("events %v", got)
	}
	if userBranch(t, project) != first {
		t.Fatal("the branch was not created")
	}
	if head := gitIn(t, user, false, "", "symbolic-ref", "HEAD"); head != "refs/heads/main" {
		t.Fatalf("HEAD moved to %s", head)
	}
	if head := gitIn(t, user, false, "", "rev-parse", "HEAD"); head != userHead {
		t.Fatalf("main moved to %s", head)
	}
	if status := gitIn(t, user, false, "", "status", "--porcelain"); status != "M README.md" {
		t.Fatalf("worktree touched: %q", status)
	}
	if refs := gitIn(t, user, false, "", "for-each-ref", "--format=%(refname)"); refs != "refs/heads/maestro/integration\nrefs/heads/main" {
		t.Fatalf("references %q", refs)
	}
	if _, err := os.Stat(filepath.Join(project.UserRepository, "FETCH_HEAD")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("FETCH_HEAD written: %v", err)
	}

	second := advanceIntegration(t, store, project, "two\n", "report-2")
	publication, err = store.PublishToUser(project)
	if err != nil {
		t.Fatal(err)
	}
	if publication.Previous != first || publication.Published != second || userBranch(t, project) != second {
		t.Fatalf("publication %+v", publication)
	}
	if op := publication.Operation; op.IntegrationBaseSHA != first || !reflect.DeepEqual(op.TestReportIDs, []string{"report-2"}) {
		t.Fatalf("the fast-forward carries %+v", op)
	}

	again, err := store.PublishToUser(project)
	if err != nil || !again.UpToDate || again.Published != second || again.Operation.ID != "" {
		t.Fatalf("again %+v, %v", again, err)
	}
	if n := len(publications(t, store, project)); n != 2 {
		t.Fatalf("%d publications journaled", n)
	}
}

func TestPublishRefusesAnUntestedIntegrationHead(t *testing.T) {
	project, _, store := publishProject(t)
	if _, err := store.PublishToUser(project); !errors.Is(err, ErrPublishUntested) {
		t.Fatalf("the imported head was published: %v", err)
	}
	if userBranch(t, project) != "" || len(publications(t, store, project)) != 0 {
		t.Fatal("a refused publication left a trace")
	}
}

func TestPublishRefusesABranchCheckedOutInAWorktree(t *testing.T) {
	project, user, store := publishProject(t)
	head := gitIn(t, user, false, "", "rev-parse", "HEAD")
	gitIn(t, user, false, "", "branch", "maestro/integration")
	linked := filepath.Join(t.TempDir(), "linked")
	gitIn(t, user, false, "", "worktree", "add", "-q", linked, "maestro/integration")
	advanceIntegration(t, store, project, "one\n", "report-1")

	_, err := store.PublishToUser(project)
	if !errors.Is(err, ErrPublishCheckedOut) || !strings.Contains(err.Error(), linked) {
		t.Fatalf("a checked-out branch was published: %v", err)
	}
	if userBranch(t, project) != head || len(publications(t, store, project)) != 0 {
		t.Fatal("a refused publication left a trace")
	}

	gitIn(t, user, false, "", "worktree", "remove", linked)
	gitIn(t, user, false, "", "switch", "-q", "maestro/integration")
	if _, err := store.PublishToUser(project); !errors.Is(err, ErrPublishCheckedOut) {
		t.Fatalf("the branch of the main worktree was published: %v", err)
	}
}

func TestPublishRefusesADivergedBranch(t *testing.T) {
	project, user, store := publishProject(t)
	gitIn(t, user, false, "", "branch", "maestro/integration")
	gitIn(t, user, false, "", "commit", "-q", "--allow-empty", "-m", "user work")
	local := gitIn(t, user, false, "", "rev-parse", "HEAD")
	gitIn(t, user, false, "", "branch", "-f", "maestro/integration", local)
	head := advanceIntegration(t, store, project, "one\n", "report-1")

	_, err := store.PublishToUser(project)
	if !errors.Is(err, ErrPublishDiverged) || !strings.Contains(err.Error(), local) || !strings.Contains(err.Error(), head) {
		t.Fatalf("a divergence was published: %v", err)
	}

	// A commit the integration knows but does not descend from diverges too.
	advanceIntegration(t, store, project, "two\n", "report-2")
	sibling := gitIn(t, project.Repository(), true, "sibling", "commit-tree", writeTree(t, project.Repository(), "x\n"), "-p", head)
	gitIn(t, project.Repository(), true, "", "update-ref", "refs/keep/sibling", sibling)
	gitIn(t, user, false, "", "fetch", "-q", project.Repository(), "refs/keep/sibling:refs/heads/maestro/integration", "--force")
	if _, err := store.PublishToUser(project); !errors.Is(err, ErrPublishDiverged) || !strings.Contains(err.Error(), sibling) {
		t.Fatalf("a sibling was overwritten: %v", err)
	}
	if userBranch(t, project) != sibling || len(publications(t, store, project)) != 0 {
		t.Fatal("a refused publication left a trace")
	}
}

func TestPublishRefusesAConcurrentUpdate(t *testing.T) {
	project, user, store := publishProject(t)
	advanceIntegration(t, store, project, "one\n", "report-1")
	gitIn(t, user, false, "", "commit", "-q", "--allow-empty", "-m", "elsewhere")
	concurrent := gitIn(t, user, false, "", "rev-parse", "HEAD")
	userPublishBeforeUpdate = func() {
		gitIn(t, user, false, "", "update-ref", UserPublishRef, concurrent)
	}
	t.Cleanup(func() { userPublishBeforeUpdate = func() {} })

	_, err := store.PublishToUser(project)
	if !errors.Is(err, ErrPublishConcurrent) || !strings.Contains(err.Error(), concurrent) {
		t.Fatalf("a concurrent update was overwritten: %v", err)
	}
	if userBranch(t, project) != concurrent {
		t.Fatal("the concurrent value was lost")
	}
	published := publications(t, store, project)
	if len(published) != 1 || published[0].State != RolledBack || !strings.Contains(published[0].Error, concurrent) {
		t.Fatalf("publications %+v", published)
	}
}

func TestPublishNeverRunsUserHooks(t *testing.T) {
	project, user, store := publishProject(t)
	marker := filepath.Join(t.TempDir(), "ran")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$0\" >> " + marker + "\n"
	for _, dir := range []string{hooks, filepath.Join(project.UserRepository, "hooks")} {
		for _, name := range []string{"reference-transaction", "post-checkout", "pre-auto-gc", "post-merge"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil { // #nosec G306 -- a test hook must be executable
				t.Fatal(err)
			}
		}
	}
	gitIn(t, user, false, "", "config", "core.hooksPath", hooks)
	head := advanceIntegration(t, store, project, "one\n", "report-1")
	if _, err := store.PublishToUser(project); err != nil {
		t.Fatal(err)
	}
	if userBranch(t, project) != head {
		t.Fatal("not published")
	}
	if ran, err := os.ReadFile(marker); !errors.Is(err, os.ErrNotExist) { // #nosec G304 -- the test's own temporary file
		t.Fatalf("a user hook ran: %s", ran)
	}
}

func TestPublishReconcilesAnInterruptedPublication(t *testing.T) {
	project, _, store := publishProject(t)
	head := advanceIntegration(t, store, project, "one\n", "report-1")
	interrupted := func() Operation {
		op, err := store.Prepare(Operation{
			Type: Publish, ProjectID: project.ID, ConfigID: "sha256-x", WorkerID: publicationWorker, AttemptID: newID(),
			IntegrationBaseSHA: strings.Repeat("0", 40), SourceHeadSHA: head, SourceCommits: []string{head},
		})
		if err != nil {
			t.Fatal(err)
		}
		if op, err = store.Start(op.ID, ""); err != nil {
			t.Fatal(err)
		}
		return op
	}

	// Interrupted before the reference update: rolled back, then retried.
	before := interrupted()
	publication, err := store.PublishToUser(project)
	if err != nil || publication.Operation.State != Committed || userBranch(t, project) != head {
		t.Fatalf("publication %+v, %v", publication, err)
	}
	if op, _ := store.Get(before.ID); op.State != RolledBack {
		t.Fatalf("the interrupted publication is %s", op.State)
	}

	// Interrupted after the reference update: finalized, nothing redone.
	gitIn(t, project.UserRepository, true, "", "update-ref", "-d", UserPublishRef)
	after := interrupted()
	gitIn(t, project.UserRepository, true, "", "update-ref", UserPublishRef, head)
	publication, err = store.PublishToUser(project)
	if err != nil || !publication.UpToDate {
		t.Fatalf("publication %+v, %v", publication, err)
	}
	if op, _ := store.Get(after.ID); op.State != Committed || !reflect.DeepEqual(op.TestReportIDs, []string{"report-1"}) {
		t.Fatalf("the interrupted publication is %+v", op)
	}

	// Any other value blocks without rewriting anything.
	gitIn(t, project.UserRepository, true, "", "update-ref", "-d", UserPublishRef)
	blocked := interrupted()
	other := gitIn(t, project.UserRepository, true, "", "rev-parse", "refs/heads/main")
	gitIn(t, project.UserRepository, true, "", "update-ref", UserPublishRef, other)
	if _, err := store.PublishToUser(project); !errors.Is(err, ErrPublishConcurrent) {
		t.Fatalf("an unexplained value was overwritten: %v", err)
	}
	if op, _ := store.Get(blocked.ID); op.State != Started || userBranch(t, project) != other {
		t.Fatalf("the blocked publication is %s", op.State)
	}
}
