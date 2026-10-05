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

	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/worktree"
)

// recoveryJournal returns the journal of openJournal with the task t1
// stamped like a stored task, so that it can be read back.
func recoveryJournal(t *testing.T) Store {
	t.Helper()
	store := openJournal(t)
	at := stamp(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if _, err := store.DB.Exec("UPDATE tasks SET created_at = ?, updated_at = ? WHERE task_id = 't1'", at, at); err != nil {
		t.Fatal(err)
	}
	return store
}

// recoveryStarted prepares and starts an integration of the task t1 on
// base in project, with its candidate tree holding content.
func recoveryStarted(t *testing.T, store Store, project worktree.Project, base, content string) Operation {
	t.Helper()
	in := integrationInputs()
	in.ProjectID, in.IntegrationBaseSHA = project.ID, base
	op, err := store.Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(op.ID, ""); err != nil {
		t.Fatal(err)
	}
	if op, err = store.RecordCandidate(op.ID, "", writeTree(t, project.Repository(), content)); err != nil {
		t.Fatal(err)
	}
	return op
}

// recoveryResult builds the result commit of a started integration.
func recoveryResult(t *testing.T, project worktree.Project, op Operation) string {
	t.Helper()
	want, err := op.expected()
	if err != nil {
		t.Fatal(err)
	}
	result, err := BuildResult(project.Repository(), want)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// recoveryApplied returns a journal, a canonical project and an applied
// integration of the task t1, with the task VALIDATING its result.
func recoveryApplied(t *testing.T) (Store, worktree.Project, Operation) {
	t.Helper()
	store := recoveryJournal(t)
	project, base := canonicalProject(t)
	op := recoveryStarted(t, store, project, base, "changed\n")
	op, err := store.ApplyIntegration(project, op.ID, recoveryResult(t, project, op))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec("UPDATE tasks SET state = 'VALIDATING', result_sha = ? WHERE task_id = 't1'", op.ResultSHA); err != nil {
		t.Fatal(err)
	}
	return store, project, op
}

// recoveryTested returns the fixture of recoveryApplied with the
// operation TESTED by one passing report.
func recoveryTested(t *testing.T) (Store, worktree.Project, Operation) {
	t.Helper()
	store, project, op := recoveryApplied(t)
	return store, project, tested(t, store, project, op)
}

// recoveryRun runs the recovery of project and returns its decisions.
func recoveryRun(t *testing.T, store Store, project worktree.Project) []Decision {
	t.Helper()
	decisions, err := store.Recover(project, nil)
	if err != nil {
		t.Fatalf("decisions=%+v err=%v", decisions, err)
	}
	return decisions
}

// recoverySummary renders decisions as action:from>to, with a star when the
// action was applied.
func recoverySummary(decisions []Decision) []string {
	var out []string
	for _, d := range decisions {
		line := string(d.Action) + ":" + string(d.From) + ">" + string(d.To)
		if d.Applied {
			line += "*"
		}
		out = append(out, line)
	}
	return out
}

// recoveryExpect fails unless decisions summarize to want.
func recoveryExpect(t *testing.T, decisions []Decision, want ...string) {
	t.Helper()
	if got := recoverySummary(decisions); !reflect.DeepEqual(got, want) {
		t.Fatalf("decisions %v, want %v\n%+v", got, want, decisions)
	}
}

// recoveryStored returns the journaled operation id.
func recoveryStored(t *testing.T, store Store, id string) Operation {
	t.Helper()
	op, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// recoveryOtherCommit stores a commit of a tree holding content with base as its
// parent and returns its id.
func recoveryOtherCommit(t *testing.T, project worktree.Project, base, content string) string {
	t.Helper()
	tree := writeTree(t, project.Repository(), content)
	return gitIn(t, project.Repository(), true, "", "commit-tree", tree, "-p", base, "-m", "other")
}

// Row 1: no result reference, branch at the base.
func TestRecoveryRebuildsAnIntegrationWithoutResultReference(t *testing.T) {
	f := candidateRepository(t, "base\n", [2]string{"feature.txt", "one\n"}, [2]string{"feature.txt", "two\n"})
	store := recoveryJournal(t)
	op := candidateOperation(t, store, f)
	if _, err := store.Start(op.ID, ""); err != nil {
		t.Fatal(err)
	}
	// The crash: the candidate worktree was added, nothing applied yet.
	path, err := CandidateWorktree(f.project, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.project.Repository(), true, "", "worktree", "add", "--detach", "--quiet", path, f.base)

	decisions := recoveryRun(t, store, f.project)
	recoveryExpect(t, decisions, "rebuild:STARTED>APPLIED*", "retest:APPLIED>APPLIED")
	ref, ok, err := ReadResultRef(f.project.Repository(), op.ID)
	if err != nil || !ok {
		t.Fatalf("no result reference: %v", err)
	}
	if after := recoveryStored(t, store, op.ID); after.ResultSHA != ref || decisions[1].ResultRef != ref {
		t.Fatalf("journal %s, reference %s, decision %s", after.ResultSHA, ref, decisions[1].ResultRef)
	}
	if head := candidateIntegrationHead(t, f); head != f.base {
		t.Fatalf("integration branch moved to %s", head)
	}
	if candidateWorktreeExists(t, f, op.ID) {
		t.Fatal("the candidate worktree was left behind")
	}
}

// Row 1, abandon: the interrupted candidate holds changes.
func TestRecoveryAbandonsAnIntegrationWithADirtyCandidate(t *testing.T) {
	f := candidateRepository(t, "base\n", [2]string{"feature.txt", "one\n"})
	store := recoveryJournal(t)
	op := candidateOperation(t, store, f)
	if _, err := store.Start(op.ID, ""); err != nil {
		t.Fatal(err)
	}
	path, err := CandidateWorktree(f.project, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.project.Repository(), true, "", "worktree", "add", "--detach", "--quiet", path, f.base)
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("half applied\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	recoveryExpect(t, recoveryRun(t, store, f.project), "abandon:STARTED>ROLLED_BACK*")
	if content, err := os.ReadFile(filepath.Join(path, "feature.txt")); err != nil || string(content) != "half applied\n" {
		t.Fatalf("candidate lost: %q %v", content, err)
	}
	if _, ok, err := ReadResultRef(f.project.Repository(), op.ID); err != nil || ok {
		t.Fatalf("result reference created: %v %v", ok, err)
	}
	if head := candidateIntegrationHead(t, f); head != f.base {
		t.Fatalf("integration branch moved to %s", head)
	}
}

// Row 2: valid result reference, SQLite without result_sha.
func TestRecoveryRecordsAppliedFromTheProof(t *testing.T) {
	store := recoveryJournal(t)
	project, base := canonicalProject(t)
	op := recoveryStarted(t, store, project, base, "changed\n")
	result := recoveryResult(t, project, op)
	// The crash: the reference exists, the journal was not written.
	if err := CreateResultRef(project.Repository(), op.ID, result); err != nil {
		t.Fatal(err)
	}

	recoveryExpect(t, recoveryRun(t, store, project), "record-result:STARTED>APPLIED*", "retest:APPLIED>APPLIED")
	if after := recoveryStored(t, store, op.ID); after.ResultSHA != result {
		t.Fatalf("result %q, want %s", after.ResultSHA, result)
	}
	if branch := branchOf(t, project); branch != base {
		t.Fatalf("branch moved to %s", branch)
	}
}

// Row 3: valid result reference, branch at the base, no test evidence.
func TestRecoveryRetestsAnAppliedResult(t *testing.T) {
	store, project, op := recoveryApplied(t)
	decisions := recoveryRun(t, store, project)
	recoveryExpect(t, decisions, "retest:APPLIED>APPLIED")
	if decisions[0].ResultRef != op.ResultSHA || decisions[0].Target != op.IntegrationBaseSHA {
		t.Fatalf("decision %+v", decisions[0])
	}
	if after := recoveryStored(t, store, op.ID); after.Version != op.Version {
		t.Fatalf("operation changed: %+v", after)
	}
	// Recovery is deterministic: the same state yields the same decision.
	recoveryExpect(t, recoveryRun(t, store, project), "retest:APPLIED>APPLIED")
	if d, err := store.DecideRecovery(project, op.ID); err != nil || d.Action != ActionRetest {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
}

// Row 4: valid result reference, branch at the base, TESTED.
func TestRecoveryRetriesThePublicationOnce(t *testing.T) {
	store, project, op := recoveryTested(t)
	recoveryExpect(t, recoveryRun(t, store, project), "retry-publication:TESTED>COMMITTED*")
	if branch := branchOf(t, project); branch != op.ResultSHA {
		t.Fatalf("branch at %s, result %s", branch, op.ResultSHA)
	}
	if state := taskState(t, store); state != task.Done {
		t.Fatalf("task %s", state)
	}
	if head := gitIn(t, IntegrationView(project), false, "", "rev-parse", "HEAD"); head != op.ResultSHA {
		t.Fatalf("view at %s", head)
	}
	recoveryExpect(t, recoveryRun(t, store, project))
}

// Row 4: a failed retry blocks and is not repeated within the recovery.
func TestRecoveryBlocksAFailedRetry(t *testing.T) {
	store, project, op := recoveryTested(t)
	if err := RefreshIntegrationView(project); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(IntegrationView(project), "stray.txt")
	if err := os.WriteFile(stray, []byte("?\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	decisions := recoveryRun(t, store, project)
	recoveryExpect(t, decisions, "block:TESTED>TESTED")
	if !errors.Is(decisions[0].Err, ErrRecoveryBlocked) || !errors.Is(decisions[0].Err, ErrIntegrationView) {
		t.Fatalf("err %v", decisions[0].Err)
	}
	if branch := branchOf(t, project); branch != op.IntegrationBaseSHA {
		t.Fatalf("branch moved to %s", branch)
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	recoveryExpect(t, recoveryRun(t, store, project), "retry-publication:TESTED>COMMITTED*")
}

// Row 5: branch at the result, valid reference, TESTED.
func TestRecoveryFinalizesAPublishedResult(t *testing.T) {
	store, project, op := recoveryTested(t)
	// The crash: the branch moved, SQLite was not committed.
	gitIn(t, project.Repository(), true, "", "update-ref", IntegrationBranch, op.ResultSHA, op.IntegrationBaseSHA)
	recoveryExpect(t, recoveryRun(t, store, project), "finalize:TESTED>COMMITTED*")
	if state := taskState(t, store); state != task.Done {
		t.Fatalf("task %s", state)
	}
	if got := events(t, store, op.ID); got[len(got)-1] != "commit:TESTED>COMMITTED" {
		t.Fatalf("events %v", got)
	}
}

// Row 6: branch at the base, operation FAILED.
func TestRecoveryRollsBackAFailedOperation(t *testing.T) {
	store, project, op := recoveryApplied(t)
	if _, err := store.Fail(op.ID, "tests interrupted"); err != nil {
		t.Fatal(err)
	}
	path, err := CandidateWorktree(project, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	diagnostic := filepath.Join(path, "conflict.log")
	if err := os.WriteFile(diagnostic, []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	recoveryExpect(t, recoveryRun(t, store, project), "roll-back:FAILED>ROLLED_BACK*")
	if _, err := os.Stat(diagnostic); err != nil {
		t.Fatalf("diagnostic lost: %v", err)
	}
	if sha, ok, err := ReadResultRef(project.Repository(), op.ID); err != nil || !ok || sha != op.ResultSHA {
		t.Fatalf("result reference %s %v %v", sha, ok, err)
	}
	if branch := branchOf(t, project); branch != op.IntegrationBaseSHA {
		t.Fatalf("branch moved to %s", branch)
	}
}

// Row 7: unexpected branch.
func TestRecoveryBlocksAnUnexpectedBranch(t *testing.T) {
	store, project, op := recoveryTested(t)
	other := recoveryOtherCommit(t, project, op.IntegrationBaseSHA, "manual\n")
	gitIn(t, project.Repository(), true, "", "update-ref", IntegrationBranch, other)

	decisions := recoveryRun(t, store, project)
	recoveryExpect(t, decisions, "block:TESTED>TESTED")
	if !errors.Is(decisions[0].Err, ErrRecoveryBlocked) || !strings.Contains(decisions[0].Diagnostic, other) {
		t.Fatalf("decision %+v", decisions[0])
	}
	if branch := branchOf(t, project); branch != other {
		t.Fatalf("branch rewritten to %s", branch)
	}
	if after := recoveryStored(t, store, op.ID); after.Version != op.Version {
		t.Fatalf("operation changed: %+v", after)
	}
}

// Row 7: inconsistent proof.
func TestRecoveryBlocksAnInconsistentProof(t *testing.T) {
	store := recoveryJournal(t)
	project, base := canonicalProject(t)
	op := recoveryStarted(t, store, project, base, "changed\n")
	if err := CreateResultRef(project.Repository(), op.ID, base); err != nil {
		t.Fatal(err)
	}

	decisions := recoveryRun(t, store, project)
	recoveryExpect(t, decisions, "block:STARTED>STARTED")
	if !errors.Is(decisions[0].Err, ErrInvalidResult) {
		t.Fatalf("err %v", decisions[0].Err)
	}
	if sha, _, _ := ReadResultRef(project.Repository(), op.ID); sha != base {
		t.Fatalf("the reference was rewritten to %s", sha)
	}
}

// Row 7: publication without TESTED.
func TestRecoveryBlocksAPublicationWithoutTests(t *testing.T) {
	store, project, op := recoveryApplied(t)
	gitIn(t, project.Repository(), true, "", "update-ref", IntegrationBranch, op.ResultSHA, op.IntegrationBaseSHA)

	recoveryExpect(t, recoveryRun(t, store, project), "block:APPLIED>APPLIED")
	if branch := branchOf(t, project); branch != op.ResultSHA {
		t.Fatalf("branch rewritten to %s", branch)
	}
	if state := taskState(t, store); state != task.Validating {
		t.Fatalf("task %s", state)
	}
}

// Row 8: COMMITTED, integration view stale and clean.
func TestRecoveryRefreshesAStaleView(t *testing.T) {
	store, project, op := recoveryTested(t)
	if _, err := store.Publish(project, op.ID); err != nil {
		t.Fatal(err)
	}
	view := IntegrationView(project)
	// The crash: the finalization was committed, the view not refreshed.
	gitIn(t, view, false, "", "checkout", "-q", "--detach", op.IntegrationBaseSHA)
	before := events(t, store, op.ID)

	recoveryExpect(t, recoveryRun(t, store, project), "refresh-view:COMMITTED>COMMITTED*")
	if head := gitIn(t, view, false, "", "rev-parse", "HEAD"); head != op.ResultSHA {
		t.Fatalf("view at %s", head)
	}
	if after := events(t, store, op.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("the integration was replayed: %v", after)
	}

	gitIn(t, view, false, "", "checkout", "-q", "--detach", op.IntegrationBaseSHA)
	if err := os.WriteFile(filepath.Join(view, "README.md"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	decisions := recoveryRun(t, store, project)
	recoveryExpect(t, decisions, "block:COMMITTED>COMMITTED")
	if !errors.Is(decisions[0].Err, ErrIntegrationView) {
		t.Fatalf("err %v", decisions[0].Err)
	}
}

// An abandoned operation is never republished.
func TestRecoveryNeverPublishesAnAbandonedOperation(t *testing.T) {
	store, project, op := recoveryTested(t)
	if _, err := store.DB.Exec("UPDATE tasks SET state = 'CANCELLED' WHERE task_id = 't1'"); err != nil {
		t.Fatal(err)
	}
	recoveryExpect(t, recoveryRun(t, store, project), "abandon:TESTED>ROLLED_BACK*")
	if branch := branchOf(t, project); branch != op.IntegrationBaseSHA {
		t.Fatalf("branch moved to %s", branch)
	}

	store, project, op = recoveryTested(t)
	in := integrationInputs()
	in.ProjectID, in.IntegrationBaseSHA, in.SupersedesOperationID = project.ID, op.IntegrationBaseSHA, op.ID
	next, err := store.Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fail(next.ID, "replaced"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RollBack(next.ID, "replaced"); err != nil {
		t.Fatal(err)
	}
	decisions := recoveryRun(t, store, project)
	recoveryExpect(t, decisions, "abandon:TESTED>ROLLED_BACK*")
	if !strings.Contains(decisions[0].Diagnostic, next.ID) {
		t.Fatalf("diagnostic %q", decisions[0].Diagnostic)
	}
	if branch := branchOf(t, project); branch != op.IntegrationBaseSHA {
		t.Fatalf("branch moved to %s", branch)
	}
}

// A new integration base requires a new operation: the old one is
// abandoned without publication.
func TestRecoveryAbandonsAnOperationOnAStaleBase(t *testing.T) {
	store, project, first := recoveryTested(t)
	if _, err := store.Publish(project, first.ID); err != nil {
		t.Fatal(err)
	}
	second := recoveryStarted(t, store, project, first.IntegrationBaseSHA, "other\n")
	second, err := store.ApplyIntegration(project, second.ID, recoveryResult(t, project, second))
	if err != nil {
		t.Fatal(err)
	}

	recoveryExpect(t, recoveryRun(t, store, project), "abandon:APPLIED>ROLLED_BACK*")
	if branch := branchOf(t, project); branch != first.ResultSHA {
		t.Fatalf("branch moved to %s", branch)
	}
}

// recoverySyncStarted prepares and starts a SYNC of project from base to a new
// commit descending from it, and records its candidate.
func recoverySyncStarted(t *testing.T, store Store, project worktree.Project, base string) Operation {
	t.Helper()
	synced := recoveryOtherCommit(t, project, base, "synced\n")
	op, err := store.Prepare(Operation{
		Type: Sync, ProjectID: project.ID, ConfigID: "sha256-x", WorkerID: "daemon", AttemptID: "a1",
		IntegrationBaseSHA: base, SourceHeadSHA: synced,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(op.ID, ""); err != nil {
		t.Fatal(err)
	}
	tree := gitIn(t, project.Repository(), true, "", "rev-parse", synced+"^{tree}")
	if op, err = store.RecordCandidate(op.ID, "refs/maestro/sync/"+op.ID, tree); err != nil {
		t.Fatal(err)
	}
	return op
}

// recoverySyncApplied pins the imported commit of a started sync in its result
// reference and records it.
func recoverySyncApplied(t *testing.T, store Store, project worktree.Project, op Operation) Operation {
	t.Helper()
	if err := CreateResultRef(project.Repository(), op.ID, op.SourceHeadSHA); err != nil {
		t.Fatal(err)
	}
	op, err := store.RecordResult(op.ID, op.SourceHeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// recoverySyncTested returns a SYNC operation TESTED by one passing report.
func recoverySyncTested(t *testing.T) (Store, worktree.Project, Operation) {
	t.Helper()
	store := recoveryJournal(t)
	project, base := canonicalProject(t)
	op := recoverySyncApplied(t, store, project, recoverySyncStarted(t, store, project, base))
	op, err := store.MarkTested(op.ID, Evidence{TestReportIDs: []string{acceptReport(t, store, "unit", "sha256-x", op.ResultSHA, 0)}})
	if err != nil {
		t.Fatal(err)
	}
	return store, project, op
}

func TestRecoverySyncFinalizesAtTheImportedCommit(t *testing.T) {
	store, project, op := recoverySyncTested(t)
	gitIn(t, project.Repository(), true, "", "update-ref", IntegrationBranch, op.ResultSHA, op.IntegrationBaseSHA)
	recoveryExpect(t, recoveryRun(t, store, project), "finalize:TESTED>COMMITTED*")
	if head := gitIn(t, IntegrationView(project), false, "", "rev-parse", "HEAD"); head != op.ResultSHA {
		t.Fatalf("view at %s", head)
	}
}

func TestRecoverySyncRetriesFromItsBase(t *testing.T) {
	store, project, op := recoverySyncTested(t)
	recoveryExpect(t, recoveryRun(t, store, project), "retry-publication:TESTED>COMMITTED*")
	if branch := branchOf(t, project); branch != op.ResultSHA {
		t.Fatalf("branch at %s", branch)
	}
}

func TestRecoverySyncRecordsItsPinnedImport(t *testing.T) {
	store := recoveryJournal(t)
	project, base := canonicalProject(t)
	op := recoverySyncStarted(t, store, project, base)
	if err := CreateResultRef(project.Repository(), op.ID, op.SourceHeadSHA); err != nil {
		t.Fatal(err)
	}
	recoveryExpect(t, recoveryRun(t, store, project), "record-result:STARTED>APPLIED*", "retest:APPLIED>APPLIED")
}

func TestRecoverySyncAbandonsAnUnpinnedImport(t *testing.T) {
	store := recoveryJournal(t)
	project, base := canonicalProject(t)
	recoverySyncStarted(t, store, project, base)
	recoveryExpect(t, recoveryRun(t, store, project), "abandon:STARTED>ROLLED_BACK*")
	if branch := branchOf(t, project); branch != base {
		t.Fatalf("branch moved to %s", branch)
	}
}

func TestRecoverySyncRollsBackOrBlocks(t *testing.T) {
	store, project, op := recoverySyncTested(t)
	if _, err := store.Fail(op.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	recoveryExpect(t, recoveryRun(t, store, project), "roll-back:FAILED>ROLLED_BACK*")

	// Published without TESTED.
	store = recoveryJournal(t)
	project, base := canonicalProject(t)
	op = recoverySyncApplied(t, store, project, recoverySyncStarted(t, store, project, base))
	gitIn(t, project.Repository(), true, "", "update-ref", IntegrationBranch, op.ResultSHA, base)
	recoveryExpect(t, recoveryRun(t, store, project), "block:APPLIED>APPLIED")

	// Unexpected branch.
	store, project, op = recoverySyncTested(t)
	other := recoveryOtherCommit(t, project, op.IntegrationBaseSHA, "manual\n")
	gitIn(t, project.Repository(), true, "", "update-ref", IntegrationBranch, other)
	recoveryExpect(t, recoveryRun(t, store, project), "block:TESTED>TESTED")
	if branch := branchOf(t, project); branch != other {
		t.Fatalf("branch rewritten to %s", branch)
	}
}

// recoveryPublishStarted publishes a tested integration privately and starts a
// PUBLISH operation of its result to the user repository, whose
// published branch does not exist yet. It returns the committed
// integration too.
func recoveryPublishStarted(t *testing.T) (Store, worktree.Project, Operation, Operation) {
	t.Helper()
	store, project, integration := recoveryTested(t)
	integration, err := store.Publish(project, integration.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := integration.ResultSHA
	op, err := store.Prepare(Operation{
		Type: Publish, ProjectID: project.ID, ConfigID: "sha256-x", WorkerID: "maestro-svc", AttemptID: "a1",
		IntegrationBaseSHA: strings.Repeat("0", len(target)), SourceHeadSHA: target, SourceCommits: []string{target},
	})
	if err != nil {
		t.Fatal(err)
	}
	if op, err = store.Start(op.ID, ""); err != nil {
		t.Fatal(err)
	}
	return store, project, integration, op
}

// recoveryUserHead returns the published branch of the user repository, empty
// when absent.
func recoveryUserHead(t *testing.T, project worktree.Project) string {
	t.Helper()
	sha, err := recoveryUserBranch(project.UserRepository)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestRecoveryPublishFinalizesAtTheTarget(t *testing.T) {
	store, project, integration, op := recoveryPublishStarted(t)
	// The crash: the objects were transferred and the branch updated.
	gitIn(t, project.UserRepository, true, "", "fetch", "-q", "--no-write-fetch-head", project.Repository(),
		IntegrationBranch+":"+IntegrationBranch)

	recoveryExpect(t, recoveryRun(t, store, project), "finalize:STARTED>COMMITTED*")
	after := recoveryStored(t, store, op.ID)
	if after.ResultSHA != integration.ResultSHA || !reflect.DeepEqual(after.TestReportIDs, integration.TestReportIDs) {
		t.Fatalf("publication %+v", after)
	}
}

func TestRecoveryPublishAbandonsAtTheOldValue(t *testing.T) {
	store, project, _, _ := recoveryPublishStarted(t)
	recoveryExpect(t, recoveryRun(t, store, project), "abandon:STARTED>ROLLED_BACK*")
	if sha := recoveryUserHead(t, project); sha != "" {
		t.Fatalf("user branch created at %s", sha)
	}
}

func TestRecoveryPublishBlocksAnotherValue(t *testing.T) {
	store, project, integration, op := recoveryPublishStarted(t)
	gitIn(t, project.UserRepository, true, "", "update-ref", IntegrationBranch, "HEAD")
	userHead := recoveryUserHead(t, project)

	decisions := recoveryRun(t, store, project)
	recoveryExpect(t, decisions, "block:STARTED>STARTED")
	if decisions[0].Target != userHead || userHead == integration.ResultSHA {
		t.Fatalf("decision %+v", decisions[0])
	}
	if sha := recoveryUserHead(t, project); sha != userHead {
		t.Fatalf("user branch rewritten to %s", sha)
	}
	if after := recoveryStored(t, store, op.ID); after.Version != op.Version {
		t.Fatalf("operation changed: %+v", after)
	}
}
