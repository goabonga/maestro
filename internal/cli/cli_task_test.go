// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/worktree"
)

// taskDaemon serves the daemon API with tasks on a Unix socket, over the
// data directory of the test, and registers one repository. It returns
// the socket, the repository and its project id.
func taskDaemon(t *testing.T) (*ipc.Server, string, string, string) {
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
	socket := filepath.Join(t.TempDir(), "svc.sock")
	listener, err := transport.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &ipc.Server{DB: db, Store: store, Service: "maestro-svc", Version: "0.0.0", Tasks: &task.Store{DB: db}}
	web := &http.Server{Handler: server.Handler()}
	go func() { _ = web.Serve(listener) }()
	t.Cleanup(func() { _ = web.Close() })
	return server, socket, repo, project.ID
}

// runTask runs one maestro task command and returns its output.
func runTask(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := Run(context.Background(), append([]string{"task"}, args...), &output, "0.0.0")
	return output.String(), err
}

// createdID reads the task id printed by maestro task new.
func createdID(t *testing.T, output string) string {
	t.Helper()
	line, _, _ := strings.Cut(output, "\n")
	id, ok := strings.CutPrefix(line, "task created: ")
	if !ok {
		t.Fatalf("unexpected output: %q", output)
	}
	return id
}

func TestTaskCommandsDiscoverTheCurrentRepository(t *testing.T) {
	_, socket, repo, projectID := taskDaemon(t)
	t.Chdir(repo)

	output, err := runTask(t, "list", "--socket", socket)
	if err != nil || output != "no tasks\n" {
		t.Fatalf("output %q, error %v", output, err)
	}

	output, err = runTask(t, "new", "add a --verbose flag\nwith a test", "--socket", socket)
	if err != nil {
		t.Fatalf("new: %v: %s", err, output)
	}
	id := createdID(t, output)
	if !strings.Contains(output, "state: NEW") || !strings.Contains(output, "branch: maestro/task-"+id) {
		t.Fatalf("unexpected output: %q", output)
	}

	output, err = runTask(t, "list", "--socket", socket)
	if err != nil || !strings.Contains(output, id) || !strings.Contains(output, "NEW") ||
		!strings.Contains(output, "add a --verbose flag") || strings.Contains(output, "with a test") {
		t.Fatalf("output %q, error %v", output, err)
	}

	output, err = runTask(t, "show", id, "--socket", socket)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{"task:", id, "project:", projectID, "state:", "NEW", "add a --verbose flag\nwith a test", "create", "fix cycles:", "0/3"} {
		if !strings.Contains(output, want) {
			t.Fatalf("show misses %q: %s", want, output)
		}
	}

	output, err = runTask(t, "cancel", id, "--socket", socket)
	if err != nil || output != "task "+id+": CANCELLED\n" {
		t.Fatalf("output %q, error %v", output, err)
	}
	if _, err := runTask(t, "cancel", id, "--socket", socket); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("second cancel: %v", err)
	}
}

func TestTaskResumeFollowsTheStoredContinuation(t *testing.T) {
	server, socket, _, projectID := taskDaemon(t)
	t.Chdir(t.TempDir())

	output, err := runTask(t, "new", "--project", projectID, "--socket", socket, "fix the parser")
	if err != nil {
		t.Fatalf("new: %v: %s", err, output)
	}
	id := createdID(t, output)
	if _, err := runTask(t, "resume", id, "--project", projectID, "--socket", socket); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("resume of a task that is not blocked: %v", err)
	}
	if _, err := server.Tasks.Transition(id, task.Input{Event: task.Block, Reason: "turn timeout"}); err != nil {
		t.Fatal(err)
	}
	output, err = runTask(t, "show", id, "--project", projectID, "--socket", socket)
	if err != nil || !strings.Contains(output, "BLOCKED") || !strings.Contains(output, "turn timeout") {
		t.Fatalf("output %q, error %v", output, err)
	}
	output, err = runTask(t, "resume", id, "--project", projectID, "--socket", socket)
	if err != nil || output != "task "+id+": NEW\n" {
		t.Fatalf("output %q, error %v", output, err)
	}
}

func TestTaskCommandsNeedAProject(t *testing.T) {
	_, socket, _, _ := taskDaemon(t)
	t.Chdir(t.TempDir())
	if _, err := runTask(t, "list", "--socket", socket); err == nil || !strings.Contains(err.Error(), "not inside a Git repository") {
		t.Fatalf("outside a repository: %v", err)
	}
	t.Chdir(gitRepo(t))
	if _, err := runTask(t, "list", "--socket", socket); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unregistered repository: %v", err)
	}
	if _, err := runTask(t, "list", "--project", "missing", "--socket", socket); err == nil || !strings.Contains(err.Error(), "unknown project: missing") {
		t.Fatalf("unknown project: %v", err)
	}
}

func TestTaskCommandErrors(t *testing.T) {
	_, socket, repo, projectID := taskDaemon(t)
	t.Chdir(repo)
	for _, args := range [][]string{nil, {"new"}, {"show"}, {"list", "extra"}, {"cancel", "a", "b"}} {
		if _, err := runTask(t, args...); err == nil || !strings.Contains(err.Error(), "usage: maestro task") {
			t.Fatalf("args %v: %v", args, err)
		}
	}
	if _, err := runTask(t, "replay", "x"); err == nil || !strings.Contains(err.Error(), "unknown task command") {
		t.Fatalf("unknown command: %v", err)
	}
	if _, err := runTask(t, "show", "missing", "--socket", socket); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("unknown task: %v", err)
	}
	if _, err := runTask(t, "new", " ", "--socket", socket); err == nil || !strings.Contains(err.Error(), "invalid_request") {
		t.Fatalf("blank description: %v", err)
	}
	absent := filepath.Join(t.TempDir(), "absent.sock")
	if _, err := runTask(t, "list", "--project", projectID, "--socket", absent); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("absent daemon: %v", err)
	}
	if output, err := runTask(t, "list", "--help"); err != nil || !strings.Contains(output, "-project") {
		t.Fatalf("help: %q %v", output, err)
	}
}

func TestTaskConfigUpdatePrintsTheChanges(t *testing.T) {
	server, socket, repo, projectID := taskDaemon(t)
	t.Chdir(repo)
	output, err := runTask(t, "new", "fix the parser", "--socket", socket)
	if err != nil {
		t.Fatalf("new: %v: %s", err, output)
	}
	id := createdID(t, output)

	output, err = runTask(t, "config", "update", id, "--socket", socket)
	if err != nil || !strings.HasPrefix(output, "task "+id+": configuration unchanged (sha256-") {
		t.Fatalf("output %q, error %v", output, err)
	}

	if _, err := server.Tasks.Transition(id, task.Input{Event: task.Assign, Guard: task.Guard{AssignmentAvailable: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Tasks.Transition(id, task.Input{Event: task.Block, Reason: "budget exceeded"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".maestro.toml"), []byte("[budgets]\nmax_turns_per_task = 90\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = runTask(t, "config", "update", "--project", projectID, id, "--socket", socket)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	for _, want := range []string{"task " + id + ": configuration updated", "previous:", "impact:", "ceiling",
		"state:", "BLOCKED (resumes in PLANNING)", "budgets.max_turns_per_task", "changed"} {
		if !strings.Contains(output, want) {
			t.Fatalf("update misses %q: %s", want, output)
		}
	}
	output, err = runTask(t, "show", id, "--socket", socket)
	if err != nil || !strings.Contains(output, "config-update") {
		t.Fatalf("output %q, error %v", output, err)
	}

	for _, args := range [][]string{{"config"}, {"config", "show", id}, {"config", "update"}, {"config", "update", "a", "b"}} {
		if _, err := runTask(t, args...); err == nil || !strings.Contains(err.Error(), "usage: maestro task") {
			t.Fatalf("args %v: %v", args, err)
		}
	}
	if _, err := runTask(t, "config", "update", "missing", "--socket", socket); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("unknown task: %v", err)
	}
}
