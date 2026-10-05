// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package provision

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var runtimeFixture = RuntimePaths{"CLAUDE.md", ".claude/", "AGENTS.md", ".mcp.json", ".maestro/"}

func pathsGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func pathsWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	file := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pathsCommit(t *testing.T, dir, message string) string {
	t.Helper()
	pathsGit(t, dir, "commit", "-q", "--allow-empty", "-m", message)
	return pathsGit(t, dir, "rev-parse", "HEAD")
}

// pathsRepository creates a repository ignoring the runtime paths, with a
// base commit tracking README.md and a CLAUDE.md that predates the task.
func pathsRepository(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	pathsGit(t, dir, "init", "-q", "-b", "main")
	pathsGit(t, dir, "config", "user.name", "Test")
	pathsGit(t, dir, "config", "user.email", "test@example.test")
	pathsGit(t, dir, "config", "commit.gpgsign", "false")
	pathsWrite(t, dir, "README.md", "project\n")
	pathsWrite(t, dir, "CLAUDE.md", "tracked by the project\n")
	pathsGit(t, dir, "add", "README.md", "CLAUDE.md")
	base := pathsCommit(t, dir, "initial")
	pathsWrite(t, dir, ".git/info/exclude", "CLAUDE.md\n.claude/\nAGENTS.md\n.mcp.json\n.maestro/\n")
	return dir, base
}

func TestValidateRefusesUncleanRuntimePaths(t *testing.T) {
	for _, entry := range []string{"", "/", ".", "./", "/etc/passwd", "../outside", "a/../b", "a//b", "a\\b", "..", "dir/./x"} {
		if err := (RuntimePaths{entry}).Validate(); !errors.Is(err, ErrInvalidRuntimePath) {
			t.Fatalf("entry %q: error %v", entry, err)
		}
	}
	if err := runtimeFixture.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCoversMatchesFilesAndDirectoriesExactly(t *testing.T) {
	for _, name := range []string{"CLAUDE.md", ".claude/settings.json", ".claude/a/b", ".maestro/x", "CLAUDE.md/inner"} {
		if !runtimeFixture.Covers(name) {
			t.Fatalf("%q not covered", name)
		}
	}
	for _, name := range []string{"CLAUDE.md.bak", "docs/CLAUDE.md", ".claudex", ".claude-settings", "claude.md", "README.md", "*.md"} {
		if runtimeFixture.Covers(name) {
			t.Fatalf("%q covered", name)
		}
	}
	if (RuntimePaths{"*.md"}).Covers("README.md") {
		t.Fatal("entry interpreted as a pattern")
	}
}

func TestCheckIndexAcceptsUnchangedTrackedAndUnrelatedPaths(t *testing.T) {
	dir, base := pathsRepository(t)
	pathsWrite(t, dir, "main.go", "package main\n")
	pathsWrite(t, dir, ".claude/settings.json", "{}\n")
	pathsGit(t, dir, "add", "main.go")
	if err := CheckIndex(dir, base, runtimeFixture); err != nil {
		t.Fatal(err)
	}
}

func TestCheckIndexRefusesForcedAdditions(t *testing.T) {
	dir, base := pathsRepository(t)
	pathsWrite(t, dir, ".claude/settings.json", "{}\n")
	pathsWrite(t, dir, "AGENTS.md", "agents\n")
	pathsGit(t, dir, "add", "-f", ".claude/settings.json", "AGENTS.md")
	err := CheckIndex(dir, base, runtimeFixture)
	if !errors.Is(err, ErrRuntimePath) {
		t.Fatalf("error %v", err)
	}
	for _, want := range []string{"index adds .claude/settings.json", "index adds AGENTS.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

func TestCheckIndexRefusesModifiedTrackedRuntimePath(t *testing.T) {
	dir, base := pathsRepository(t)
	pathsWrite(t, dir, "CLAUDE.md", "rewritten by the agent\n")
	if err := CheckIndex(dir, base, runtimeFixture); err != nil {
		t.Fatalf("unstaged change refused: %v", err)
	}
	pathsGit(t, dir, "add", "-f", "CLAUDE.md")
	err := CheckIndex(dir, base, runtimeFixture)
	if !errors.Is(err, ErrRuntimePath) || !strings.Contains(err.Error(), "index modifies CLAUDE.md") {
		t.Fatalf("error %v", err)
	}
}

func TestCheckIndexComparesWithTheBaseNotHead(t *testing.T) {
	dir, base := pathsRepository(t)
	pathsWrite(t, dir, ".mcp.json", "{}\n")
	pathsGit(t, dir, "add", "-f", ".mcp.json")
	pathsCommit(t, dir, "sneak a runtime path")
	err := CheckIndex(dir, base, runtimeFixture)
	if !errors.Is(err, ErrRuntimePath) || !strings.Contains(err.Error(), ".mcp.json") {
		t.Fatalf("error %v", err)
	}
}

func TestCheckIndexAcceptsRemovalOfTrackedRuntimePath(t *testing.T) {
	dir, base := pathsRepository(t)
	pathsGit(t, dir, "rm", "-q", "--cached", "CLAUDE.md")
	if err := CheckIndex(dir, base, runtimeFixture); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCommitsAcceptsCleanHistory(t *testing.T) {
	dir, base := pathsRepository(t)
	pathsWrite(t, dir, "main.go", "package main\n")
	pathsGit(t, dir, "add", "main.go")
	head := pathsCommit(t, dir, "add main")
	if err := CheckCommits(dir, base, head, runtimeFixture); err != nil {
		t.Fatal(err)
	}
	if err := CheckCommits(dir, base, base, runtimeFixture); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCommitsRefusesPathAddedThenRemoved(t *testing.T) {
	dir, base := pathsRepository(t)
	pathsWrite(t, dir, ".maestro/state.json", "{}\n")
	pathsGit(t, dir, "add", "-f", ".maestro/state.json")
	added := pathsCommit(t, dir, "add runtime state")
	pathsGit(t, dir, "rm", "-q", "--cached", ".maestro/state.json")
	head := pathsCommit(t, dir, "remove runtime state")
	if out := pathsGit(t, dir, "diff", "--name-only", base, head); out != "" {
		t.Fatalf("final diff %q", out)
	}
	err := CheckCommits(dir, base, head, runtimeFixture)
	if !errors.Is(err, ErrRuntimePath) {
		t.Fatalf("error %v", err)
	}
	if want := "runtime path change refused: commit " + added + " adds .maestro/state.json"; err.Error() != want {
		t.Fatalf("error %q, want %q", err, want)
	}
}

func TestCheckCommitsRefusesModificationOfTrackedRuntimePath(t *testing.T) {
	dir, base := pathsRepository(t)
	pathsWrite(t, dir, "CLAUDE.md", "rewritten\n")
	pathsGit(t, dir, "add", "CLAUDE.md")
	head := pathsCommit(t, dir, "rewrite instructions")
	err := CheckCommits(dir, base, head, runtimeFixture)
	if !errors.Is(err, ErrRuntimePath) || !strings.Contains(err.Error(), "commit "+head+" modifies CLAUDE.md") {
		t.Fatalf("error %v", err)
	}
}

func TestCheckCommitsAcceptsRemovalOfTrackedRuntimePath(t *testing.T) {
	dir, base := pathsRepository(t)
	pathsGit(t, dir, "rm", "-q", "--cached", "CLAUDE.md")
	head := pathsCommit(t, dir, "untrack instructions")
	if err := CheckCommits(dir, base, head, runtimeFixture); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCommitsChecksMergedSideBranches(t *testing.T) {
	dir, base := pathsRepository(t)
	pathsGit(t, dir, "checkout", "-q", "-b", "side")
	pathsWrite(t, dir, ".claude/agents/x.md", "x\n")
	pathsGit(t, dir, "add", "-f", ".claude/agents/x.md")
	side := pathsCommit(t, dir, "side runtime path")
	pathsGit(t, dir, "checkout", "-q", "main")
	pathsWrite(t, dir, "main.go", "package main\n")
	pathsGit(t, dir, "add", "main.go")
	pathsCommit(t, dir, "main work")
	pathsGit(t, dir, "merge", "-q", "--no-edit", "side")
	head := pathsGit(t, dir, "rev-parse", "HEAD")
	err := CheckCommits(dir, base, head, runtimeFixture)
	if !errors.Is(err, ErrRuntimePath) {
		t.Fatalf("error %v", err)
	}
	for _, want := range []string{"commit " + side + " adds .claude/agents/x.md", "commit " + head + " adds .claude/agents/x.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

func TestCheckCommitsHandlesRootCommits(t *testing.T) {
	dir, _ := pathsRepository(t)
	pathsGit(t, dir, "checkout", "-q", "--orphan", "fresh")
	pathsGit(t, dir, "rm", "-q", "-r", "--cached", ".")
	pathsWrite(t, dir, "AGENTS.md", "agents\n")
	pathsGit(t, dir, "add", "-f", "AGENTS.md")
	root := pathsCommit(t, dir, "orphan root")
	err := CheckCommits(dir, "main", root, runtimeFixture)
	if !errors.Is(err, ErrRuntimePath) || !strings.Contains(err.Error(), "commit "+root+" adds AGENTS.md") {
		t.Fatalf("error %v", err)
	}
}

func TestChecksRefuseInvalidRevisionsAndPaths(t *testing.T) {
	dir, base := pathsRepository(t)
	for _, rev := range []string{"", "--output=/tmp/x", "-p", "missing", "HEAD\nHEAD"} {
		if err := CheckIndex(dir, rev, runtimeFixture); !errors.Is(err, ErrInvalidRevision) {
			t.Fatalf("index rev %q: error %v", rev, err)
		}
		if err := CheckCommits(dir, base, rev, runtimeFixture); !errors.Is(err, ErrInvalidRevision) {
			t.Fatalf("commits rev %q: error %v", rev, err)
		}
	}
	tree := pathsGit(t, dir, "rev-parse", "HEAD^{tree}")
	if err := CheckIndex(dir, tree, runtimeFixture); !errors.Is(err, ErrInvalidRevision) {
		t.Fatalf("tree accepted as base: %v", err)
	}
	if err := CheckIndex(dir, base, RuntimePaths{"../x"}); !errors.Is(err, ErrInvalidRuntimePath) {
		t.Fatalf("error %v", err)
	}
	if err := CheckCommits(dir, base, base, RuntimePaths{"/abs"}); !errors.Is(err, ErrInvalidRuntimePath) {
		t.Fatalf("error %v", err)
	}
}
