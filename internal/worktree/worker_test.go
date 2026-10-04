// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// projectFixture imports a fresh user repository and returns its project.
func projectFixture(t *testing.T) (Store, Project) {
	t.Helper()
	store := Store{Base: t.TempDir()}
	project, _, err := store.Init(userRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	return store, project
}

func TestAddWorkerCreatesAnIsolatedClone(t *testing.T) {
	store, project := projectFixture(t)
	worker, created, err := store.AddWorker(project, "claude-01")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if _, err := os.Stat(filepath.Join(worker.Dir, "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatal("the worker repository uses alternates")
	}
	if remotes := run(t, worker.Dir, "git", "remote"); remotes != "" {
		t.Fatalf("the worker repository keeps a remote: %q", remotes)
	}

	again, created, err := store.AddWorker(project, "claude-01")
	if err != nil || created || again.Dir != worker.Dir {
		t.Fatalf("created=%v dir=%s err=%v", created, again.Dir, err)
	}

	workers, err := project.Workers()
	if err != nil || len(workers) != 1 || workers[0].Name != "claude-01" {
		t.Fatalf("workers=%+v err=%v", workers, err)
	}
}

func TestAddWorkerRejectsUnsafeNames(t *testing.T) {
	store, project := projectFixture(t)
	for _, name := range []string{"", "a/b", "..", "A", "claude_01", "-x", ".git"} {
		if _, _, err := store.AddWorker(project, name); err == nil || !strings.Contains(err.Error(), "invalid worker name") {
			t.Fatalf("name %q: error %v", name, err)
		}
	}
}

func TestProvisionCopiesTheIntegrationHead(t *testing.T) {
	store, project := projectFixture(t)
	worker, _, err := store.AddWorker(project, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	head := run(t, project.Repository(), "git", "rev-parse", "refs/heads/maestro/integration")
	sha, err := store.Provision(project, worker, "maestro/task-1")
	if err != nil || sha != head {
		t.Fatalf("sha=%s head=%s err=%v", sha, head, err)
	}
	if run(t, worker.Dir, "git", "rev-parse", "refs/heads/maestro/task-1") != head {
		t.Fatal("the task branch does not point at the integration head")
	}

	// The worker repository must stay complete without the canonical one.
	moved := project.Repository() + ".moved"
	if err := os.Rename(project.Repository(), moved); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Rename(moved, project.Repository()) }()
	run(t, worker.Dir, "git", "fsck", "--strict")
}

func TestImportCopiesWorkerCommitsWithoutMovingIntegration(t *testing.T) {
	store, project := projectFixture(t)
	worker, _, err := store.AddWorker(project, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Provision(project, worker, "maestro/task-1"); err != nil {
		t.Fatal(err)
	}

	// Commit on the task branch through a temporary worktree of the
	// worker repository, as a worker session would.
	work := filepath.Join(t.TempDir(), "work")
	run(t, worker.Dir, "git", "worktree", "add", "-q", work, "maestro/task-1")
	if err := os.WriteFile(filepath.Join(work, "change.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, work, "git", "config", "user.name", "Test")
	run(t, work, "git", "config", "user.email", "test@example.test")
	run(t, work, "git", "config", "commit.gpgsign", "false")
	run(t, work, "git", "add", "change.txt")
	run(t, work, "git", "commit", "-q", "-m", "feat: change")
	head := run(t, work, "git", "rev-parse", "HEAD")

	integration := run(t, project.Repository(), "git", "rev-parse", "refs/heads/maestro/integration")
	sha, err := store.Import(project, worker, "maestro/task-1")
	if err != nil || sha != head {
		t.Fatalf("sha=%s head=%s err=%v", sha, head, err)
	}
	imported := run(t, project.Repository(), "git", "rev-parse", "refs/maestro/workers/claude-01/maestro/task-1")
	if imported != head {
		t.Fatal("the imported ref does not point at the worker head")
	}
	if run(t, project.Repository(), "git", "rev-parse", "refs/heads/maestro/integration") != integration {
		t.Fatal("import moved the integration branch")
	}

	// The canonical repository must stay complete without the worker one.
	run(t, worker.Dir, "git", "worktree", "remove", "--force", work)
	if err := os.RemoveAll(worker.Dir); err != nil {
		t.Fatal(err)
	}
	run(t, project.Repository(), "git", "fsck", "--strict")
}

func TestWorkersAreIsolatedFromEachOther(t *testing.T) {
	store, project := projectFixture(t)
	first, _, err := store.AddWorker(project, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.AddWorker(project, "codex-01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Provision(project, first, "maestro/task-1"); err != nil {
		t.Fatal(err)
	}
	if out := run(t, second.Dir, "git", "for-each-ref", "refs/heads/maestro/task-1"); out != "" {
		t.Fatalf("task branch leaked into another worker: %s", out)
	}
	if out := run(t, project.Repository(), "git", "for-each-ref", "refs/heads/maestro/task-1"); out != "" {
		t.Fatalf("task branch leaked into the canonical repository: %s", out)
	}
}
