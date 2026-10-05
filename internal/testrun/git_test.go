// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package testrun

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/config"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// repository creates a repository with two commits on main and returns
// it with the first and the second commit.
func repository(t *testing.T) (string, string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.test")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, dir, "README.md", "first\n")
	writeFile(t, dir, ".gitignore", "bin/\n")
	runGit(t, dir, "add", "README.md", ".gitignore")
	runGit(t, dir, "commit", "-q", "-m", "first")
	first := runGit(t, dir, "rev-parse", "HEAD")
	writeFile(t, dir, "README.md", "second\n")
	runGit(t, dir, "commit", "-q", "-am", "second")
	return dir, first, runGit(t, dir, "rev-parse", "HEAD")
}

func TestCommandsComeFromConfigurationSortedByName(t *testing.T) {
	cfg := config.Defaults()
	cfg.Tests = map[string]config.TestCommand{
		"unit": {Argv: []string{"go", "test", "./..."}, Timeout: &config.Duration{Duration: time.Minute}},
		"lint": {Argv: []string{"make", "lint"}},
	}
	commands := Commands(cfg)
	if len(commands) != 2 || commands[0].Name != "lint" || commands[1].Name != "unit" {
		t.Fatalf("commands %+v", commands)
	}
	if commands[0].Timeout != 0 || commands[1].Timeout != time.Minute || strings.Join(commands[1].Argv, " ") != "go test ./..." {
		t.Fatalf("commands %+v", commands)
	}
	// The command owns its argv: changing it leaves the configuration intact.
	commands[1].Argv[0] = "rm"
	if cfg.Tests["unit"].Argv[0] != "go" {
		t.Fatal("the command shares its argv with the configuration")
	}
}

func TestEnvironmentHoldsNoDaemonVariable(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-FAKE0123456789abcdefFAKE")
	for key := range Environment() {
		if !map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "TERM": true}[key] {
			t.Fatalf("unexpected variable %s", key)
		}
	}
}

func TestCheckoutClonesTheExactRevision(t *testing.T) {
	repo, first, second := repository(t)
	// An older commit is checked out detached, not the branch tip.
	clone := filepath.Join(t.TempDir(), "clone")
	if err := checkout(repo, first, clone); err != nil {
		t.Fatal(err)
	}
	if head := runGit(t, clone, "rev-parse", "HEAD"); head != first {
		t.Fatalf("HEAD %s, want %s", head, first)
	}
	if branch := runGit(t, clone, "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
		t.Fatalf("the clone is on %s, not detached", branch)
	}
	if content, _ := os.ReadFile(filepath.Join(clone, "README.md")); string(content) != "first\n" {
		t.Fatalf("README %q", content)
	}
	// The clone is private: its objects are copies, not links into the
	// repository.
	if alternates, err := os.ReadFile(filepath.Join(clone, ".git", "objects", "info", "alternates")); err == nil {
		t.Fatalf("the clone borrows objects: %s", alternates)
	}

	// A commit reachable only from an operation reference is fetched.
	runGit(t, repo, "update-ref", "refs/maestro/op/candidate", second)
	runGit(t, repo, "reset", "-q", "--hard", first)
	unlisted := filepath.Join(t.TempDir(), "clone")
	if err := checkout(repo, second, unlisted); err != nil {
		t.Fatal(err)
	}
	if head := runGit(t, unlisted, "rev-parse", "HEAD"); head != second {
		t.Fatalf("HEAD %s, want %s", head, second)
	}
}

func TestCheckoutRefusesInvalidRevisions(t *testing.T) {
	repo, first, _ := repository(t)
	existing := t.TempDir()
	for name, c := range map[string]struct{ sha, clone string }{
		"abbreviated": {first[:12], filepath.Join(t.TempDir(), "clone")},
		"option":      {"--upload-pack=touch", filepath.Join(t.TempDir(), "clone")},
		"unknown":     {strings.Repeat("d", 40), filepath.Join(t.TempDir(), "clone")},
		"existing":    {first, existing},
	} {
		if err := checkout(repo, c.sha, c.clone); !errors.Is(err, ErrCheckout) {
			t.Fatalf("%s: expected ErrCheckout, got %v", name, err)
		}
	}
}

func TestInspectSeparatesSourceChangesFromBuildOutputs(t *testing.T) {
	repo, _, second := repository(t)
	clone := filepath.Join(t.TempDir(), "clone")
	if err := checkout(repo, second, clone); err != nil {
		t.Fatal(err)
	}
	baseline, err := indexEntries(clone)
	if err != nil {
		t.Fatal(err)
	}
	changed, untracked, err := inspect(clone, second, baseline)
	if err != nil || len(changed) != 0 || len(untracked) != 0 {
		t.Fatalf("a fresh clone: %v %v %v", changed, untracked, err)
	}

	writeFile(t, clone, "bin/app", "binary")
	writeFile(t, clone, "coverage.out", "report")
	changed, untracked, err = inspect(clone, second, baseline)
	if err != nil || len(changed) != 0 || strings.Join(untracked, ",") != "bin/,coverage.out" {
		t.Fatalf("build outputs: %v %v %v", changed, untracked, err)
	}

	writeFile(t, clone, "README.md", "rewritten\n")
	writeFile(t, clone, "new.go", "package x\n")
	runGit(t, clone, "add", "new.go")
	changed, _, err = inspect(clone, second, baseline)
	if err != nil || strings.Join(changed, ",") != "README.md,new.go" {
		t.Fatalf("source changes: %v %v", changed, err)
	}

	runGit(t, clone, "checkout", "-q", "--force", "--detach", "HEAD~1")
	changed, _, err = inspect(clone, second, baseline)
	if err != nil || len(changed) == 0 || changed[0] != "HEAD" {
		t.Fatalf("a moved HEAD: %v %v", changed, err)
	}
}

func TestTailKeepsTheEndOfTheOutput(t *testing.T) {
	output := &tail{max: 8}
	for _, chunk := range []string{"0123", "4567", "89ab"} {
		if _, err := output.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if text, truncated := output.String(); text != "456789ab" || !truncated {
		t.Fatalf("tail %q %v", text, truncated)
	}
	short := &tail{max: 8}
	_, _ = short.Write([]byte("ok"))
	if text, truncated := short.String(); text != "ok" || truncated {
		t.Fatalf("tail %q %v", text, truncated)
	}
	// A cut through a multi-byte character still yields valid text.
	cut := &tail{max: 4}
	_, _ = cut.Write([]byte("é…"))
	if text, _ := cut.String(); text != "\uFFFD…" {
		t.Fatalf("tail %q", text)
	}
}
