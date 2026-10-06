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

func TestCheckoutWorkerFollowsTheIntegrationHead(t *testing.T) {
	store, project := projectFixture(t)
	worker, _, err := store.AddWorker(project, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	path, head, err := store.CheckoutWorker(project, worker)
	if err != nil {
		t.Fatal(err)
	}
	if path != project.WorkerWorktree("claude-01") || head != run(t, project.Repository(), "git", "rev-parse", "refs/heads/maestro/integration") {
		t.Fatalf("path=%s head=%s", path, head)
	}
	if run(t, path, "git", "rev-parse", "HEAD") != head {
		t.Fatal("the worktree is not at the integration head")
	}
	if branch := run(t, path, "git", "branch", "--show-current"); branch != "" {
		t.Fatalf("the worktree is on branch %s, not detached", branch)
	}

	// The integration advances: a new checkout moves the worktree.
	scratch := filepath.Join(t.TempDir(), "scratch")
	run(t, project.Repository(), "git", "worktree", "add", "-q", "--detach", scratch, "refs/heads/maestro/integration")
	run(t, scratch, "git", "config", "user.name", "Test")
	run(t, scratch, "git", "config", "user.email", "test@example.test")
	run(t, scratch, "git", "config", "commit.gpgsign", "false")
	run(t, scratch, "git", "commit", "-q", "--allow-empty", "-m", "feat: next")
	next := run(t, scratch, "git", "rev-parse", "HEAD")
	run(t, project.Repository(), "git", "update-ref", "refs/heads/maestro/integration", next)
	again, moved, err := store.CheckoutWorker(project, worker)
	if err != nil || again != path || moved != next || run(t, path, "git", "rev-parse", "HEAD") != next {
		t.Fatalf("path=%s head=%s err=%v", again, moved, err)
	}

	// A new checkout discards the uncommitted work of a previous run.
	run(t, path, "git", "config", "user.name", "Test")
	run(t, path, "git", "config", "user.email", "test@example.test")
	run(t, path, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, path, "git", "add", "tracked.txt")
	run(t, path, "git", "commit", "-q", "-m", "feat: tracked")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "drafts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "drafts", "draft.txt"), []byte("draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reset, at, err := store.CheckoutWorker(project, worker)
	if err != nil || reset != path || at != next || run(t, path, "git", "rev-parse", "HEAD") != next {
		t.Fatalf("path=%s head=%s err=%v", reset, at, err)
	}
	if status := run(t, path, "git", "status", "--porcelain", "--ignored"); status != "" {
		t.Fatalf("the worktree kept the previous work: %s", status)
	}
	if _, err := os.Stat(filepath.Join(path, "tracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("a file of the previous run survived: %v", err)
	}
}

// victimRepository creates a repository a git-spawned caller would name
// in GIT_DIR, and returns it with a fingerprint of its configuration,
// references, index and worktree.
func victimRepository(t *testing.T) (string, func() string) {
	t.Helper()
	victim := t.TempDir()
	run(t, victim, "git", "init", "-q", "-b", "trunk")
	run(t, victim, "git", "config", "user.name", "Victim")
	run(t, victim, "git", "config", "user.email", "victim@example.test")
	run(t, victim, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(victim, "victim.txt"), []byte("victim\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, victim, "git", "add", "victim.txt")
	run(t, victim, "git", "commit", "-q", "-m", "feat: victim")
	fingerprint := func() string {
		t.Helper()
		var parts []string
		for _, name := range []string{"config", "HEAD", "index"} {
			data, err := os.ReadFile(filepath.Join(victim, ".git", name)) // #nosec G304 -- the test's own temporary repository
			if err != nil {
				t.Fatal(err)
			}
			parts = append(parts, string(data))
		}
		parts = append(parts,
			run(t, victim, "git", "--git-dir", filepath.Join(victim, ".git"), "--work-tree", victim, "for-each-ref"),
			run(t, victim, "git", "--git-dir", filepath.Join(victim, ".git"), "--work-tree", victim, "status", "--porcelain", "--ignored"))
		return strings.Join(parts, "\n--\n")
	}
	return victim, fingerprint
}

// inheritGitDir sets the variables a git-spawned caller exports, naming
// the victim repository, for the rest of the test.
func inheritGitDir(t *testing.T, victim string) {
	t.Setenv("GIT_DIR", filepath.Join(victim, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(victim, ".git", "index"))
}

func TestCheckoutWorkerIgnoresAnInheritedGitDir(t *testing.T) {
	store, project := projectFixture(t)
	worker, _, err := store.AddWorker(project, "claude-01")
	if err != nil {
		t.Fatal(err)
	}
	path, head, err := store.CheckoutWorker(project, worker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "draft.txt"), []byte("draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	victim, fingerprint := victimRepository(t)
	if err := os.WriteFile(filepath.Join(victim, "untracked.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := fingerprint()

	// GIT_DIR without GIT_WORK_TREE, as a hook exports it: the forced
	// checkout and the clean must still only touch the worker's worktree.
	inheritGitDir(t, victim)
	again, at, err := store.CheckoutWorker(project, worker)
	if err != nil || again != path || at != head {
		t.Fatalf("path=%s head=%s err=%v", again, at, err)
	}
	if after := fingerprint(); after != before {
		t.Fatalf("the victim repository changed:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(path, "draft.txt")); !os.IsNotExist(err) {
		t.Fatalf("the worker worktree was not cleaned: %v", err)
	}
	if run(t, path, "git", "--git-dir", filepath.Join(path, ".git"), "rev-parse", "HEAD") != head {
		t.Fatal("the worker worktree is not at the integration head")
	}
}
