// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskWorktreeStatusesReadBranchesHeadsAndDirtiness(t *testing.T) {
	store, project, worker := workerFixture(t)
	clean, _, err := store.AddTaskWorktree(project, worker, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	dirty, _, err := store.AddTaskWorktree(project, worker, "task-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirty, "untracked.txt"), []byte("data\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	statuses, err := project.TaskWorktreeStatuses()
	if err != nil || len(statuses) != 2 {
		t.Fatalf("statuses=%+v err=%v", statuses, err)
	}
	head := run(t, clean, "git", "rev-parse", "HEAD")
	first, second := statuses[0], statuses[1]
	if first.TaskID != "task-1" || first.Branch != "maestro/task-task-1" || first.Head != head || first.Dirty || first.Path != clean {
		t.Fatalf("unexpected status %+v", first)
	}
	if second.TaskID != "task-2" || !second.Dirty {
		t.Fatalf("unexpected status %+v", second)
	}
}

func TestTaskWorktreeStatusesSkipManuallyDeletedWorktrees(t *testing.T) {
	store, project, worker := workerFixture(t)
	path, _, err := store.AddTaskWorktree(project, worker, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	statuses, err := project.TaskWorktreeStatuses()
	if err != nil || len(statuses) != 0 {
		t.Fatalf("statuses=%+v err=%v", statuses, err)
	}
}

func TestWorkerLooksAnExistingWorkerUp(t *testing.T) {
	_, project, worker := workerFixture(t)
	found, ok, err := project.Worker("claude-01")
	if err != nil || !ok || found.Dir != worker.Dir {
		t.Fatalf("found=%+v ok=%v err=%v", found, ok, err)
	}
	if _, ok, err := project.Worker("codex-01"); err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if _, _, err := project.Worker("../escape"); err == nil || !strings.Contains(err.Error(), "invalid worker name") {
		t.Fatalf("error %v", err)
	}
}

func TestDiffShowsPendingChanges(t *testing.T) {
	store, project, worker := workerFixture(t)
	path, _, err := store.AddTaskWorktree(project, worker, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if diff, err := project.Diff(worker, "task-1"); err != nil || diff != "" {
		t.Fatalf("diff=%q err=%v", diff, err)
	}
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diff, err := project.Diff(worker, "task-1")
	if err != nil || !strings.Contains(diff, "+changed") {
		t.Fatalf("diff=%q err=%v", diff, err)
	}
}
