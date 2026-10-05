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
	"time"

	"github.com/goabonga/maestro/internal/handoff"
	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/testrun"
	"github.com/goabonga/maestro/internal/worktree"
)

// syncTestTester is a fixture test runner: it records the revisions it
// is asked to test, creates the clone directory and reports every
// command with the configured exit code.
type syncTestTester struct {
	exit   int
	err    error
	tested []string
}

func (f *syncTestTester) Run(repository, sha, clone string, commands []testrun.Command) (testrun.Run, error) {
	f.tested = append(f.tested, sha)
	if f.err != nil {
		return testrun.Run{}, f.err
	}
	if err := os.MkdirAll(clone, 0o700); err != nil {
		return testrun.Run{}, err
	}
	run := testrun.Run{TestedSHA: sha, Clone: clone}
	for _, command := range commands {
		run.Results = append(run.Results, testrun.Result{
			Name: command.Name, Argv: command.Argv, TestedSHA: sha, ExitCode: f.exit, Duration: time.Second,
		})
	}
	return run, nil
}

// syncTestCommands are the configured test commands of the fixtures.
var syncTestCommands = []testrun.Command{{Name: "unit", Argv: []string{"/bin/true"}}}

// syncTestFixture is a project imported from a user repository, with a
// journal and a syncer using a fixture tester.
type syncTestFixture struct {
	project worktree.Project
	user    string
	initial string
	syncer  Syncer
	tester  *syncTestTester
}

func newSyncTestFixture(t *testing.T) syncTestFixture {
	t.Helper()
	project, initial := canonicalProject(t)
	tester := &syncTestTester{}
	return syncTestFixture{
		project: project, user: filepath.Dir(project.UserRepository), initial: initial,
		syncer: Syncer{Store: openJournal(t), Tester: tester}, tester: tester,
	}
}

// commit writes a file in the user repository and commits it on the
// current branch.
func (f syncTestFixture) commit(t *testing.T, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.user, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.user, false, "", "add", name)
	gitIn(t, f.user, false, "", "commit", "-q", "-m", "feat: "+name)
	return gitIn(t, f.user, false, "", "rev-parse", "HEAD")
}

func (f syncTestFixture) request(branch string) SyncRequest {
	return SyncRequest{Project: f.project, Branch: branch, ConfigID: "sha256-x", Tests: syncTestCommands}
}

func (f syncTestFixture) integration(t *testing.T) string {
	t.Helper()
	return gitIn(t, f.project.Repository(), true, "", "rev-parse", "refs/heads/maestro/integration")
}

func TestSyncAdvancesTheIntegrationToTheFrozenSource(t *testing.T) {
	f := newSyncTestFixture(t)
	f.commit(t, "a.txt", "a\n")
	frozen := f.commit(t, "b.txt", "b\n")

	plan, err := f.syncer.Prepare(f.request("main"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.SyncedSHA != frozen || plan.PreviousSHA != f.initial || plan.Operation.State != Prepared ||
		plan.Operation.Type != Sync || plan.Operation.IntegrationBaseSHA != f.initial || plan.Operation.SourceHeadSHA != frozen {
		t.Fatalf("plan %+v", plan)
	}
	if len(plan.Diagnostics) != 0 {
		t.Fatalf("a clean repository has no diagnostic: %v", plan.Diagnostics)
	}
	// The branch moves on after the sync was prepared: the frozen SHA
	// is what gets synced.
	later := f.commit(t, "c.txt", "c\n")
	userRefs := gitIn(t, f.user, false, "", "for-each-ref")

	op, err := f.syncer.Run(plan)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != Committed || op.ResultSHA != frozen || op.CandidateRef != "refs/maestro/sync/"+op.ID || len(op.TestReportIDs) != 1 {
		t.Fatalf("operation %+v", op)
	}
	if head := f.integration(t); head != frozen {
		t.Fatalf("integration at %s, want the frozen %s (not %s)", head, frozen, later)
	}
	if got := gitIn(t, f.project.Repository(), true, "", "rev-parse", op.CandidateRef); got != frozen {
		t.Fatalf("sync reference at %s", got)
	}
	if pinned, ok, err := ReadResultRef(f.project.Repository(), op.ID); err != nil || !ok || pinned != frozen {
		t.Fatalf("result reference %s %v %v", pinned, ok, err)
	}
	if !reflect.DeepEqual(f.tester.tested, []string{frozen}) {
		t.Fatalf("tested %v", f.tester.tested)
	}
	stored, err := handoff.Load(f.syncer.Store.DB, op.TestReportIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testrun.ForRevision(stored.Document, frozen); err != nil {
		t.Fatalf("report not bound to the synced SHA: %v", err)
	}
	want := []string{"prepare:>PREPARED", "start:PREPARED>STARTED", "record-candidate:STARTED>STARTED",
		"apply:STARTED>APPLIED", "test:APPLIED>TESTED", "commit:TESTED>COMMITTED"}
	if got := events(t, f.syncer.Store, op.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v", got)
	}
	// The user repository was only read.
	if after := gitIn(t, f.user, false, "", "for-each-ref"); after != userRefs {
		t.Fatalf("user references changed:\n%s\n%s", userRefs, after)
	}
	if _, err := os.Stat(filepath.Join(f.project.Dir, "operations", op.ID, "tests")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the test clone of a passed sync is removed: %v", err)
	}
}

func TestSyncIgnoresUncommittedWorkWithADiagnostic(t *testing.T) {
	f := newSyncTestFixture(t)
	committed := f.commit(t, "a.txt", "committed\n")
	tree := gitIn(t, f.user, false, "", "rev-parse", "HEAD^{tree}")
	for name, content := range map[string]string{"a.txt": "staged\n", "b.txt": "new staged\n"} {
		if err := os.WriteFile(filepath.Join(f.user, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		gitIn(t, f.user, false, "", "add", name)
	}
	if err := os.WriteFile(filepath.Join(f.user, "a.txt"), []byte("unstaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.user, "untracked.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	index := gitIn(t, f.user, false, "", "ls-files", "--stage")

	plan, err := f.syncer.Prepare(f.request("main"))
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := strings.Join(plan.Diagnostics, "\n")
	for _, want := range []string{"staged changes", "unstaged changes", "untracked files"} {
		if !strings.Contains(diagnostics, want) {
			t.Fatalf("diagnostics miss %q: %v", want, plan.Diagnostics)
		}
	}
	op, err := f.syncer.Run(plan)
	if err != nil {
		t.Fatal(err)
	}
	if op.ResultSHA != committed || op.CandidateTreeSHA != tree {
		t.Fatalf("operation %+v, want the committed tree %s", op, tree)
	}
	if got := gitIn(t, f.user, false, "", "ls-files", "--stage"); got != index {
		t.Fatalf("user index changed:\n%s\n%s", index, got)
	}
}

func TestSyncNeverRunsTheUserContentFilters(t *testing.T) {
	f := newSyncTestFixture(t)
	f.commit(t, "a.txt", "a\n")
	marker := filepath.Join(t.TempDir(), "ran")
	gitIn(t, f.user, false, "", "config", "filter.spy.clean", "touch "+marker+"; cat")
	if err := os.WriteFile(filepath.Join(f.user, ".gitattributes"), []byte("*.txt filter=spy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.user, "a.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(marker)

	plan, err := f.syncer.Prepare(f.request("main"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Diagnostics, "\n"), "content filters") {
		t.Fatalf("diagnostics %v", plan.Diagnostics)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a user filter ran: %v", err)
	}
}

func TestSyncRunsNoHook(t *testing.T) {
	f := newSyncTestFixture(t)
	f.commit(t, "a.txt", "a\n")
	marker := filepath.Join(t.TempDir(), "hook")
	script := []byte("#!/bin/sh\necho ran >> " + marker + "\n")
	for _, dir := range []string{filepath.Join(f.project.Repository(), "hooks"), filepath.Join(f.project.UserRepository, "hooks")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, hook := range []string{"reference-transaction", "post-index-change", "fsmonitor-watchman"} {
			if err := os.WriteFile(filepath.Join(dir, hook), script, 0o700); err != nil { // #nosec G306 -- an executable test hook
				t.Fatal(err)
			}
		}
	}
	plan, err := f.syncer.Prepare(f.request("main"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.syncer.Run(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a hook ran: %v", err)
	}
}

func TestSyncRefusesADivergentSource(t *testing.T) {
	f := newSyncTestFixture(t)
	gitIn(t, f.user, false, "", "switch", "-q", "-c", "side")
	side := f.commit(t, "side.txt", "side\n")
	gitIn(t, f.user, false, "", "switch", "-q", "main")
	f.commit(t, "a.txt", "a\n")
	plan, err := f.syncer.Prepare(f.request("main"))
	if err != nil {
		t.Fatal(err)
	}
	op, err := f.syncer.Run(plan)
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.syncer.Prepare(f.request("side"))
	if !errors.Is(err, ErrSyncDiverged) || !strings.Contains(err.Error(), side) || !strings.Contains(err.Error(), op.ResultSHA) {
		t.Fatalf("expected a divergence naming both SHAs, got %v", err)
	}
	// An integration advanced by commits the user repository does not
	// hold is a divergence too.
	tree := gitIn(t, f.project.Repository(), true, "", "rev-parse", op.ResultSHA+"^{tree}")
	private := gitIn(t, f.project.Repository(), true, "private\n", "commit-tree", tree, "-p", op.ResultSHA)
	gitIn(t, f.project.Repository(), true, "", "update-ref", "refs/heads/maestro/integration", private)
	ahead := f.commit(t, "b.txt", "b\n")
	_, err = f.syncer.Prepare(f.request("main"))
	if !errors.Is(err, ErrSyncDiverged) || !strings.Contains(err.Error(), ahead) || !strings.Contains(err.Error(), private) {
		t.Fatalf("expected a divergence naming both SHAs, got %v", err)
	}
	if ops, err := f.syncer.Store.List(f.project.ID); err != nil || len(ops) != 1 {
		t.Fatalf("a refused sync journals nothing: %v %v", ops, err)
	}
	if head := f.integration(t); head != private {
		t.Fatalf("integration moved to %s", head)
	}
}

func TestSyncRefusesBeforeJournaling(t *testing.T) {
	f := newSyncTestFixture(t)
	cases := []struct {
		name    string
		syncer  Syncer
		request SyncRequest
		want    error
	}{
		{"missing branch", f.syncer, f.request("nope"), ErrSyncSource},
		{"option-like branch", f.syncer, f.request("--upload-pack=touch"), ErrSyncSource},
		{"invalid branch", f.syncer, f.request("a..b"), ErrSyncSource},
		{"empty branch", f.syncer, f.request(""), ErrSyncSource},
		{"up to date", f.syncer, f.request("main"), ErrSyncUpToDate},
		{"no test", f.syncer, SyncRequest{Project: f.project, Branch: "main", ConfigID: "sha256-x"}, ErrSyncUntested},
		{"no tester", Syncer{Store: f.syncer.Store}, f.request("main"), ErrSyncUntested},
	}
	for _, c := range cases {
		if _, err := c.syncer.Prepare(c.request); !errors.Is(err, c.want) {
			t.Fatalf("%s: expected %v, got %v", c.name, c.want, err)
		}
	}
	if ops, err := f.syncer.Store.List(f.project.ID); err != nil || len(ops) != 0 {
		t.Fatalf("operations %v %v", ops, err)
	}
}

func TestSyncRollsBackOnFailedTests(t *testing.T) {
	f := newSyncTestFixture(t)
	synced := f.commit(t, "a.txt", "a\n")
	f.tester.exit = 1
	plan, err := f.syncer.Prepare(f.request("main"))
	if err != nil {
		t.Fatal(err)
	}
	op, err := f.syncer.Run(plan)
	if !errors.Is(err, ErrSyncTestsFailed) {
		t.Fatalf("expected failed tests, got %v", err)
	}
	if op.State != RolledBack || op.ResultSHA != synced || !strings.Contains(op.Error, "unit") ||
		!strings.Contains(op.Error, "sync-"+op.ID+"-unit") {
		t.Fatalf("operation %+v", op)
	}
	if head := f.integration(t); head != f.initial {
		t.Fatalf("integration moved to %s", head)
	}
	if _, err := handoff.Load(f.syncer.Store.DB, "sync-"+op.ID+"-unit"); err != nil {
		t.Fatalf("the failed report is kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.project.Dir, "operations", op.ID, "tests")); err != nil {
		t.Fatalf("the test clone of a failed sync is kept: %v", err)
	}

	f.tester.exit, f.tester.err = 0, errors.New("sandbox lost")
	plan, err = f.syncer.Prepare(f.request("main"))
	if err != nil {
		t.Fatal(err)
	}
	if op, err = f.syncer.Run(plan); err == nil || op.State != RolledBack || !strings.Contains(op.Error, "sandbox lost") {
		t.Fatalf("operation %+v, error %v", op, err)
	}
}

func TestSyncRefusesAConcurrentIntegrationChange(t *testing.T) {
	f := newSyncTestFixture(t)
	first := f.commit(t, "a.txt", "a\n")
	plan, err := f.syncer.Prepare(f.request("main"))
	if err != nil {
		t.Fatal(err)
	}
	// Someone else advances the integration while the sync runs.
	tree := gitIn(t, f.project.Repository(), true, "", "rev-parse", f.initial+"^{tree}")
	other := gitIn(t, f.project.Repository(), true, "other\n", "commit-tree", tree, "-p", f.initial)
	gitIn(t, f.project.Repository(), true, "", "update-ref", "refs/heads/maestro/integration", other)

	op, err := f.syncer.Run(plan)
	if !errors.Is(err, ErrSyncConcurrent) || !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), f.initial) {
		t.Fatalf("expected a concurrent change naming both heads, got %v", err)
	}
	if op.State != RolledBack {
		t.Fatalf("operation %+v", op)
	}
	if head := f.integration(t); head != other {
		t.Fatalf("integration at %s, want %s; %s was not published", head, other, first)
	}
}

func TestSyncRunsTheTestsInTheSandbox(t *testing.T) {
	l, err := launcher.New()
	if errors.Is(err, launcher.ErrUnsupported) {
		t.Skipf("host cannot confine: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	f := newSyncTestFixture(t)
	f.syncer.Tester = &testrun.Runner{Launcher: l, Grace: time.Second}
	synced := f.commit(t, "a.txt", "a\n")
	request := f.request("main")
	request.Tests = []testrun.Command{{Name: "has-file", Argv: []string{"/bin/sh", "-c", "test -f a.txt"}}}
	plan, err := f.syncer.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	op, err := f.syncer.Run(plan)
	if err != nil || op.State != Committed || f.integration(t) != synced {
		t.Fatalf("operation %+v, error %v", op, err)
	}
}
