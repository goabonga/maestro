// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/state"
)

// Object ids used where no repository is involved.
var (
	shaBase   = strings.Repeat("a", 40)
	shaTask   = strings.Repeat("b", 40)
	shaHead   = strings.Repeat("c", 40)
	shaTree   = strings.Repeat("d", 40)
	shaResult = strings.Repeat("e", 40)
)

// openJournal returns an operations store on a migrated database that
// holds the configuration snapshot "sha256-x" and the task "t1".
func openJournal(t *testing.T) Store {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO config_snapshots (config_id, created_at, document) VALUES ('sha256-x', 'now', '{}')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (task_id, config_id, task_base_sha, branch, state, version, max_fix_cycles, created_at, updated_at)
		VALUES ('t1', 'sha256-x', ?, 'maestro/task-t1', 'INTEGRATING', 1, 3, 'now', 'now')`, shaTask); err != nil {
		t.Fatal(err)
	}
	return Store{DB: db}
}

// frozenMetadata returns valid commit metadata at a fixed instant.
func frozenMetadata() *CommitMetadata {
	when := time.Date(2026, 10, 5, 12, 30, 0, 0, time.FixedZone("", 2*3600))
	return &CommitMetadata{
		Message:   "feat: add a flag",
		Author:    Signature{Name: "Ada Lovelace", Email: "ada@example.com", When: when},
		Committer: Signature{Name: "Maestro", Email: "maestro@localhost", When: when.Add(time.Minute)},
	}
}

// integrationInputs returns the inputs of a valid INTEGRATE operation.
func integrationInputs() Operation {
	return Operation{
		Type: Integrate, ProjectID: "p1", ConfigID: "sha256-x", TaskID: "t1", WorkerID: "w1", AttemptID: "a1",
		TaskBaseSHA: shaTask, IntegrationBaseSHA: shaBase, SourceHeadSHA: shaHead,
		SourceCommits: []string{shaTask[:39] + "f", shaHead}, CommitMetadata: frozenMetadata(),
	}
}

// syncInputs returns the inputs of a valid SYNC operation.
func syncInputs() Operation {
	return Operation{Type: Sync, ProjectID: "p1", ConfigID: "sha256-x", WorkerID: "w1", AttemptID: "a1", IntegrationBaseSHA: shaBase}
}

// events returns the event names of an operation.
func events(t *testing.T, store Store, id string) []string {
	t.Helper()
	records, err := store.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range records {
		names = append(names, r.Event+":"+string(r.From)+">"+string(r.To))
	}
	return names
}

func TestTransitionTablePerType(t *testing.T) {
	for _, typ := range []Type{Integrate, Sync} {
		path := []State{Prepared, Started, Applied, Tested, Committed}
		for i := 0; i+1 < len(path); i++ {
			if !Allowed(typ, path[i], path[i+1]) {
				t.Errorf("%s: %s -> %s refused", typ, path[i], path[i+1])
			}
		}
	}
	if Allowed(Publish, Applied, Tested) || !Allowed(Publish, Applied, Committed) {
		t.Error("PUBLISH does not skip TESTED")
	}
	for _, typ := range []Type{Integrate, Sync, Publish} {
		for _, from := range []State{Prepared, Started, Applied, Tested} {
			if typ == Publish && from == Tested {
				continue
			}
			if !Allowed(typ, from, Failed) {
				t.Errorf("%s: %s cannot fail", typ, from)
			}
		}
		if !Allowed(typ, Failed, RolledBack) || Allowed(typ, Failed, Committed) || Allowed(typ, Started, Committed) {
			t.Errorf("%s: a failed or unfinished operation can succeed", typ)
		}
		for _, terminal := range []State{Committed, RolledBack} {
			if !terminal.Terminal() || len(transitions[typ][terminal]) != 0 {
				t.Errorf("%s: %s is not terminal", typ, terminal)
			}
		}
	}
}

func TestPrepareRefusesIncompleteInputs(t *testing.T) {
	store := openJournal(t)
	cases := map[string]func(*Operation){
		"unknown type":        func(op *Operation) { op.Type = "MERGE" },
		"no project":          func(op *Operation) { op.ProjectID = "" },
		"no worker":           func(op *Operation) { op.WorkerID = "" },
		"bad base":            func(op *Operation) { op.IntegrationBaseSHA = "main" },
		"bad source commit":   func(op *Operation) { op.SourceCommits = []string{"HEAD", shaHead} },
		"chain not at head":   func(op *Operation) { op.SourceCommits = []string{shaHead, shaTask} },
		"no task":             func(op *Operation) { op.TaskID = "" },
		"no metadata":         func(op *Operation) { op.CommitMetadata = nil },
		"empty message":       func(op *Operation) { op.CommitMetadata.Message = "\n\n" },
		"name with crud":      func(op *Operation) { op.CommitMetadata.Author.Name = "Ada " },
		"email with brackets": func(op *Operation) { op.CommitMetadata.Committer.Email = "a<b>@c" },
		"sub-second date": func(op *Operation) {
			op.CommitMetadata.Author.When = op.CommitMetadata.Author.When.Add(time.Millisecond)
		},
		"result set in advance":   func(op *Operation) { op.ResultSHA = shaResult },
		"state set in advance":    func(op *Operation) { op.State = Committed },
		"bad superseded id":       func(op *Operation) { op.SupersedesOperationID = "previous" },
		"unknown superseded":      func(op *Operation) { op.SupersedesOperationID = newID() },
		"unknown config snapshot": func(op *Operation) { op.ConfigID = "sha256-missing" },
	}
	for name, mutate := range cases {
		op := integrationInputs()
		mutate(&op)
		if _, err := store.Prepare(op); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if ops, err := store.List("p1"); err != nil || len(ops) != 0 {
		t.Fatalf("refused operations were stored: %v %v", ops, err)
	}
}

func TestPrepareFreezesTheInputs(t *testing.T) {
	store := openJournal(t)
	op, err := store.Prepare(integrationInputs())
	if err != nil {
		t.Fatal(err)
	}
	if op.State != Prepared || op.Version != 1 || !operationID.MatchString(op.ID) {
		t.Fatalf("prepared %+v", op)
	}
	stored, err := store.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CommitMetadata.Message != "feat: add a flag\n" {
		t.Fatalf("message %q", stored.CommitMetadata.Message)
	}
	want := frozenMetadata()
	if !stored.CommitMetadata.Author.When.Equal(want.Author.When) {
		t.Fatalf("author date %s", stored.CommitMetadata.Author.When)
	}
	if _, offset := stored.CommitMetadata.Author.When.Zone(); offset != 2*3600 {
		t.Fatalf("the author offset was not kept: %d", offset)
	}
	if !reflect.DeepEqual(stored.SourceCommits, op.SourceCommits) || stored.TaskID != "t1" || stored.IntegrationBaseSHA != shaBase {
		t.Fatalf("stored %+v", stored)
	}
	if got := events(t, store, op.ID); !reflect.DeepEqual(got, []string{"prepare:>PREPARED"}) {
		t.Fatalf("events %v", got)
	}
}

func TestSyncGoesFromPreparedToCommitted(t *testing.T) {
	store := openJournal(t)
	op, err := store.Prepare(syncInputs())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(op.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordResult(op.ID, shaResult); !errors.Is(err, ErrGuard) {
		t.Fatalf("a result without its candidate tree: %v", err)
	}
	if _, err := store.RecordCandidate(op.ID, "refs/maestro/candidates/x", shaTree); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(op.ID, Evidence{TestReportIDs: []string{"r1"}}); !errors.Is(err, ErrTransition) {
		t.Fatalf("committed before being tested: %v", err)
	}
	if _, err := store.RecordResult(op.ID, shaResult); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkTested(op.ID, Evidence{}); !errors.Is(err, ErrGuard) {
		t.Fatalf("tested without a report: %v", err)
	}
	if _, err := store.MarkTested(op.ID, Evidence{TestReportIDs: []string{"r1"}, ReviewArtifactIDs: []string{"v1"}}); err != nil {
		t.Fatal(err)
	}
	done, err := store.Commit(op.ID, Evidence{TestReportIDs: []string{"r1"}, ApprovalArtifactIDs: []string{"h1"}})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, done) {
		t.Fatalf("stored %+v, returned %+v", stored, done)
	}
	if stored.State != Committed || stored.Version != 6 || stored.ResultSHA != shaResult ||
		!reflect.DeepEqual(stored.TestReportIDs, []string{"r1"}) || !reflect.DeepEqual(stored.ApprovalArtifactIDs, []string{"h1"}) {
		t.Fatalf("committed %+v", stored)
	}
	want := []string{
		"prepare:>PREPARED", "start:PREPARED>STARTED", "record-candidate:STARTED>STARTED",
		"apply:STARTED>APPLIED", "test:APPLIED>TESTED", "commit:TESTED>COMMITTED",
	}
	if got := events(t, store, op.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v", got)
	}
	if _, err := store.Fail(op.ID, "late"); !errors.Is(err, ErrTransition) {
		t.Fatalf("a committed operation failed: %v", err)
	}
}

func TestPublishSkipsTested(t *testing.T) {
	store := openJournal(t)
	in := syncInputs()
	in.Type = Publish
	op, err := store.Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(op.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordResult(op.ID, shaResult); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkTested(op.ID, Evidence{TestReportIDs: []string{"r1"}}); !errors.Is(err, ErrTransition) {
		t.Fatalf("a publication was tested: %v", err)
	}
	if _, err := store.Commit(op.ID, Evidence{}); !errors.Is(err, ErrGuard) {
		t.Fatalf("a publication committed without evidence: %v", err)
	}
	if op, err := store.Commit(op.ID, Evidence{TestReportIDs: []string{"r1"}}); err != nil || op.State != Committed {
		t.Fatalf("op=%+v err=%v", op, err)
	}
}

func TestIntegrationResultNeedsItsReference(t *testing.T) {
	store := openJournal(t)
	op, err := store.Prepare(integrationInputs())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(op.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordCandidate(op.ID, "", shaTree); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordResult(op.ID, shaResult); !errors.Is(err, ErrGuard) {
		t.Fatalf("an integration result was recorded without its reference: %v", err)
	}
	if stored, err := store.Get(op.ID); err != nil || stored.State != Started || stored.ResultSHA != "" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
}

func TestCandidateTreeIsFrozen(t *testing.T) {
	store := openJournal(t)
	op, err := store.Prepare(syncInputs())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordCandidate(op.ID, "", shaTree); !errors.Is(err, ErrTransition) {
		t.Fatalf("a candidate was recorded before the start: %v", err)
	}
	if _, err := store.Start(op.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordCandidate(op.ID, "refs/maestro/operations/x/result", shaTree); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a candidate in the result namespace: %v", err)
	}
	if _, err := store.RecordCandidate(op.ID, "refs/heads/../x", shaTree); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a malformed candidate reference: %v", err)
	}
	first, err := store.RecordCandidate(op.ID, "", shaTree)
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.RecordCandidate(op.ID, "", shaTree)
	if err != nil || again.Version != first.Version {
		t.Fatalf("recording the same tree again: version %d -> %d, %v", first.Version, again.Version, err)
	}
	if _, err := store.RecordCandidate(op.ID, "", shaResult); !errors.Is(err, ErrGuard) {
		t.Fatalf("the tree was replaced: %v", err)
	}
	if stored, _ := store.Get(op.ID); stored.CandidateTreeSHA != shaTree {
		t.Fatalf("tree %s", stored.CandidateTreeSHA)
	}
}

func TestFailureNeverTurnsIntoSuccess(t *testing.T) {
	store := openJournal(t)
	op, err := store.Prepare(syncInputs())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fail(op.ID, "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a failure without its error: %v", err)
	}
	if _, err := store.Start(op.ID, ""); err != nil {
		t.Fatal(err)
	}
	failed, err := store.Fail(op.ID, "tests failed")
	if err != nil || failed.State != Failed || failed.Error != "tests failed" {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}
	for name, step := range map[string]func() (Operation, error){
		"start":  func() (Operation, error) { return store.Start(op.ID, "") },
		"result": func() (Operation, error) { return store.RecordResult(op.ID, shaResult) },
		"test":   func() (Operation, error) { return store.MarkTested(op.ID, Evidence{TestReportIDs: []string{"r"}}) },
		"commit": func() (Operation, error) { return store.Commit(op.ID, Evidence{TestReportIDs: []string{"r"}}) },
	} {
		if _, err := step(); !errors.Is(err, ErrTransition) {
			t.Errorf("%s after a failure: %v", name, err)
		}
	}
	rolled, err := store.RollBack(op.ID, "candidate abandoned")
	if err != nil || rolled.State != RolledBack || rolled.Error != "tests failed" {
		t.Fatalf("rolled=%+v err=%v", rolled, err)
	}
	if _, err := store.Fail(op.ID, "again"); !errors.Is(err, ErrTransition) {
		t.Fatalf("a rolled back operation failed again: %v", err)
	}
}

func TestConcurrentTransitionLoses(t *testing.T) {
	store := openJournal(t)
	op, err := store.Prepare(syncInputs())
	if err != nil {
		t.Fatal(err)
	}
	other := Store{DB: store.DB}
	racing := Store{DB: store.DB, Now: func() time.Time {
		// Another writer moves the operation between the read and the
		// compare-and-set of this transition.
		if _, err := other.Fail(op.ID, "aborted"); err != nil {
			t.Fatal(err)
		}
		return time.Now()
	}}
	if _, err := racing.Start(op.ID, ""); !errors.Is(err, ErrTransition) {
		t.Fatalf("the losing transition: %v", err)
	}
	stored, err := store.Get(op.ID)
	if err != nil || stored.State != Failed {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if got := events(t, store, op.ID); !reflect.DeepEqual(got, []string{"prepare:>PREPARED", "fail:PREPARED>FAILED"}) {
		t.Fatalf("events %v", got)
	}
}

func TestSupersedingStaysInTheSameTask(t *testing.T) {
	store := openJournal(t)
	first, err := store.Prepare(integrationInputs())
	if err != nil {
		t.Fatal(err)
	}
	other := syncInputs()
	other.SupersedesOperationID = first.ID
	if _, err := store.Prepare(other); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a sync superseded an integration: %v", err)
	}
	retry := integrationInputs()
	retry.SupersedesOperationID = first.ID
	second, err := store.Prepare(retry)
	if err != nil {
		t.Fatal(err)
	}
	if second.SupersedesOperationID != first.ID {
		t.Fatalf("supersedes %q", second.SupersedesOperationID)
	}
	if ops, err := store.List("p1"); err != nil || len(ops) != 2 {
		t.Fatalf("ops=%d err=%v", len(ops), err)
	}
}

func TestUnknownOperation(t *testing.T) {
	store := openJournal(t)
	if _, err := store.Start(newID(), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}
