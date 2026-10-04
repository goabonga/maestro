// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package handoff

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repairAssignment is the failed assignment's repair: a new attempt.
func repairAssignment() Assignment {
	repair := assignment(Plan)
	repair.AttemptID = "attempt-2"
	return repair
}

// repairDocument is a valid plan for the repair attempt.
func repairDocument(t *testing.T) []byte {
	t.Helper()
	return planDocument(t, func(e map[string]any) { e["attempt_id"] = "attempt-2" })
}

// failedTurn returns a repository whose turn left an invalid document,
// and the verdict asking for a repair.
func failedTurn(t *testing.T) (string, Verdict) {
	t.Helper()
	worktree, _, _ := history(t)
	if err := Write(worktree, "turn-1", "attempt-1", []byte(`{"schema_version": 1}`)); err != nil {
		t.Fatal(err)
	}
	verdict, err := EndTurn(worktree, assignment(Plan))
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Outcome != RepairRequested || verdict.Snapshot.Head == "" {
		t.Fatalf("verdict %+v", verdict)
	}
	return worktree, verdict
}

// gitIn runs git in a worktree.
func gitIn(t *testing.T, worktree string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = worktree
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func TestEndTurnAcceptsAValidDocument(t *testing.T) {
	worktree, _, _ := history(t)
	if err := Write(worktree, "turn-1", "attempt-1", planDocument(t, nil)); err != nil {
		t.Fatal(err)
	}
	verdict, err := EndTurn(worktree, assignment(Plan))
	if err != nil || verdict.Outcome != Accepted || verdict.Envelope.ArtifactID != "art-1" {
		t.Fatalf("verdict %+v, %v", verdict, err)
	}
}

func TestEndTurnAsksOneRepairForAMissingDocument(t *testing.T) {
	worktree, _, _ := history(t)
	verdict, err := EndTurn(worktree, assignment(Plan))
	if err != nil || verdict.Outcome != RepairRequested || !strings.Contains(verdict.Reason, "no handoff document") {
		t.Fatalf("verdict %+v, %v", verdict, err)
	}
}

func TestEndTurnBlocksOnAmbiguousErrors(t *testing.T) {
	verdict, err := EndTurn(filepath.Join(t.TempDir(), "gone"), assignment(Plan))
	if err != nil || verdict.Outcome != Blocked || !strings.Contains(verdict.Reason, "ambiguous") {
		t.Fatalf("verdict %+v, %v", verdict, err)
	}
}

func TestRepairWithoutCodeChangesIsAccepted(t *testing.T) {
	worktree, failed := failedTurn(t)
	if err := Write(worktree, "turn-1", "attempt-2", repairDocument(t)); err != nil {
		t.Fatal(err)
	}
	verdict, err := EndRepair(worktree, assignment(Plan), repairAssignment(), failed.Snapshot)
	if err != nil || verdict.Outcome != Accepted || verdict.Envelope.AttemptID != "attempt-2" {
		t.Fatalf("verdict %+v, %v", verdict, err)
	}
}

func TestRepairThatTouchesTheCodeIsBlocked(t *testing.T) {
	for name, tamper := range map[string]func(t *testing.T, worktree string){
		"edited file": func(t *testing.T, worktree string) {
			if err := os.WriteFile(filepath.Join(worktree, "first"), []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"new file": func(t *testing.T, worktree string) {
			if err := os.WriteFile(filepath.Join(worktree, "added.go"), []byte("package x"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"staged file": func(t *testing.T, worktree string) {
			if err := os.WriteFile(filepath.Join(worktree, "staged"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitIn(t, worktree, "add", "staged")
		},
		"new commit": func(t *testing.T, worktree string) {
			gitIn(t, worktree, "commit", "-q", "--allow-empty", "-m", "fix: sneaky")
		},
	} {
		worktree, failed := failedTurn(t)
		tamper(t, worktree)
		if err := Write(worktree, "turn-1", "attempt-2", repairDocument(t)); err != nil {
			t.Fatal(err)
		}
		verdict, err := EndRepair(worktree, assignment(Plan), repairAssignment(), failed.Snapshot)
		if err != nil || verdict.Outcome != Blocked {
			t.Fatalf("%s: verdict %+v, %v", name, verdict, err)
		}
	}
}

func TestASecondFailureIsBlocked(t *testing.T) {
	worktree, failed := failedTurn(t)
	if err := Write(worktree, "turn-1", "attempt-2", []byte(`not json`)); err != nil {
		t.Fatal(err)
	}
	verdict, err := EndRepair(worktree, assignment(Plan), repairAssignment(), failed.Snapshot)
	if err != nil || verdict.Outcome != Blocked || !strings.Contains(verdict.Reason, "failed too") {
		t.Fatalf("verdict %+v, %v", verdict, err)
	}
}

func TestRepairMustUseANewAttempt(t *testing.T) {
	worktree, failed := failedTurn(t)
	if _, err := EndRepair(worktree, assignment(Plan), assignment(Plan), failed.Snapshot); !errors.Is(err, ErrInvalid) {
		t.Fatalf("error %v", err)
	}
}
