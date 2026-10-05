// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package testrun

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/handoff"
	"github.com/goabonga/maestro/internal/state"
)

var (
	tested = strings.Repeat("a", 40)
	other  = strings.Repeat("b", 40)
)

func identity() Identity {
	return Identity{ArtifactPrefix: "tests-1", TaskID: "task-1", TurnID: "turn-1", AttemptID: "attempt-1",
		WorkerID: "maestro", ConfigID: "config-1"}
}

func sampleRun() Run {
	return Run{TestedSHA: tested, Results: []Result{
		{Name: "lint", Argv: []string{"make", "lint"}, TestedSHA: tested, Duration: 1500 * time.Millisecond, Output: "ok\n",
			Untracked: []string{"bin/"}},
		{Name: "unit", Argv: []string{"go", "test", "./..."}, TestedSHA: tested, ExitCode: 1, Output: "FAIL\n", Truncated: true,
			Changed: []string{"go.sum"}},
	}}
}

func TestDocumentsAreValidTestReports(t *testing.T) {
	documents, err := sampleRun().Documents(identity())
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 {
		t.Fatalf("%d documents", len(documents))
	}
	envelope, decoded, err := handoff.Decode(documents[0])
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Kind != handoff.TestReport || envelope.ArtifactID != "tests-1-lint" || envelope.WorkerID != "maestro" {
		t.Fatalf("envelope %+v", envelope)
	}
	lint := decoded.(handoff.TestReportPayload)
	if lint.TestedSHA != tested || lint.Name != "lint" || lint.DurationMS != 1500 || lint.Log != "ok\n" ||
		lint.Environment["HOME"] != "/tmp" || strings.Join(lint.UntrackedPaths, ",") != "bin/" || !Passed(lint) {
		t.Fatalf("lint %+v", lint)
	}
	_, decoded, err = handoff.Decode(documents[1])
	if err != nil {
		t.Fatal(err)
	}
	unit := decoded.(handoff.TestReportPayload)
	if unit.ExitCode != 1 || !unit.LogTruncated || strings.Join(unit.ChangedPaths, ",") != "go.sum" || Passed(unit) {
		t.Fatalf("unit %+v", unit)
	}

	bad := identity()
	bad.TaskID = "../escape"
	if _, err := sampleRun().Documents(bad); !errors.Is(err, handoff.ErrInvalid) {
		t.Fatalf("a bad identity: %v", err)
	}
	if _, err := (Run{TestedSHA: tested}).Documents(identity()); !errors.Is(err, ErrNoCommands) {
		t.Fatalf("an empty run: %v", err)
	}
}

func TestAcceptPersistsOneArtifactPerCommand(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	stored, err := Accept(db, sampleRun(), identity())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[1].Envelope.ArtifactID != "tests-1-unit" {
		t.Fatalf("stored %+v", stored)
	}
	loaded, err := handoff.Load(db, "tests-1-lint")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ForRevision(loaded.Document, tested); err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(db, sampleRun(), identity()); !errors.Is(err, handoff.ErrDuplicate) {
		t.Fatalf("a second run under the same ids: %v", err)
	}
}

func TestForRevisionRefusesAnotherSHA(t *testing.T) {
	documents, err := sampleRun().Documents(identity())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := ForRevision(documents[0], tested)
	if err != nil || payload.Name != "lint" {
		t.Fatalf("same revision: %+v %v", payload, err)
	}
	if _, err := ForRevision(documents[0], other); !errors.Is(err, ErrRevision) {
		t.Fatalf("another revision: %v", err)
	}
	if _, err := ForRevision([]byte(`{}`), tested); !errors.Is(err, handoff.ErrInvalid) {
		t.Fatalf("not a document: %v", err)
	}
}

func TestForTaskRefusesReportsOfAnotherConfiguration(t *testing.T) {
	documents, err := sampleRun().Documents(identity())
	if err != nil {
		t.Fatal(err)
	}
	if payload, err := ForTask(documents[0], "task-1", "config-1", tested); err != nil || payload.Name != "lint" {
		t.Fatalf("payload=%+v err=%v", payload, err)
	}
	if _, err := ForTask(documents[0], "task-1", "config-2", tested); !errors.Is(err, handoff.ErrConfig) {
		t.Fatalf("old configuration: %v", err)
	}
	if _, err := ForTask(documents[0], "task-2", "config-1", tested); !errors.Is(err, handoff.ErrInvalid) {
		t.Fatalf("other task: %v", err)
	}
	if _, err := ForTask(documents[0], "task-1", "config-1", other); !errors.Is(err, ErrRevision) {
		t.Fatalf("other revision: %v", err)
	}
	if _, err := ForTask([]byte("{}"), "task-1", "config-1", tested); !errors.Is(err, handoff.ErrInvalid) {
		t.Fatalf("not a document: %v", err)
	}
}
