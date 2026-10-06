// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/scheduler"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/turn"
	"github.com/goabonga/maestro/internal/worker"
	"github.com/goabonga/maestro/internal/worktree"
)

// workerDaemon serves the daemon API with tasks and workers on a Unix
// socket, over the data directory of the test, and registers one
// repository. It returns the server, the socket, the repository and its
// project.
func workerDaemon(t *testing.T) (*ipc.Server, string, string, worktree.Project) {
	t.Helper()
	return workerDaemonWith(t, func(*ipc.Server) {})
}

// workerDaemonWith is workerDaemon with the server configured before it
// serves.
func workerDaemonWith(t *testing.T, configure func(*ipc.Server)) (*ipc.Server, string, string, worktree.Project) {
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
	server := &ipc.Server{DB: db, Store: store, Service: "maestro-svc", Version: "0.0.0",
		Tasks: &task.Store{DB: db}, Workers: &worker.Store{DB: db}}
	configure(server)
	web := &http.Server{Handler: server.Handler()}
	go func() { _ = web.Serve(listener) }()
	t.Cleanup(func() { _ = web.Close() })
	return server, socket, repo, project
}

// runWorker runs one maestro worker command and returns its output.
func runWorker(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := Run(context.Background(), append([]string{"worker"}, args...), &output, "0.0.0")
	return output.String(), err
}

// registerWorker registers a claude worker in the project.
func registerWorker(t *testing.T, server *ipc.Server, project worktree.Project, name string) {
	t.Helper()
	spec := worker.Spec{Name: name, Agent: "claude", AgentKind: "claude-code", Driver: "claude-code-2.1"}
	if _, err := server.Workers.Register(project, spec); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCommandsReadTheRegistryOfTheCurrentRepository(t *testing.T) {
	server, socket, repo, project := workerDaemon(t)
	t.Chdir(repo)

	output, err := runWorker(t, "list", "--socket", socket)
	if err != nil || output != "no workers\n" {
		t.Fatalf("output %q, error %v", output, err)
	}

	output, err = runTask(t, "new", "add a --verbose flag", "--socket", socket)
	if err != nil {
		t.Fatalf("new: %v: %s", err, output)
	}
	created, err := server.Tasks.Get(createdID(t, output))
	if err != nil {
		t.Fatal(err)
	}
	assigned, err := turn.Store{DB: server.DB}.Create(created.ID, "claude", created.ConfigID)
	if err != nil {
		t.Fatal(err)
	}
	registerWorker(t, server, project, "codex-01")
	registerWorker(t, server, project, "claude-01")
	for _, input := range []worker.Input{
		{Event: worker.Start, Guard: worker.Guard{CapacityReserved: true}},
		{Event: worker.Ready, Guard: worker.Guard{SessionReady: true, ProfileConfirmed: true}},
		{Event: worker.Assign, Reason: "implement the flag",
			Assignment: worker.Assignment{TaskID: created.ID, Role: worker.Implementation, TurnID: assigned.ID},
			Guard:      worker.Guard{AssignmentPersisted: true}},
	} {
		if _, err := server.Workers.Transition(project.ID, "claude-01", input); err != nil {
			t.Fatalf("%s: %v", input.Event, err)
		}
	}

	output, err = runWorker(t, "list", "--socket", socket)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") ||
		!strings.Contains(lines[1], "claude-01") || !strings.Contains(lines[1], "BUSY") ||
		!strings.Contains(lines[1], created.ID+" (implementation)") ||
		!strings.Contains(lines[2], "codex-01") || !strings.Contains(lines[2], "STOPPED") || !strings.HasSuffix(lines[2], "-") {
		t.Fatalf("unexpected list: %q", output)
	}

	output, err = runWorker(t, "show", "claude-01", "--socket", socket)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{"worker:", "claude-01", "project:", project.ID, "agent:", "claude (claude-code)",
		"driver:", "claude-code-2.1", "state:", "BUSY", "task:", created.ID, "role:", "implementation",
		"turn:", assigned.ID, "repository:", project.WorkerRepository("claude-01"),
		"EVENT", "register", "assign", "implement the flag"} {
		if !strings.Contains(output, want) {
			t.Fatalf("show misses %q: %s", want, output)
		}
	}

	output, err = runWorker(t, "show", "codex-01", "--socket", socket)
	if err != nil || !strings.Contains(output, "assignment:") || !strings.Contains(output, "STOPPED") {
		t.Fatalf("output %q, error %v", output, err)
	}
}

func TestWorkerCommandsTakeAnExplicitProject(t *testing.T) {
	server, socket, _, project := workerDaemon(t)
	t.Chdir(t.TempDir())
	registerWorker(t, server, project, "claude-01")

	output, err := runWorker(t, "list", "--project", project.ID, "--socket", socket)
	if err != nil || !strings.Contains(output, "claude-01") {
		t.Fatalf("output %q, error %v", output, err)
	}
	output, err = runWorker(t, "show", "--project", project.ID, "--socket", socket, "claude-01")
	if err != nil || !strings.Contains(output, "claude-01") {
		t.Fatalf("output %q, error %v", output, err)
	}
	if _, err := runWorker(t, "list", "--socket", socket); err == nil || !strings.Contains(err.Error(), "not inside a Git repository") {
		t.Fatalf("outside a repository: %v", err)
	}
	if _, err := runWorker(t, "list", "--project", "missing", "--socket", socket); err == nil || !strings.Contains(err.Error(), "unknown project: missing") {
		t.Fatalf("unknown project: %v", err)
	}
}

func TestWorkerCommandErrors(t *testing.T) {
	_, socket, repo, project := workerDaemon(t)
	t.Chdir(repo)
	for _, args := range [][]string{nil, {"show"}, {"list", "extra"}, {"show", "a", "b"}, {"start"}, {"stop"}, {"start", "a", "b"}} {
		if _, err := runWorker(t, args...); err == nil || !strings.Contains(err.Error(), "usage: maestro worker") {
			t.Fatalf("args %v: %v", args, err)
		}
	}
	if _, err := runWorker(t, "pause", "x"); err == nil || !strings.Contains(err.Error(), "unknown worker command") {
		t.Fatalf("unknown command: %v", err)
	}
	if _, err := runWorker(t, "show", "missing", "--socket", socket); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("unknown worker: %v", err)
	}
	absent := filepath.Join(t.TempDir(), "absent.sock")
	if _, err := runWorker(t, "list", "--project", project.ID, "--socket", absent); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("absent daemon: %v", err)
	}
	if output, err := runWorker(t, "list", "--help"); err != nil || !strings.Contains(output, "-project") {
		t.Fatalf("help: %q %v", output, err)
	}
}

func TestWorkerStartRefusesInvalidCounts(t *testing.T) {
	_, socket, repo, _ := workerDaemon(t)
	t.Chdir(repo)
	if _, err := runWorker(t, "start", "claude-code", "--count", "0", "--socket", socket); err == nil || !strings.Contains(err.Error(), "--count must be at least 1") {
		t.Fatalf("count 0: %v", err)
	}
	if _, err := runWorker(t, "list", "--count", "2", "--socket", socket); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("count on list: %v", err)
	}
	if _, err := runWorker(t, "start", "claude-code", "--socket", socket); err == nil || !strings.Contains(err.Error(), "this daemon does not start workers") {
		t.Fatalf("a daemon without supervisor: %v", err)
	}
}

// Fixture agents: each answers --version on the host like the real CLI.
const (
	// claudeFixture records its chosen session in its private
	// configuration, as Claude Code does, then waits on its terminal.
	claudeFixture = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$2" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$2.jsonl"
exec cat
`
	// crashingCodex exits before recording any session.
	crashingCodex = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli 0.160.0"; exit 0; fi
exit 3
`
)

func TestWorkerStartAndStopFollowTheWorkers(t *testing.T) {
	confined, err := launcher.New()
	if errors.Is(err, launcher.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for name, script := range map[string]string{"claude": claudeFixture, "codex": crashingCodex} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil { // #nosec G306 -- an executable fixture
			t.Fatal(err)
		}
	}
	_, socket, repo, _ := workerDaemonWith(t, func(server *ipc.Server) {
		capacity, err := scheduler.NewCapacity(2, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		server.Capacity = capacity
		server.Supervisor = &worker.Supervisor{
			Store: *server.Workers, Projects: server.Store, Capacity: capacity, Launcher: confined,
			LookPath:     func(name string) (string, error) { return filepath.Join(bin, name), nil },
			StartTimeout: 15 * time.Second, StopGrace: 200 * time.Millisecond,
		}
		t.Cleanup(server.Supervisor.Close)
	})
	t.Chdir(repo)

	output, err := runWorker(t, "start", "codex", "--socket", socket)
	if err == nil || !strings.Contains(err.Error(), "1 of 1 workers failed to start") ||
		!strings.Contains(output, "codex-01: FAILED: start failed:") || !strings.Contains(output, "exited with code 3") {
		t.Fatalf("output %q, error %v", output, err)
	}

	output, err = runWorker(t, "start", "claude-code", "--count", "2", "--socket", socket)
	if err != nil {
		t.Fatalf("start: %v: %s", err, output)
	}
	for _, want := range []string{"starting claude-code-01 (claude-code, driver claude-code-2.1)", "starting claude-code-02",
		"claude-code-01: IDLE", "claude-code-02: IDLE"} {
		if !strings.Contains(output, want) {
			t.Fatalf("start misses %q: %s", want, output)
		}
	}
	if _, err := runWorker(t, "start", "claude-code", "--socket", socket); err == nil || !strings.Contains(err.Error(), "capacity exhausted") {
		t.Fatalf("a start above capacity: %v", err)
	}

	output, err = runWorker(t, "stop", "claude-code-01", "--socket", socket)
	if err != nil || output != "claude-code-01: STOPPED\n" {
		t.Fatalf("output %q, error %v", output, err)
	}
	if _, err := runWorker(t, "stop", "claude-code-01", "--socket", socket); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("a stopped worker was stopped again: %v", err)
	}
}
