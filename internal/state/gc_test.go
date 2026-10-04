// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGCPlansAndRemovesSupersededBackups(t *testing.T) {
	base := dataDirectory(t)
	db, err := Open(filepath.Join(base, "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	first, err := db.Backup()
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.Backup()
	if err != nil {
		t.Fatal(err)
	}

	plan, err := PlanGC(base)
	if err != nil || len(plan.Candidates) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if filepath.Join(base, plan.Candidates[0].Path) != first || plan.Candidates[0].Size == 0 {
		t.Fatalf("candidate %+v", plan.Candidates[0])
	}

	journal, err := ApplyGC(base, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatal("the superseded backup survived")
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatal("the newest backup was removed")
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatal("no deletion journal was written")
	}
	// Re-applying the same reviewed plan is a harmless no-op.
	if _, err := ApplyGC(base, plan); err != nil {
		t.Fatal(err)
	}
}
