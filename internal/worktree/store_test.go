// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func userRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main")
	run(t, dir, "git", "config", "user.name", "Test")
	run(t, dir, "git", "config", "user.email", "test@example.test")
	run(t, dir, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", "README.md")
	run(t, dir, "git", "commit", "-q", "-m", "feat: initial")
	return dir
}

func TestInitImportsWithoutTouchingTheUserRepository(t *testing.T) {
	user := userRepository(t)
	head := run(t, user, "git", "rev-parse", "HEAD")
	before := run(t, user, "git", "for-each-ref")

	store := Store{Base: t.TempDir()}
	project, created, err := store.Init(user)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if run(t, project.Repository(), "git", "rev-parse", "refs/heads/maestro/integration") != head {
		t.Fatal("integration ref does not point at the imported HEAD")
	}
	if run(t, user, "git", "status", "--porcelain") != "" {
		t.Fatal("user worktree modified")
	}
	if run(t, user, "git", "for-each-ref") != before {
		t.Fatal("user references modified")
	}
}

func TestInitIsIdempotentAcrossAliasesAndWorktrees(t *testing.T) {
	user := userRepository(t)
	store := Store{Base: t.TempDir()}
	project, created, err := store.Init(user)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}

	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(user, alias); err != nil {
		t.Fatal(err)
	}
	fromAlias, created, err := store.Init(alias)
	if err != nil || created || fromAlias.ID != project.ID {
		t.Fatalf("alias: created=%v id=%s err=%v", created, fromAlias.ID, err)
	}

	linked := filepath.Join(t.TempDir(), "linked")
	run(t, user, "git", "worktree", "add", "-q", linked)
	fromLinked, created, err := store.Init(linked)
	if err != nil || created || fromLinked.ID != project.ID {
		t.Fatalf("worktree: created=%v id=%s err=%v", created, fromLinked.ID, err)
	}

	projects, err := store.Projects()
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects=%d err=%v", len(projects), err)
	}
}

func TestInitCopiesObjectsWithoutLinksToTheSource(t *testing.T) {
	user := userRepository(t)
	store := Store{Base: t.TempDir()}
	project, _, err := store.Init(user)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project.Repository(), "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatal("the canonical repository uses alternates")
	}
	moved := user + ".moved"
	if err := os.Rename(user, moved); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Rename(moved, user) }()
	run(t, project.Repository(), "git", "fsck", "--strict")
}

func TestInitRefusesRepositoryWithoutCommits(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q")
	if _, _, err := (Store{Base: t.TempDir()}).Init(dir); err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("error %v", err)
	}
}

func TestInitRefusesNonRepository(t *testing.T) {
	if _, _, err := (Store{Base: t.TempDir()}).Init(t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a Git repository") {
		t.Fatalf("error %v", err)
	}
}

func TestDefaultStoreHonorsEnvironmentOverrides(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", "/data/maestro-home")
	t.Setenv("XDG_DATA_HOME", "/data/xdg")
	if s, err := DefaultStore(); err != nil || s.Base != "/data/maestro-home" {
		t.Fatalf("%+v %v", s, err)
	}
	t.Setenv("MAESTRO_DATA_HOME", "")
	if s, err := DefaultStore(); err != nil || s.Base != "/data/xdg/maestro" {
		t.Fatalf("%+v %v", s, err)
	}
}
