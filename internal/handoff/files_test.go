// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package handoff

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// planDocument is a valid plan for the default assignment.
func planDocument(t *testing.T, change func(map[string]any)) []byte {
	t.Helper()
	return document(t, Plan, PlanPayload{Body: planBody}, change)
}

func TestWriteThenReadRoundTrip(t *testing.T) {
	worktree := t.TempDir()
	data := planDocument(t, nil)
	if err := Write(worktree, "turn-1", "attempt-1", data); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".maestro", "handoff", "turn-1", "attempt-1.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("the temporary file was left behind")
	}
	envelope, payload, raw, err := Read(worktree, assignment(Plan))
	if err != nil {
		t.Fatal(err)
	}
	if envelope.ArtifactID != "art-1" || !bytes.Equal(raw, data) {
		t.Fatalf("envelope %+v", envelope)
	}
	if plan, ok := payload.(PlanPayload); !ok || plan.Body != planBody {
		t.Fatalf("payload %#v", payload)
	}
}

func TestReadRefusesPartialAndStaleDocuments(t *testing.T) {
	worktree := t.TempDir()
	if _, _, _, err := Read(worktree, assignment(Plan)); !errors.Is(err, ErrMissing) {
		t.Fatalf("empty worktree: %v", err)
	}
	// Only the temporary file exists: the write is not finished.
	dir := filepath.Join(worktree, ".maestro", "handoff", "turn-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "attempt-1.json.tmp"), planDocument(t, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Read(worktree, assignment(Plan)); !errors.Is(err, ErrMissing) {
		t.Fatalf("temporary file only: %v", err)
	}
	// A document written by an earlier attempt, copied to the current
	// attempt's path, still names the earlier attempt.
	stale := planDocument(t, func(e map[string]any) { e["attempt_id"] = "attempt-0" })
	if err := Write(worktree, "turn-1", "attempt-1", stale); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Read(worktree, assignment(Plan)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale attempt: %v", err)
	}
}

func TestReadRefusesSymlinksAndSpecialFiles(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, planDocument(t, nil), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symbolic link as the document itself.
	worktree := t.TempDir()
	dir := filepath.Join(worktree, ".maestro", "handoff", "turn-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "attempt-1.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Read(worktree, assignment(Plan)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlinked document: %v", err)
	}

	// A symbolic link on the way, even pointing inside the worktree.
	worktree = t.TempDir()
	real := filepath.Join(worktree, "elsewhere")
	if err := Write(real, "turn-1", "attempt-1", planDocument(t, nil)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, ".maestro"), filepath.Join(worktree, ".maestro")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Read(worktree, assignment(Plan)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlinked directory: %v", err)
	}

	// A named pipe instead of a file.
	worktree = t.TempDir()
	dir = filepath.Join(worktree, ".maestro", "handoff", "turn-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "attempt-1.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Read(worktree, assignment(Plan)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("fifo: %v", err)
	}
}

func TestReadAndWriteBoundTheSize(t *testing.T) {
	worktree := t.TempDir()
	huge := bytes.Repeat([]byte(" "), MaxDocument+1)
	if err := Write(worktree, "turn-1", "attempt-1", huge); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized write: %v", err)
	}
	dir := filepath.Join(worktree, ".maestro", "handoff", "turn-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "attempt-1.json"), huge, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Read(worktree, assignment(Plan)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("oversized read: %v", err)
	}
}

func TestPathRefusesUnsafeIdentifiers(t *testing.T) {
	for _, ids := range [][2]string{{"../x", "a"}, {"t", "a/b"}, {"", "a"}} {
		if _, err := Path(ids[0], ids[1]); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%v: %v", ids, err)
		}
	}
}
