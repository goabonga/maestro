// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/integration"
	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/testrun"
	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/worktree"
)

// exitTester is a fixture test runner reporting every command with
// one exit code on the revision it is given.
type exitTester struct {
	exit int
}

func (f exitTester) Run(_, sha, clone string, commands []testrun.Command) (testrun.Run, error) {
	run := testrun.Run{TestedSHA: sha, Clone: clone}
	for _, command := range commands {
		run.Results = append(run.Results, testrun.Result{Name: command.Name, Argv: command.Argv, TestedSHA: sha, ExitCode: f.exit})
	}
	return run, nil
}

// syncGit runs git in the user repository and returns its trimmed
// output.
func syncGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// syncCommit writes and commits one file in the user repository.
func syncCommit(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	syncGit(t, dir, "add", name)
	syncGit(t, dir, "commit", "-q", "--no-gpg-sign", "-m", "feat: "+name)
	return syncGit(t, dir, "rev-parse", "HEAD")
}

// syncDaemon serves the daemon API with syncs on a Unix socket, over
// the data directory of the test, and registers one repository whose
// configuration names a test command. It returns the socket, the
// repository and the project.
func syncDaemon(t *testing.T, tester integration.SyncTester) (string, string, worktree.Project) {
	t.Helper()
	data := t.TempDir()
	t.Setenv("MAESTRO_DATA_HOME", data)
	db, err := state.Open(filepath.Join(data, "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	store := worktree.Store{Base: data}
	repo := gitRepo(t)
	project, _, err := store.Init(repo)
	if err != nil {
		t.Fatal(err)
	}
	syncCommit(t, repo, ".maestro.toml", "[tests.unit]\nargv = [\"/bin/true\"]\n")
	socket := filepath.Join(t.TempDir(), "svc.sock")
	listener, err := transport.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &ipc.Server{DB: db, Store: store, Service: "maestro-svc", Version: "0.0.0",
		Sync: &integration.Syncer{Store: integration.Store{DB: db}, Tester: tester}}
	web := &http.Server{Handler: server.Handler()}
	go func() { _ = web.Serve(listener) }()
	t.Cleanup(server.Wait)
	t.Cleanup(func() { _ = web.Close() })
	return socket, repo, project
}

// runSync runs one maestro sync command and returns its output.
func runSync(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := Run(context.Background(), append([]string{"sync"}, args...), &output, "0.0.0")
	return output.String(), err
}

func TestSyncCommandAdvancesTheIntegration(t *testing.T) {
	socket, repo, project := syncDaemon(t, exitTester{})
	previous := syncGit(t, project.Repository(), "rev-parse", "refs/heads/maestro/integration")
	synced := syncGit(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "draft.txt"), []byte("draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	output, err := runSync(t, "--from", "main", "--socket", socket)
	if err != nil {
		t.Fatalf("sync: %v: %s", err, output)
	}
	for _, want := range []string{
		"syncing branch main at " + synced + " from integration " + previous,
		"note: untracked files in the user repository are not synced",
		"synced " + synced + ": integration advanced from " + previous,
		"tests: sync-",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output misses %q:\n%s", want, output)
		}
	}
	if head := syncGit(t, project.Repository(), "rev-parse", "refs/heads/maestro/integration"); head != synced {
		t.Fatalf("integration at %s, want %s", head, synced)
	}

	// Nothing left to sync.
	output, err = runSync(t, "--from", "main", "--project", project.ID, "--socket", socket)
	if err == nil || !strings.Contains(err.Error(), "conflict") || !strings.Contains(err.Error(), synced) {
		t.Fatalf("output %q, error %v", output, err)
	}
}

func TestSyncCommandReportsARollBack(t *testing.T) {
	socket, repo, project := syncDaemon(t, exitTester{exit: 2})
	previous := syncGit(t, project.Repository(), "rev-parse", "refs/heads/maestro/integration")
	t.Chdir(repo)
	output, err := runSync(t, "--from", "main", "--socket", socket)
	if err == nil || !strings.Contains(err.Error(), "rolled back") || !strings.Contains(err.Error(), previous) ||
		!strings.Contains(err.Error(), "tests failed") {
		t.Fatalf("output %q, error %v", output, err)
	}
	if head := syncGit(t, project.Repository(), "rev-parse", "refs/heads/maestro/integration"); head != previous {
		t.Fatalf("integration moved to %s", head)
	}
}

func TestSyncCommandRefusesADivergentBranch(t *testing.T) {
	socket, repo, project := syncDaemon(t, exitTester{})
	previous := syncGit(t, project.Repository(), "rev-parse", "refs/heads/maestro/integration")
	syncGit(t, repo, "switch", "-q", "--orphan", "other")
	other := syncCommit(t, repo, "other.txt", "other\n")
	syncGit(t, repo, "switch", "-q", "main")
	output, err := runSync(t, "--from", "other", "--project", project.ID, "--socket", socket)
	if err == nil || !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), previous) {
		t.Fatalf("output %q, error %v", output, err)
	}
}

func TestSyncCommandUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"main"}, {"--from", "main", "extra"}} {
		if _, err := runSync(t, args...); err == nil || !strings.Contains(err.Error(), "usage: maestro sync") {
			t.Fatalf("args %v: error %v", args, err)
		}
	}
	if _, err := runSync(t, "--from", "main", "--project", "p1", "--socket", filepath.Join(t.TempDir(), "none.sock")); err == nil ||
		!strings.Contains(err.Error(), "daemon not reachable") {
		t.Fatalf("error %v", err)
	}
}
