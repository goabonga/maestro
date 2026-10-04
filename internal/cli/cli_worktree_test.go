// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/worktree"
)

// registeredProject initializes a repository and one worker with a task
// worktree, and returns the repository path and the worktree path.
func registeredProject(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("MAESTRO_DATA_HOME", t.TempDir())
	repo := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "--quiet", "--no-gpg-sign", "-m", "feat: readme"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	store, err := worktree.DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := store.Init(repo)
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err := store.AddWorker(project, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := store.AddTaskWorktree(project, worker, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	return repo, path
}

func TestWorktreeListShowsTaskWorktrees(t *testing.T) {
	repo, path := registeredProject(t)
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"worktree", "list", "--path", repo}, &output, "0.0.0"); err != nil {
		t.Fatalf("list: %v: %s", err, output.String())
	}
	listing := output.String()
	for _, want := range []string{"claude-01", "task-1", "maestro/task-task-1", "clean"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("listing misses %q: %s", want, listing)
		}
	}

	if err := os.WriteFile(filepath.Join(path, "untracked.txt"), []byte("data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := Run(context.Background(), []string{"worktree", "list", "--path", repo}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "dirty") {
		t.Fatalf("listing misses the dirty state: %s", output.String())
	}
}

func TestWorktreeListRequiresARegisteredRepository(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", t.TempDir())
	repo := gitRepo(t)
	var output bytes.Buffer
	err := Run(context.Background(), []string{"worktree", "list", "--path", repo}, &output, "0.0.0")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("error %v", err)
	}
}

func TestDiffShowsPendingChangesOfTheSoleTask(t *testing.T) {
	repo, path := registeredProject(t)
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"diff", "claude-01", "--path", repo}, &output, "0.0.0"); err != nil {
		t.Fatalf("diff: %v: %s", err, output.String())
	}
	if output.String() != "" {
		t.Fatalf("expected an empty diff, got %q", output.String())
	}

	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := Run(context.Background(), []string{"diff", "claude-01", "--path", repo}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "+changed") {
		t.Fatalf("diff misses the change: %s", output.String())
	}
}

func TestDiffErrors(t *testing.T) {
	repo, _ := registeredProject(t)
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"diff", "codex-01", "--path", repo}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "unknown worker") {
		t.Fatalf("error %v", err)
	}
	if err := Run(context.Background(), []string{"diff", "--path", repo}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "usage: maestro diff") {
		t.Fatalf("error %v", err)
	}

	store, err := worktree.DefaultStore()
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := store.Find(repo)
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err := project.Worker("claude-01")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddTaskWorktree(project, worker, "task-2"); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), []string{"diff", "claude-01", "--path", repo}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "pick one") {
		t.Fatalf("error %v", err)
	}
}
