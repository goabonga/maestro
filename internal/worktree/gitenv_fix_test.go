// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worktree

import (
	"path/filepath"
	"testing"
)

func TestGitIgnoresAnInheritedGitDir(t *testing.T) {
	user := userRepository(t)
	want, err := filepath.EvalSymlinks(filepath.Join(user, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	run(t, other, "git", "init", "-q")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	if got, err := canonicalGitDir(user); err != nil || got != want {
		t.Fatalf("canonicalGitDir = %q, %v; want %q", got, err, want)
	}
}
