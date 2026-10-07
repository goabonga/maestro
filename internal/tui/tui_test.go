// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package tui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/scheduler"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/turn"
	"github.com/goabonga/maestro/internal/worker"
	"github.com/goabonga/maestro/internal/worktree"
)

// gitRepo returns a temporary repository with one commit.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	return dir
}

// daemon serves the daemon API with capacity, tasks and workers on a
// Unix socket, over the data directory of the test, with one registered
// repository. It returns the socket and the project id.
func daemon(t *testing.T) (string, string) {
	t.Helper()
	_, socket, project := daemonWith(t, func(*ipc.Server) {})
	return socket, project.ID
}

// daemonWith is daemon with the server configured before it serves; it
// returns the server, the socket and the project.
func daemonWith(t *testing.T, configure func(*ipc.Server)) (*ipc.Server, string, worktree.Project) {
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
	project, _, err := store.Init(gitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	capacity, err := scheduler.NewCapacity(3, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capacity.Reserve(scheduler.Sessions, project.ID); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "svc.sock")
	listener, err := transport.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &ipc.Server{DB: db, Store: store, Service: "maestro-svc", Version: "0.0.0",
		Capacity: capacity, Tasks: &task.Store{DB: db}, Workers: &worker.Store{DB: db}}
	configure(server)
	web := &http.Server{Handler: server.Handler()}
	go func() { _ = web.Serve(listener) }()
	t.Cleanup(func() { _ = web.Close() })
	return server, socket, project
}

// createTask creates a task over the daemon API and returns its id.
func createTask(t *testing.T, socket, project, description string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"project_id": project, "description": description})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://maestro/v1/tasks", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	sum := sha256.Sum256([]byte(description))
	request.Header.Set("Idempotency-Key", hex.EncodeToString(sum[:]))
	response, err := transport.Client(socket).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var envelope struct {
		Data struct {
			ID string `json:"task_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("create: %s, %v", response.Status, err)
	}
	return envelope.Data.ID
}

// send applies msg to the model, then runs the refresh the model asks
// for, if any, and applies its result.
func send(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, cmd := m.Update(msg)
	m = next.(Model)
	if cmd == nil {
		return m
	}
	if result, ok := cmd().(refreshedMsg); ok {
		next, _ = m.Update(result)
		m = next.(Model)
	}
	return m
}

// start returns a model with its first refresh applied.
func start(t *testing.T, options Options) Model {
	t.Helper()
	m := New(options)
	next, _ := m.Update(m.fetch()())
	return next.(Model)
}

// key returns the message of one key press.
func key(name string) tea.KeyMsg {
	switch name {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)}
}

// contains fails unless view holds every wanted text.
func contains(t *testing.T, view string, want ...string) {
	t.Helper()
	for _, text := range want {
		if !strings.Contains(view, text) {
			t.Fatalf("view misses %q:\n%s", text, view)
		}
	}
}

func TestDashboardShowsStatusCapacityAndProjects(t *testing.T) {
	socket, project := daemon(t)
	m := New(Options{Socket: socket})
	contains(t, m.View(), "maestro · dashboard", "loading…")

	m = start(t, Options{Socket: socket})
	contains(t, m.View(), "daemon: maestro-svc 0.0.0", "CAPACITY", "sessions", project+": 1",
		"PROJECT", "> "+project, "ok", "enter tasks")
}

func TestNavigatesFromProjectToTaskDetailAndBack(t *testing.T) {
	socket, project := daemon(t)
	first := createTask(t, socket, project, "add a verbose flag")
	second := createTask(t, socket, project, "write the changelog\nwith details")
	m := start(t, Options{Socket: socket})

	m = send(t, m, key("enter"))
	contains(t, m.View(), "maestro · tasks of "+project, "> "+first, second, "NEW",
		"add a verbose flag", "write the changelog", "esc back")
	if strings.Contains(m.View(), "with details") {
		t.Fatalf("the list shows more than the first line:\n%s", m.View())
	}

	m = send(t, m, key("down"))
	m = send(t, m, key("down"))
	contains(t, m.View(), "> "+second)
	m = send(t, m, key("enter"))
	contains(t, m.View(), "maestro · task "+second, "state:", "NEW", "maestro/task-"+second,
		"fix cycles:", "write the changelog", "with details", "EVENT", "created")

	m = send(t, m, key("esc"))
	contains(t, m.View(), "maestro · tasks of "+project, "> "+second)
	m = send(t, m, key("esc"))
	contains(t, m.View(), "maestro · dashboard", "> "+project)
}

// registerWorker registers a STOPPED claude worker in the project.
func registerWorker(t *testing.T, server *ipc.Server, project worktree.Project, name string) {
	t.Helper()
	spec := worker.Spec{Name: name, Agent: "claude", AgentKind: "claude-code", Driver: "claude-code-2.1"}
	if _, err := server.Workers.Register(project, spec); err != nil {
		t.Fatal(err)
	}
}

// assignWorker starts a registered worker and assigns it the
// implementation of a task, with a reason; it returns the turn id.
func assignWorker(t *testing.T, server *ipc.Server, project worktree.Project, name, taskID, reason string) string {
	t.Helper()
	created, err := server.Tasks.Get(taskID)
	if err != nil {
		t.Fatal(err)
	}
	assigned, err := turn.Store{DB: server.DB}.Create(created.ID, "claude", created.ConfigID)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []worker.Input{
		{Event: worker.Start, Guard: worker.Guard{CapacityReserved: true}},
		{Event: worker.Ready, Guard: worker.Guard{SessionReady: true, ProfileConfirmed: true}},
		{Event: worker.Assign, Reason: reason,
			Assignment: worker.Assignment{TaskID: taskID, Role: worker.Implementation, TurnID: assigned.ID},
			Guard:      worker.Guard{AssignmentPersisted: true}},
	} {
		if _, err := server.Workers.Transition(project.ID, name, input); err != nil {
			t.Fatalf("%s: %v", input.Event, err)
		}
	}
	return assigned.ID
}

func TestWorkersScreenListsTheProjectWorkersAndShowsOne(t *testing.T) {
	server, socket, project := daemonWith(t, func(*ipc.Server) {})
	m := start(t, Options{Socket: socket})
	m = send(t, m, key("w"))
	contains(t, m.View(), "maestro · workers of "+project.ID, "no workers")

	taskID := createTask(t, socket, project.ID, "add a verbose flag")
	registerWorker(t, server, project, "codex-01")
	registerWorker(t, server, project, "claude-01")
	turnID := assignWorker(t, server, project, "claude-01", taskID, "implement the flag")
	m = send(t, m, key("r"))
	contains(t, m.View(), "NAME", "AGENT", "STATE", "TASK", "ROLE", "UPDATED", "REASON",
		"> claude-01", "BUSY", taskID, "implementation", "codex-01", "STOPPED", "enter show")

	m = send(t, m, key("down"))
	contains(t, m.View(), "> codex-01")
	m = send(t, m, key("up"))
	m = send(t, m, key("enter"))
	contains(t, m.View(), "maestro · worker claude-01", "project:", project.ID, "agent:",
		"claude (claude-code)", "driver:", "claude-code-2.1", "state:", "BUSY", "task:", taskID,
		"role:", "implementation", "turn:", turnID, "repository:", project.WorkerRepository("claude-01"),
		"EVENT", "register", "assign", "implement the flag")

	m = send(t, m, key("esc"))
	contains(t, m.View(), "maestro · workers of "+project.ID, "> claude-01")
	m = send(t, m, key("esc"))
	contains(t, m.View(), "maestro · dashboard", "> "+project.ID)
}

func TestWorkersScreenOpensFromTheTasksAndReturnsThere(t *testing.T) {
	server, socket, project := daemonWith(t, func(*ipc.Server) {})
	registerWorker(t, server, project, "claude-01")
	m := start(t, Options{Socket: socket, Project: project.ID})
	contains(t, m.View(), "w workers")

	m = send(t, m, key("w"))
	contains(t, m.View(), "maestro · workers of "+project.ID, "> claude-01", "STOPPED")
	m = send(t, m, key("enter"))
	contains(t, m.View(), "maestro · worker claude-01", "assignment:", "-")
	// w only opens the workers from a project or its tasks.
	m = send(t, m, key("w"))
	contains(t, m.View(), "maestro · worker claude-01")
	m = send(t, m, key("esc"))
	m = send(t, m, key("esc"))
	contains(t, m.View(), "maestro · tasks of "+project.ID)
}

func TestWorkersScreenShowsTheDaemonError(t *testing.T) {
	socket, project := daemon(t)
	m := start(t, Options{Socket: socket, Project: project})
	m = send(t, m, key("w"))
	// enter on an empty list opens nothing.
	m = send(t, m, key("enter"))
	contains(t, m.View(), "maestro · workers of "+project, "no workers")
	m.screen, m.workerName = workerScreen, "missing"
	m = send(t, m, key("r"))
	contains(t, m.View(), "not_found: unknown worker in project "+project+": missing")

	_, without, other := daemonWith(t, func(server *ipc.Server) { server.Workers = nil })
	m = start(t, Options{Socket: without, Project: other.ID})
	m = send(t, m, key("w"))
	contains(t, m.View(), "maestro · workers of "+other.ID, "not_found: this daemon does not serve workers")
}

func TestTaskDetailShowsTheAssignedWorker(t *testing.T) {
	server, socket, project := daemonWith(t, func(*ipc.Server) {})
	first := createTask(t, socket, project.ID, "add a verbose flag")
	createTask(t, socket, project.ID, "write the changelog")
	registerWorker(t, server, project, "claude-01")
	registerWorker(t, server, project, "codex-01")
	assignWorker(t, server, project, "claude-01", first, "implement the flag")
	m := start(t, Options{Socket: socket, Project: project.ID})

	m = send(t, m, key("enter"))
	contains(t, m.View(), "maestro · task "+first, "worker:", "claude-01 (implementation, BUSY)")
	if strings.Contains(m.View(), "codex-01") {
		t.Fatalf("the detail shows a worker of no assignment:\n%s", m.View())
	}

	m = send(t, m, key("esc"))
	m = send(t, m, key("down"))
	m = send(t, m, key("enter"))
	contains(t, m.View(), "maestro · task ", "write the changelog")
	if strings.Contains(m.View(), "claude-01") || !strings.Contains(m.View(), "worker:") {
		t.Fatalf("the detail of an unassigned task:\n%s", m.View())
	}
}

func TestTaskDetailWithoutWorkersOmitsTheWorker(t *testing.T) {
	_, socket, project := daemonWith(t, func(server *ipc.Server) { server.Workers = nil })
	createTask(t, socket, project.ID, "add a verbose flag")
	m := start(t, Options{Socket: socket, Project: project.ID})
	m = send(t, m, key("enter"))
	contains(t, m.View(), "add a verbose flag", "state:")
	if strings.Contains(m.View(), "worker:") {
		t.Fatalf("the detail shows workers the daemon does not serve:\n%s", m.View())
	}
}

func TestProjectOptionOpensItsTasks(t *testing.T) {
	socket, project := daemon(t)
	m := start(t, Options{Socket: socket, Project: project})
	contains(t, m.View(), "maestro · tasks of "+project, "no tasks")

	createTask(t, socket, project, "first task")
	m = send(t, m, key("r"))
	contains(t, m.View(), "first task")
}

func TestUnknownProjectShowsTheDaemonError(t *testing.T) {
	socket, _ := daemon(t)
	m := start(t, Options{Socket: socket, Project: "missing"})
	contains(t, m.View(), "daemon: maestro-svc", "not_found: unknown project: missing")
}

func TestDaemonDownIsReportedAndRetried(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "absent.sock")
	m := start(t, Options{Socket: socket, Interval: time.Minute})
	contains(t, m.View(), "daemon not reachable at "+socket, "retrying every 1m0s")

	// A tick refreshes; the daemon answering again is shown.
	next, cmd := m.Update(tickMsg{})
	m = next.(Model)
	if cmd == nil || !m.loading {
		t.Fatal("a tick does not refresh")
	}
	live, _ := daemon(t)
	m.client = newClient(live)
	m = send(t, m, key("r"))
	contains(t, m.View(), "daemon: maestro-svc 0.0.0")
}

func TestStaleRefreshIsIgnored(t *testing.T) {
	socket, project := daemon(t)
	m := start(t, Options{Socket: socket})
	stale := m.refresh()
	m = send(t, m, key("enter"))
	next, _ := m.Update(stale())
	m = next.(Model)
	contains(t, m.View(), "maestro · tasks of "+project, "no tasks")
}

func TestTickWhileLoadingOnlyRearmsTheTimer(t *testing.T) {
	socket, _ := daemon(t)
	m := New(Options{Socket: socket})
	seq := m.seq
	next, cmd := m.Update(tickMsg{})
	if cmd == nil || next.(Model).seq != seq {
		t.Fatal("a tick during a refresh starts another one")
	}
}

func TestViewNeutralizesControlCharacters(t *testing.T) {
	socket, project := daemon(t)
	createTask(t, socket, project, "evil \x1b[2J title")
	m := start(t, Options{Socket: socket, Project: project})
	view := m.View()
	if strings.Contains(view, "\x1b") {
		t.Fatalf("view carries an escape sequence: %q", view)
	}
	contains(t, view, "evil ?[2J title")
}

func TestViewFitsTheWindowWidth(t *testing.T) {
	socket, _ := daemon(t)
	m := start(t, Options{Socket: socket})
	m = send(t, m, tea.WindowSizeMsg{Width: 20, Height: 10})
	for _, line := range strings.Split(m.View(), "\n") {
		if len([]rune(line)) > 20 {
			t.Fatalf("line wider than the window: %q", line)
		}
	}
}

func TestQuitKeys(t *testing.T) {
	m := New(Options{Socket: "unused"})
	for _, k := range []tea.KeyMsg{key("q"), {Type: tea.KeyCtrlC}} {
		_, cmd := m.Update(k)
		if cmd == nil {
			t.Fatalf("%s does not quit", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s does not quit", k)
		}
	}
}

// watchWriter records the output of a program and signals once it holds
// a text.
type watchWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	want   string
	seen   chan struct{}
	once   sync.Once
}

func (w *watchWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buffer.Write(p)
	if strings.Contains(w.buffer.String(), w.want) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func TestRunShowsTheDashboardUntilQuit(t *testing.T) {
	socket, project := daemon(t)
	input, keys := io.Pipe()
	output := &watchWriter{want: project, seen: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), Options{Socket: socket}, input, output) }()

	select {
	case <-output.seen:
	case err := <-done:
		t.Fatalf("the program ended before showing the project: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the dashboard never showed the project")
	}
	if _, err := keys.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("q does not quit")
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	input, _ := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Socket: filepath.Join(t.TempDir(), "absent.sock")}, input, io.Discard)
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the program ignores its context")
	}
}
