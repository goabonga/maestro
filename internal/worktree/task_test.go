// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worktree

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workerFixture imports a project and adds one worker.
func workerFixture(t *testing.T) (Store, Project, Worker) {
	t.Helper()
	store, project := projectFixture(t)
	worker, _, err := store.AddWorker(project, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	return store, project, worker
}

// commitIn writes a file and commits it in a worktree.
func commitIn(t *testing.T, path, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(path, name), []byte(name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, path, "git", "config", "user.name", "Test")
	run(t, path, "git", "config", "user.email", "test@example.test")
	run(t, path, "git", "config", "commit.gpgsign", "false")
	run(t, path, "git", "add", name)
	run(t, path, "git", "commit", "-q", "-m", "feat: "+name)
	return run(t, path, "git", "rev-parse", "HEAD")
}

func TestAddTaskWorktreeChecksTheTaskBranchOut(t *testing.T) {
	store, project, worker := workerFixture(t)
	path, created, err := store.AddTaskWorktree(project, worker, "task-1")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if path != project.TaskWorktree(worker, "task-1") {
		t.Fatalf("unexpected path %s", path)
	}
	integration := run(t, project.Repository(), "git", "rev-parse", "refs/heads/maestro/integration")
	if run(t, path, "git", "rev-parse", "HEAD") != integration {
		t.Fatal("the worktree does not start at the integration head")
	}
	if run(t, path, "git", "rev-parse", "--abbrev-ref", "HEAD") != "maestro/task-task-1" {
		t.Fatal("the worktree is not on the task branch")
	}

	again, created, err := store.AddTaskWorktree(project, worker, "task-1")
	if err != nil || created || again != path {
		t.Fatalf("created=%v path=%s err=%v", created, again, err)
	}
}

func TestAddTaskWorktreeRejectsUnsafeTaskIDs(t *testing.T) {
	store, project, worker := workerFixture(t)
	for _, id := range []string{"", "a/b", "..", "A", "_x"} {
		if _, _, err := store.AddTaskWorktree(project, worker, id); err == nil || !strings.Contains(err.Error(), "invalid task id") {
			t.Fatalf("id %q: error %v", id, err)
		}
	}
}

func TestTaskBranchSurvivesWorktreeRemoval(t *testing.T) {
	store, project, worker := workerFixture(t)
	path, _, err := store.AddTaskWorktree(project, worker, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	head := commitIn(t, path, "change.txt")

	if err := store.RemoveTaskWorktree(project, worker, "task-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the worktree directory still exists")
	}
	if run(t, worker.Dir, "git", "rev-parse", "refs/heads/maestro/task-task-1") != head {
		t.Fatal("the task branch lost its commits")
	}

	// A new checkout of the same task resumes on the kept branch.
	path, created, err := store.AddTaskWorktree(project, worker, "task-1")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if run(t, path, "git", "rev-parse", "HEAD") != head {
		t.Fatal("the new worktree does not resume at the kept head")
	}
}

func TestRemoveTaskWorktreeRefusesDirtyWorktrees(t *testing.T) {
	store, project, worker := workerFixture(t)
	path, _, err := store.AddTaskWorktree(project, worker, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "untracked.txt"), []byte("data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveTaskWorktree(project, worker, "task-1"); !errors.Is(err, ErrDirtyWorktree) {
		t.Fatalf("expected ErrDirtyWorktree, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "untracked.txt")); err != nil {
		t.Fatal("the dirty worktree lost data")
	}
}

func TestRemoveTaskWorktreeRefusesANonWorktree(t *testing.T) {
	store, project, worker := workerFixture(t)
	if err := store.RemoveTaskWorktree(project, worker, "task-1"); err == nil || !strings.Contains(err.Error(), "not a worktree") {
		t.Fatalf("error %v", err)
	}
}

func TestQuarantineKeepsADirtyWorktreeWithItsReference(t *testing.T) {
	store, project, worker := workerFixture(t)
	path, _, err := store.AddTaskWorktree(project, worker, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "draft.txt"), []byte("draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	quarantine, err := store.QuarantineTaskWorktree(project, worker, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if quarantine.Branch != "maestro/task-task-1" || quarantine.OriginalPath != path {
		t.Fatalf("unexpected quarantine %+v", quarantine)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the original worktree path still exists")
	}
	draft, err := os.ReadFile(filepath.Join(quarantine.Worktree(), "draft.txt"))
	if err != nil || string(draft) != "draft\n" {
		t.Fatalf("quarantined data lost: %v", err)
	}
	// The moved worktree stays registered with the worker repository.
	if !strings.Contains(run(t, worker.Dir, "git", "worktree", "list"), quarantine.Worktree()) {
		t.Fatal("the quarantined worktree is no longer registered")
	}

	quarantines, err := project.Quarantines()
	if err != nil || len(quarantines) != 1 || quarantines[0].TaskID != "task-1" {
		t.Fatalf("quarantines=%+v err=%v", quarantines, err)
	}
}
