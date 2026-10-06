// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package handoff

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGitIgnoresAnInheritedGitDir(t *testing.T) {
	repository, _, commits := history(t)
	other := t.TempDir()
	gitIn(t, other, "init", "-q")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	if head, err := git(repository, "rev-parse", "HEAD"); err != nil || head != commits[1] {
		t.Fatalf("git rev-parse HEAD = %q, %v; want %s", head, err, commits[1])
	}
	if raw, err := rawGit(repository, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(string(raw)) != commits[1] {
		t.Fatalf("raw git rev-parse HEAD = %q, %v; want %s", raw, err, commits[1])
	}
}
