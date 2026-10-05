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
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/testrun"
	"github.com/goabonga/maestro/internal/worktree"
)

// publicationFixture returns a journal, a canonical project and an
// applied integration of the task t1 on the project's integration head,
// with the task VALIDATING the operation's result.
func publicationFixture(t *testing.T) (Store, worktree.Project, Operation) {
	t.Helper()
	store := openJournal(t)
	project, base := canonicalProject(t)
	tree := writeTree(t, project.Repository(), "changed\n")
	op := startedIntegration(t, store, base, tree)
	result, err := BuildResult(project.Repository(), Expected{Base: base, Tree: tree, Metadata: *op.CommitMetadata})
	if err != nil {
		t.Fatal(err)
	}
	if op, err = store.ApplyIntegration(project, op.ID, result); err != nil {
		t.Fatal(err)
	}
	at := stamp(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if _, err := store.DB.Exec(`UPDATE tasks SET state = 'VALIDATING', result_sha = ?, created_at = ?, updated_at = ?
		WHERE task_id = 't1'`, result, at, at); err != nil {
		t.Fatal(err)
	}
	return store, project, op
}

// acceptReport stores a fake TEST_REPORT of the task t1 for the command
// name on revision, produced under configID, exiting with exitCode, and
// returns its artifact id.
func acceptReport(t *testing.T, store Store, name, configID, revision string, exitCode int) string {
	t.Helper()
	run := testrun.Run{TestedSHA: revision, Results: []testrun.Result{{
		Name: name, Argv: []string{"make", "test"}, TestedSHA: revision, ExitCode: exitCode, Output: "log of " + name,
	}}}
	stored, err := testrun.Accept(store.DB, run, testrun.Identity{
		ArtifactPrefix: "report", TaskID: "t1", TurnID: "turn-1", AttemptID: "a1", WorkerID: "w1", ConfigID: configID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return stored[0].Envelope.ArtifactID
}

// branchOf returns the commit the integration branch points at.
func branchOf(t *testing.T, project worktree.Project) string {
	t.Helper()
	return gitIn(t, project.Repository(), true, "", "rev-parse", IntegrationBranch)
}

// taskState returns the stored state of the task t1.
func taskState(t *testing.T, store Store) task.State {
	t.Helper()
	current, err := task.Store{DB: store.DB}.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	return current.State
}

// tested records one passing report on the fixture's operation.
func tested(t *testing.T, store Store, project worktree.Project, op Operation) Operation {
	t.Helper()
	op, err := store.RecordTestReports(project, op.ID, []string{acceptReport(t, store, "unit", "sha256-x", op.ResultSHA, 0)})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestPublishMovesTheBranchThenCommitsAndFinishesTheTask(t *testing.T) {
	store, project, op := publicationFixture(t)
	base := op.IntegrationBaseSHA
	report := acceptReport(t, store, "unit", "sha256-x", op.ResultSHA, 0)
	op, err := store.RecordTestReports(project, op.ID, []string{report})
	if err != nil || op.State != Tested || !reflect.DeepEqual(op.TestReportIDs, []string{report}) {
		t.Fatalf("op=%+v err=%v", op, err)
	}
	if branch := branchOf(t, project); branch != base {
		t.Fatalf("tested but branch at %s", branch)
	}
	committed, err := store.Publish(project, op.ID)
	if err != nil || committed.State != Committed {
		t.Fatalf("committed=%+v err=%v", committed, err)
	}
	if branch := branchOf(t, project); branch != op.ResultSHA {
		t.Fatalf("branch at %s, result %s", branch, op.ResultSHA)
	}
	if stored, err := store.Get(op.ID); err != nil || stored.State != Committed || stored.Version != committed.Version {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if state := taskState(t, store); state != task.Done {
		t.Fatalf("task %s", state)
	}
	if got := events(t, store, op.ID); got[len(got)-1] != "commit:TESTED>COMMITTED" {
		t.Fatalf("events %v", got)
	}
	view := IntegrationView(project)
	if head := gitIn(t, view, false, "", "rev-parse", "HEAD"); head != op.ResultSHA {
		t.Fatalf("view at %s", head)
	}
	if content, err := os.ReadFile(filepath.Join(view, "README.md")); err != nil || string(content) != "changed\n" {
		t.Fatalf("view content %q err=%v", content, err)
	}
	if _, err := store.Publish(project, op.ID); !errors.Is(err, ErrTransition) {
		t.Fatalf("second publication: %v", err)
	}
}

func TestRecordTestReportsRefusesReportsNotBoundToTheResult(t *testing.T) {
	store, project, op := publicationFixture(t)
	if _, err := store.DB.Exec("INSERT INTO config_snapshots (config_id, created_at, document) VALUES ('sha256-y', 'now', '{}')"); err != nil {
		t.Fatal(err)
	}
	onBase := acceptReport(t, store, "base", "sha256-x", op.IntegrationBaseSHA, 0)
	otherConfig := acceptReport(t, store, "config", "sha256-y", op.ResultSHA, 0)
	cases := map[string]struct {
		ids  []string
		want error
	}{
		"none":           {nil, ErrGuard},
		"unknown":        {[]string{"report-missing"}, ErrGuard},
		"other revision": {[]string{onBase}, testrun.ErrRevision},
		"other config":   {[]string{otherConfig}, handoff.ErrConfig},
	}
	for name, c := range cases {
		if _, err := store.RecordTestReports(project, op.ID, c.ids); !errors.Is(err, c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := store.DB.Exec("UPDATE tasks SET config_id = 'sha256-y' WHERE task_id = 't1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordTestReports(project, op.ID, []string{otherConfig}); !errors.Is(err, handoff.ErrConfig) {
		t.Errorf("task on another snapshot: %v", err)
	}
	if stored, err := store.Get(op.ID); err != nil || stored.State != Applied || stored.Version != op.Version {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
}

func TestFailedTestsRollBackAndKeepTheDiagnostics(t *testing.T) {
	store, project, op := publicationFixture(t)
	passing := acceptReport(t, store, "lint", "sha256-x", op.ResultSHA, 0)
	failing := acceptReport(t, store, "unit", "sha256-x", op.ResultSHA, 2)
	rolledBack, err := store.RecordTestReports(project, op.ID, []string{passing, failing})
	if !errors.Is(err, ErrTestsFailed) || rolledBack.State != RolledBack {
		t.Fatalf("rolledBack=%+v err=%v", rolledBack, err)
	}
	if !strings.Contains(rolledBack.Error, failing) || strings.Contains(rolledBack.Error, passing) {
		t.Fatalf("error %q", rolledBack.Error)
	}
	want := []string{"fail:APPLIED>FAILED", "roll-back:FAILED>ROLLED_BACK"}
	if got := events(t, store, op.ID); !reflect.DeepEqual(got[len(got)-2:], want) {
		t.Fatalf("events %v", got)
	}
	if branch := branchOf(t, project); branch != op.IntegrationBaseSHA {
		t.Fatalf("branch moved to %s", branch)
	}
	if sha, ok, err := ReadResultRef(project.Repository(), op.ID); err != nil || !ok || sha != op.ResultSHA {
		t.Fatalf("result reference %s %v %v", sha, ok, err)
	}
	if _, err := handoff.Load(store.DB, failing); err != nil {
		t.Fatalf("failing report lost: %v", err)
	}
	if state := taskState(t, store); state != task.Validating {
		t.Fatalf("task %s", state)
	}
	if _, err := store.Publish(project, op.ID); !errors.Is(err, ErrTransition) {
		t.Fatalf("publication of a rolled back operation: %v", err)
	}
}

func TestRollBackIsRefusedOnceTheResultIsPublished(t *testing.T) {
	store, project, op := publicationFixture(t)
	gitIn(t, project.Repository(), true, "", "update-ref", IntegrationBranch, op.ResultSHA, op.IntegrationBaseSHA)
	failed, err := store.RollBackIntegration(project, op.ID, "abandoned")
	if !errors.Is(err, ErrPublished) || failed.State != Failed {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}
	if branch := branchOf(t, project); branch != op.ResultSHA {
		t.Fatalf("branch rewritten to %s", branch)
	}
}

func TestPublishRefusesAMovedBranchWithoutCommitting(t *testing.T) {
	store, project, op := publicationFixture(t)
	op = tested(t, store, project, op)
	other, err := BuildResult(project.Repository(), Expected{
		Base: op.IntegrationBaseSHA, Tree: writeTree(t, project.Repository(), "other\n"), Metadata: *op.CommitMetadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, project.Repository(), true, "", "update-ref", IntegrationBranch, other)
	if _, err := store.Publish(project, op.ID); !errors.Is(err, ErrBranchMoved) {
		t.Fatalf("err=%v", err)
	}
	if branch := branchOf(t, project); branch != other {
		t.Fatalf("branch at %s", branch)
	}
	if stored, err := store.Get(op.ID); err != nil || stored.State != Tested || stored.Version != op.Version {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if state := taskState(t, store); state != task.Validating {
		t.Fatalf("task %s", state)
	}
}

func TestPublishFinalizesAfterACrashFollowingTheUpdateRef(t *testing.T) {
	store, project, op := publicationFixture(t)
	op = tested(t, store, project, op)
	// The crash: the branch was moved, SQLite was not committed.
	gitIn(t, project.Repository(), true, "", "update-ref", IntegrationBranch, op.ResultSHA, op.IntegrationBaseSHA)
	committed, err := store.Publish(project, op.ID)
	if err != nil || committed.State != Committed {
		t.Fatalf("committed=%+v err=%v", committed, err)
	}
	if state := taskState(t, store); state != task.Done {
		t.Fatalf("task %s", state)
	}
}

func TestPublishChecksTheProofAndTheTaskBeforeAnyMutation(t *testing.T) {
	store, project, op := publicationFixture(t)
	if _, err := store.Publish(project, op.ID); !errors.Is(err, ErrTransition) {
		t.Fatalf("untested: %v", err)
	}
	op = tested(t, store, project, op)
	if _, err := store.DB.Exec("UPDATE tasks SET state = 'INTEGRATING' WHERE task_id = 't1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(project, op.ID); !errors.Is(err, task.ErrTransition) {
		t.Fatalf("task not validating: %v", err)
	}
	if _, err := store.DB.Exec("UPDATE tasks SET state = 'VALIDATING' WHERE task_id = 't1'"); err != nil {
		t.Fatal(err)
	}
	ref, err := ResultRef(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, project.Repository(), true, "", "update-ref", "-d", ref)
	if _, err := store.Publish(project, op.ID); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("no result reference: %v", err)
	}
	if branch := branchOf(t, project); branch != op.IntegrationBaseSHA {
		t.Fatalf("branch moved to %s", branch)
	}
	if stored, err := store.Get(op.ID); err != nil || stored.State != Tested {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
}

func TestFinalizationCommitsNothingWhenTheTaskCannotFinish(t *testing.T) {
	store, project, op := publicationFixture(t)
	op = tested(t, store, project, op)
	if _, err := store.DB.Exec("UPDATE tasks SET state = 'INTEGRATING' WHERE task_id = 't1'"); err != nil {
		t.Fatal(err)
	}
	pass := task.Input{Event: task.ValidationPass, Revision: op.ResultSHA, Guard: task.Guard{Published: true}}
	if _, err := store.publicationFinalize(task.Store{DB: store.DB}, op, pass); !errors.Is(err, task.ErrTransition) {
		t.Fatalf("err=%v", err)
	}
	if stored, err := store.Get(op.ID); err != nil || stored.State != Tested || stored.Version != op.Version {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if got := events(t, store, op.ID); got[len(got)-1] != "test:APPLIED>TESTED" {
		t.Fatalf("events %v", got)
	}
}

func TestAnUnexplainedChangeInTheViewForbidsPublication(t *testing.T) {
	store, project, op := publicationFixture(t)
	op = tested(t, store, project, op)
	if err := RefreshIntegrationView(project); err != nil {
		t.Fatal(err)
	}
	view := IntegrationView(project)
	if err := os.WriteFile(filepath.Join(view, "stray.txt"), []byte("?\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(project, op.ID); !errors.Is(err, ErrIntegrationView) {
		t.Fatalf("err=%v", err)
	}
	if branch := branchOf(t, project); branch != op.IntegrationBaseSHA {
		t.Fatalf("branch moved to %s", branch)
	}
	if stored, err := store.Get(op.ID); err != nil || stored.State != Tested {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if err := os.Remove(filepath.Join(view, "stray.txt")); err != nil {
		t.Fatal(err)
	}
	if committed, err := store.Publish(project, op.ID); err != nil || committed.State != Committed {
		t.Fatalf("committed=%+v err=%v", committed, err)
	}
}

func TestRefreshIntegrationViewRefusesUnexplainedStates(t *testing.T) {
	_, project, op := publicationFixture(t)
	view := IntegrationView(project)
	if err := RefreshIntegrationView(project); err != nil {
		t.Fatal(err)
	}
	if head := gitIn(t, view, false, "", "rev-parse", "HEAD"); head != op.IntegrationBaseSHA {
		t.Fatalf("view at %s", head)
	}
	gitIn(t, project.Repository(), true, "", "update-ref", IntegrationBranch, op.ResultSHA)
	if err := RefreshIntegrationView(project); err != nil {
		t.Fatal(err)
	}
	if head := gitIn(t, view, false, "", "rev-parse", "HEAD"); head != op.ResultSHA {
		t.Fatalf("view at %s", head)
	}

	// A commit made in the view is not an ancestor of the branch.
	if err := os.WriteFile(filepath.Join(view, "README.md"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RefreshIntegrationView(project); !errors.Is(err, ErrIntegrationView) {
		t.Fatalf("modified file: %v", err)
	}
	gitIn(t, view, false, "", "-c", "commit.gpgSign=false", "commit", "-q", "-am", "edit")
	if err := RefreshIntegrationView(project); !errors.Is(err, ErrIntegrationView) {
		t.Fatalf("foreign commit: %v", err)
	}
	gitIn(t, view, false, "", "checkout", "-q", "--detach", op.ResultSHA)
	gitIn(t, view, false, "", "checkout", "-q", "-b", "local")
	if err := RefreshIntegrationView(project); !errors.Is(err, ErrIntegrationView) {
		t.Fatalf("on a branch: %v", err)
	}
	gitIn(t, view, false, "", "checkout", "-q", "--detach")
	if err := RefreshIntegrationView(project); err != nil {
		t.Fatalf("clean again: %v", err)
	}
}

func TestRefreshIntegrationViewRefusesAForeignDirectory(t *testing.T) {
	project, _ := canonicalProject(t)
	view := IntegrationView(project)
	if err := os.MkdirAll(view, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RefreshIntegrationView(project); !errors.Is(err, ErrIntegrationView) {
		t.Fatalf("plain directory: %v", err)
	}
	other := t.TempDir()
	gitIn(t, other, false, "", "init", "-q")
	if err := os.Remove(view); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, view); err != nil {
		t.Fatal(err)
	}
	if err := RefreshIntegrationView(project); !errors.Is(err, ErrIntegrationView) {
		t.Fatalf("symbolic link: %v", err)
	}
}
