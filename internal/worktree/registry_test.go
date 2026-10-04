// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLookupFindsAProjectByID(t *testing.T) {
	store, project := projectFixture(t)
	found, ok, err := store.Lookup(project.ID)
	if err != nil || !ok || found.ID != project.ID {
		t.Fatalf("found=%+v ok=%v err=%v", found, ok, err)
	}
	if _, ok, err := store.Lookup("0000"); err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestStateReportsAMissingRepository(t *testing.T) {
	store, project := projectFixture(t)
	if project.State() != StateOK {
		t.Fatalf("state=%s", project.State())
	}
	moved := strings.TrimSuffix(project.UserRepository, "/.git") + ".moved"
	if err := os.Rename(strings.TrimSuffix(project.UserRepository, "/.git"), moved); err != nil {
		t.Fatal(err)
	}
	refreshed, _, err := store.Lookup(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.State() != StateMissing {
		t.Fatalf("state=%s", refreshed.State())
	}
}

func TestRelocateFollowsAMovedRepository(t *testing.T) {
	store, project := projectFixture(t)
	source := strings.TrimSuffix(project.UserRepository, "/.git")
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(source, moved); err != nil {
		t.Fatal(err)
	}

	relocated, err := store.Relocate(project.ID, moved)
	if err != nil {
		t.Fatal(err)
	}
	if relocated.State() != StateOK {
		t.Fatalf("state=%s", relocated.State())
	}
	// The update is durable and the new path resolves to the project.
	found, ok, err := store.Find(moved)
	if err != nil || !ok || found.ID != project.ID {
		t.Fatalf("found=%+v ok=%v err=%v", found, ok, err)
	}
	// Relocating to the current path is a no-op.
	if _, err := store.Relocate(project.ID, moved); err != nil {
		t.Fatal(err)
	}
}

func TestRelocateRefusesAnUnknownProject(t *testing.T) {
	store, _ := projectFixture(t)
	if _, err := store.Relocate("0000", t.TempDir()); err == nil || !strings.Contains(err.Error(), "unknown project") {
		t.Fatalf("error %v", err)
	}
}

func TestRelocateRefusesAForeignRepository(t *testing.T) {
	store, project := projectFixture(t)
	foreign := userRepository(t)
	// Make the foreign history deterministic and distinct: two fixture
	// repositories born in the same second would share their root SHA.
	run(t, foreign, "git", "commit", "-q", "--amend", "-m", "feat: foreign history")
	_, err := store.Relocate(project.ID, foreign)
	if err == nil || !strings.Contains(err.Error(), "does not contain the project's history") {
		t.Fatalf("error %v", err)
	}
}

func TestRelocateRefusesAPathOfAnotherProject(t *testing.T) {
	store, project := projectFixture(t)
	other := userRepository(t)
	registered, _, err := store.Init(other)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Relocate(project.ID, other)
	if err == nil || !strings.Contains(err.Error(), registered.ID) {
		t.Fatalf("error %v", err)
	}
}

func TestRelocateRefusesANonRepository(t *testing.T) {
	store, project := projectFixture(t)
	if _, err := store.Relocate(project.ID, t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a Git repository") {
		t.Fatalf("error %v", err)
	}
}
