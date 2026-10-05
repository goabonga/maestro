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
	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/worktree"
)

// publishDaemon serves the daemon API with publication on a Unix
// socket, over the data directory of the test, and registers one
// repository. It returns the socket, the operations store, the
// repository and its project.
func publishDaemon(t *testing.T) (string, *integration.Store, string, worktree.Project) {
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
	if _, err := db.Exec("INSERT INTO config_snapshots (config_id, created_at, document) VALUES ('sha256-x', 'now', '{}')"); err != nil {
		t.Fatal(err)
	}
	store := worktree.Store{Base: data}
	repo := gitRepo(t)
	project, _, err := store.Init(repo)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "svc.sock")
	listener, err := transport.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	operations := &integration.Store{DB: db}
	server := &ipc.Server{DB: db, Store: store, Service: "maestro-svc", Version: "0.0.0", Operations: operations}
	web := &http.Server{Handler: server.Handler()}
	go func() { _ = web.Serve(listener) }()
	t.Cleanup(func() { _ = web.Close() })
	return socket, operations, repo, project
}

// gitOut runs git in dir, or against the bare repository dir when
// bare, and returns its trimmed output.
func gitOut(t *testing.T, dir string, bare bool, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test",
	}
	if bare {
		cmd.Env = append(cmd.Env, "GIT_DIR="+dir)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// syncIntegration commits on the canonical integration branch and
// journals the move as a committed, tested SYNC operation.
func syncIntegration(t *testing.T, store *integration.Store, project worktree.Project) string {
	t.Helper()
	repository := project.Repository()
	base := gitOut(t, repository, true, "rev-parse", "refs/heads/maestro/integration")
	tree := gitOut(t, repository, true, "rev-parse", base+"^{tree}")
	head := gitOut(t, repository, true, "commit-tree", tree, "-p", base, "-m", "feat: synced")
	gitOut(t, repository, true, "update-ref", "refs/heads/maestro/integration", head, base)
	op, err := store.Prepare(integration.Operation{
		Type: integration.Sync, ProjectID: project.ID, ConfigID: "sha256-x", WorkerID: "w1", AttemptID: "a1",
		IntegrationBaseSHA: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() (integration.Operation, error){
		func() (integration.Operation, error) { return store.Start(op.ID, "") },
		func() (integration.Operation, error) { return store.RecordCandidate(op.ID, "", tree) },
		func() (integration.Operation, error) { return store.RecordResult(op.ID, head) },
		func() (integration.Operation, error) {
			return store.MarkTested(op.ID, integration.Evidence{TestReportIDs: []string{"report-1"}})
		},
		func() (integration.Operation, error) { return store.Commit(op.ID, integration.Evidence{}) },
	} {
		if _, err := step(); err != nil {
			t.Fatal(err)
		}
	}
	return head
}

// runPublish runs maestro publish and returns its output.
func runPublish(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := Run(context.Background(), append([]string{"publish"}, args...), &output, "0.0.0")
	return output.String(), err
}

func TestPublishFastForwardsTheUserBranch(t *testing.T) {
	socket, operations, repo, project := publishDaemon(t)
	t.Chdir(repo)
	if _, err := runPublish(t, "--socket", socket); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("an untested head was published: %v", err)
	}

	first := syncIntegration(t, operations, project)
	output, err := runPublish(t, "--socket", socket)
	if err != nil || !strings.HasPrefix(output, "published "+first+" to refs/heads/maestro/integration (was created)\noperation: ") {
		t.Fatalf("output %q, error %v", output, err)
	}
	if branch := gitOut(t, repo, false, "rev-parse", "maestro/integration"); branch != first {
		t.Fatalf("branch at %s", branch)
	}
	if head := gitOut(t, repo, false, "symbolic-ref", "--short", "HEAD"); head != "main" {
		t.Fatalf("HEAD moved to %s", head)
	}

	second := syncIntegration(t, operations, project)
	output, err = runPublish(t, "--project", project.ID, "--socket", socket)
	if err != nil || !strings.HasPrefix(output, "published "+second+" to refs/heads/maestro/integration (was "+first+")\n") {
		t.Fatalf("output %q, error %v", output, err)
	}
	output, err = runPublish(t, "--socket", socket)
	if err != nil || output != "refs/heads/maestro/integration already at "+second+"\n" {
		t.Fatalf("output %q, error %v", output, err)
	}
}

func TestPublishReportsADivergenceAndACheckedOutBranch(t *testing.T) {
	socket, operations, repo, project := publishDaemon(t)
	t.Chdir(repo)
	head := syncIntegration(t, operations, project)
	gitOut(t, repo, false, "commit", "-q", "--allow-empty", "-m", "user work")
	local := gitOut(t, repo, false, "rev-parse", "HEAD")
	gitOut(t, repo, false, "branch", "maestro/integration", local)

	_, err := runPublish(t, "--socket", socket)
	if err == nil || !strings.Contains(err.Error(), "conflict") || !strings.Contains(err.Error(), local) || !strings.Contains(err.Error(), head) {
		t.Fatalf("a divergence was published: %v", err)
	}

	gitOut(t, repo, false, "branch", "-f", "maestro/integration", "HEAD~1")
	linked := filepath.Join(t.TempDir(), "linked")
	gitOut(t, repo, false, "worktree", "add", "-q", linked, "maestro/integration")
	if _, err := runPublish(t, "--socket", socket); err == nil || !strings.Contains(err.Error(), "checked out at "+linked) {
		t.Fatalf("a checked-out branch was published: %v", err)
	}
	if branch := gitOut(t, repo, false, "rev-parse", "maestro/integration"); branch == head {
		t.Fatal("the checked-out branch moved")
	}
}

func TestPublishRejectsAnArgument(t *testing.T) {
	if _, err := runPublish(t, "now"); err == nil || !strings.Contains(err.Error(), "unexpected argument: now") {
		t.Fatalf("error %v", err)
	}
}
