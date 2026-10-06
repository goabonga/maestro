// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package provision

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGitIgnoresAnInheritedGitDir(t *testing.T) {
	dir, _ := pathsRepository(t)
	want := pathsGit(t, dir, "rev-parse", "HEAD")
	other := t.TempDir()
	pathsGit(t, other, "init", "-q")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	if got, err := git(dir, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(got) != want {
		t.Fatalf("git rev-parse HEAD = %q, %v; want %s", got, err, want)
	}
	if got, err := gitIn(dir, "rev-parse", "HEAD"); err != nil || got != want {
		t.Fatalf("gitIn rev-parse HEAD = %q, %v; want %s", got, err, want)
	}
}
