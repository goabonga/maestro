// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/scheduler"
	"github.com/goabonga/maestro/internal/turn"
	"github.com/goabonga/maestro/internal/worker"
	"github.com/goabonga/maestro/internal/worktree"
)

// workerServer builds a server serving tasks and workers with one
// registered project.
func workerServer(t *testing.T) (*Server, *httptest.Server, worktree.Project) {
	t.Helper()
	server, web, project := taskServer(t)
	server.Workers = &worker.Store{DB: server.DB}
	return server, web, project
}

// registerWorker registers a claude worker in a project.
func registerWorker(t *testing.T, server *Server, project worktree.Project, name string) {
	t.Helper()
	spec := worker.Spec{Name: name, Agent: "claude", AgentKind: "claude-code", Driver: "claude-code-2.1"}
	if _, err := server.Workers.Register(project, spec); err != nil {
		t.Fatal(err)
	}
}

// stepWorker applies inputs to a worker in order.
func stepWorker(t *testing.T, server *Server, project worktree.Project, name string, inputs ...worker.Input) {
	t.Helper()
	for _, input := range inputs {
		if _, err := server.Workers.Transition(project.ID, name, input); err != nil {
			t.Fatalf("%s: %v", input.Event, err)
		}
	}
}

// decodeWorkers reads the worker views of an envelope.
func decodeWorkers[T any](t *testing.T, envelope Envelope) T {
	t.Helper()
	raw, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var views T
	if err := json.Unmarshal(raw, &views); err != nil {
		t.Fatalf("not a worker document: %s", raw)
	}
	return views
}

func TestWorkerRoutesNeedAWorkerStore(t *testing.T) {
	_, web := newServer(t)
	for _, path := range []string{"/v1/workers?project_id=p", "/v1/workers/claude-01?project_id=p"} {
		status, envelope, raw := call(t, web, "GET", path, nil, "")
		if status != http.StatusNotFound || envelope.Error == nil || envelope.Error.Message != "this daemon does not serve workers" {
			t.Fatalf("%s: status=%d body=%s", path, status, raw)
		}
	}
}

func TestListWorkersIsScopedToOneProject(t *testing.T) {
	server, web, project := workerServer(t)
	other, _, err := server.Store.Init(repository(t))
	if err != nil {
		t.Fatal(err)
	}
	status, envelope, raw := call(t, web, "GET", "/v1/workers?project_id="+project.ID, nil, "")
	if status != http.StatusOK || len(decodeWorkers[[]workerView](t, envelope)) != 0 {
		t.Fatalf("empty registry: status=%d body=%s", status, raw)
	}

	registerWorker(t, server, project, "codex-01")
	registerWorker(t, server, other, "elsewhere")
	registerWorker(t, server, project, "claude-01")
	stepWorker(t, server, project, "claude-01", worker.Input{Event: worker.Start, Guard: worker.Guard{CapacityReserved: true}})

	status, envelope, raw = call(t, web, "GET", "/v1/workers?project_id="+project.ID, nil, "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	views := decodeWorkers[[]workerView](t, envelope)
	if len(views) != 2 || views[0].Name != "claude-01" || views[1].Name != "codex-01" {
		t.Fatalf("views=%+v", views)
	}
	if first := views[0]; first.ProjectID != project.ID || first.Agent != "claude" || first.AgentKind != "claude-code" ||
		first.Driver != "claude-code-2.1" || first.State != string(worker.Starting) || first.Version != 2 ||
		first.Repository != project.WorkerRepository("claude-01") || first.Assignment != nil || first.Events != nil {
		t.Fatalf("first=%+v", first)
	}

	for path, want := range map[string]int{
		"/v1/workers":                    http.StatusBadRequest,
		"/v1/workers?project_id=missing": http.StatusNotFound,
	} {
		status, _, raw := call(t, web, "GET", path, nil, "")
		if status != want {
			t.Fatalf("%s: status=%d body=%s", path, status, raw)
		}
	}
}

func TestShowWorkerReturnsItsAssignmentAndEventsWithinItsProject(t *testing.T) {
	server, web, project := workerServer(t)
	other, _, err := server.Store.Init(repository(t))
	if err != nil {
		t.Fatal(err)
	}
	created := newTask(t, web, project.ID, "k1", "first")
	assigned, err := turn.Store{DB: server.DB}.Create(created.ID, "claude", created.ConfigID)
	if err != nil {
		t.Fatal(err)
	}
	registerWorker(t, server, project, "claude-01")
	stepWorker(t, server, project, "claude-01",
		worker.Input{Event: worker.Start, Guard: worker.Guard{CapacityReserved: true}},
		worker.Input{Event: worker.Ready, Guard: worker.Guard{SessionReady: true, ProfileConfirmed: true}},
		worker.Input{Event: worker.Assign, Reason: "planning turn",
			Assignment: worker.Assignment{TaskID: created.ID, Role: worker.Planning, TurnID: assigned.ID},
			Guard:      worker.Guard{AssignmentPersisted: true}})

	status, envelope, raw := call(t, web, "GET", "/v1/workers/claude-01?project_id="+project.ID, nil, "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	shown := decodeWorkers[workerView](t, envelope)
	if shown.Name != "claude-01" || shown.State != string(worker.Busy) || shown.Assignment == nil ||
		*shown.Assignment != (assignmentView{TaskID: created.ID, Role: string(worker.Planning), TurnID: assigned.ID}) {
		t.Fatalf("shown=%+v", shown)
	}
	if len(shown.Events) != 4 || shown.Events[0].Event != string(worker.Registered) || shown.Events[0].From != "" ||
		shown.Events[3].Event != string(worker.Assign) || shown.Events[3].TaskID != created.ID ||
		shown.Events[3].TurnID != assigned.ID || shown.Events[3].Reason != "planning turn" {
		t.Fatalf("events=%+v", shown.Events)
	}

	for _, path := range []string{
		"/v1/workers/claude-01?project_id=" + other.ID,
		"/v1/workers/missing?project_id=" + project.ID,
		"/v1/workers/claude-01?project_id=missing",
	} {
		status, envelope, raw := call(t, web, "GET", path, nil, "")
		if status != http.StatusNotFound || envelope.Error == nil || envelope.Error.Code != CodeNotFound {
			t.Fatalf("%s: status=%d body=%s", path, status, raw)
		}
	}
	if status, _, raw := call(t, web, "GET", "/v1/workers/claude-01", nil, ""); status != http.StatusBadRequest {
		t.Fatalf("no project: status=%d body=%s", status, raw)
	}
}

func TestShowWorkerKeepsOnlyTheRecentEvents(t *testing.T) {
	server, web, project := workerServer(t)
	registerWorker(t, server, project, "claude-01")
	cycle := []worker.Input{
		{Event: worker.Start, Guard: worker.Guard{CapacityReserved: true}},
		{Event: worker.Stop, Guard: worker.Guard{Reconciled: true}},
	}
	for i := 0; i < RecentWorkerEvents; i++ {
		cycle[1].Reason = fmt.Sprintf("stop %d", i)
		stepWorker(t, server, project, "claude-01", cycle...)
	}
	status, envelope, raw := call(t, web, "GET", "/v1/workers/claude-01?project_id="+project.ID, nil, "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	events := decodeWorkers[workerView](t, envelope).Events
	last := fmt.Sprintf("stop %d", RecentWorkerEvents-1)
	if len(events) != RecentWorkerEvents || events[0].Event != string(worker.Start) ||
		events[len(events)-1].Reason != last {
		t.Fatalf("events=%+v", events)
	}
}

// claudeFixture stands for Claude Code: it answers --version on the
// host, and in its confined PTY records its chosen session in its
// private configuration, draws its terminal, then waits on it.
const claudeFixture = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$2" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$2.jsonl"
echo READY
exec cat
`

// supervisedServer builds a server starting workers of the fixture
// agent, with the given session ceiling.
func supervisedServer(t *testing.T, sessions int) (*Server, *httptest.Server, worktree.Project) {
	t.Helper()
	confined, err := launcher.New()
	if errors.Is(err, launcher.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	server, web, project := workerServer(t)
	capacity, err := scheduler.NewCapacity(sessions, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(binary, []byte(claudeFixture), 0o700); err != nil { // #nosec G306 -- an executable fixture
		t.Fatal(err)
	}
	server.Capacity = capacity
	server.Supervisor = &worker.Supervisor{
		Store: *server.Workers, Projects: server.Store, Capacity: capacity, Launcher: confined,
		LookPath: func(name string) (string, error) {
			if name != "claude" {
				return "", errors.New("not found")
			}
			return binary, nil
		},
		StartTimeout: 15 * time.Second, StopGrace: 200 * time.Millisecond,
	}
	t.Cleanup(server.Supervisor.Close)
	return server, web, project
}

// awaitWorker polls a worker over the API until it reaches a state.
func awaitWorker(t *testing.T, web *httptest.Server, project worktree.Project, name string, want worker.State) workerView {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		status, envelope, raw := call(t, web, "GET", "/v1/workers/"+name+"?project_id="+project.ID, nil, "")
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, raw)
		}
		view := decodeWorkers[workerView](t, envelope)
		if view.State == string(want) {
			return view
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s (%s), want %s", name, view.State, view.Reason, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorkerStartAndStopNeedASupervisor(t *testing.T) {
	_, web, project := workerServer(t)
	for path, document := range map[string]string{
		"/v1/workers":                  `{"project_id": "` + project.ID + `", "agent": "claude-code", "count": 1}`,
		"/v1/workers/claude-01/stop":   `{"project_id": "` + project.ID + `"}`,
		"/v1/workers/claude-01/pause":  `{"project_id": "` + project.ID + `"}`,
		"/v1/workers/claude-01/resume": `{"project_id": "` + project.ID + `"}`,
	} {
		status, envelope, raw := call(t, web, "POST", path, map[string]string{"Idempotency-Key": path}, document)
		if status != http.StatusNotFound || envelope.Error == nil || envelope.Error.Message != "this daemon does not start workers" {
			t.Fatalf("%s: status=%d body=%s", path, status, raw)
		}
	}
}

func TestStartWorkersUpToCapacityThenStopThem(t *testing.T) {
	server, web, project := supervisedServer(t, 2)
	start := `{"project_id": "` + project.ID + `", "agent": "claude-code", "count": 2}`
	status, envelope, raw := call(t, web, "POST", "/v1/workers", map[string]string{"Idempotency-Key": "s1"}, start)
	if status != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	started := decodeWorkers[[]workerView](t, envelope)
	if len(started) != 2 || started[0].Name != "claude-code-01" || started[1].Name != "claude-code-02" ||
		started[0].State != string(worker.Starting) || started[0].Driver != "claude-code-2.1" {
		t.Fatalf("started=%+v", started)
	}
	// A retry with the same key replays the start, and starts nothing.
	if replayed, _, again := call(t, web, "POST", "/v1/workers", map[string]string{"Idempotency-Key": "s1"}, start); replayed != http.StatusAccepted || string(again) != string(raw) {
		t.Fatalf("replay: status=%d body=%s", replayed, again)
	}
	for _, view := range started {
		idle := awaitWorker(t, web, project, view.Name, worker.Idle)
		if !strings.Contains(idle.Reason, "native session") {
			t.Fatalf("idle=%+v", idle)
		}
	}
	if used := server.Capacity.Usage()[scheduler.Sessions].Used; used != 2 {
		t.Fatalf("sessions used %d", used)
	}

	for document, want := range map[string]int{
		`{"project_id": "` + project.ID + `", "agent": "claude-code", "count": 1}`: http.StatusConflict,
		`{"project_id": "` + project.ID + `", "agent": "claude-code", "count": 0}`: http.StatusBadRequest,
		`{"project_id": "` + project.ID + `", "agent": "gpt", "count": 1}`:         http.StatusBadRequest,
		`{"project_id": "` + project.ID + `", "agent": "codex", "count": 1}`:       http.StatusConflict,
		`{"project_id": "` + project.ID + `"}`:                                     http.StatusBadRequest,
		`{"project_id": "missing", "agent": "claude-code", "count": 1}`:            http.StatusNotFound,
	} {
		status, _, raw := call(t, web, "POST", "/v1/workers", map[string]string{"Idempotency-Key": document}, document)
		if status != want {
			t.Fatalf("%s: status=%d body=%s", document, status, raw)
		}
	}

	stop := `{"project_id": "` + project.ID + `"}`
	status, envelope, raw = call(t, web, "POST", "/v1/workers/claude-code-01/stop", map[string]string{"Idempotency-Key": "t1"}, stop)
	if status != http.StatusOK || decodeWorkers[workerView](t, envelope).State != string(worker.Stopped) {
		t.Fatalf("stop: status=%d body=%s", status, raw)
	}
	if used := server.Capacity.Usage()[scheduler.Sessions].Used; used != 1 {
		t.Fatalf("the stop kept its slot: %d used", used)
	}
	for path, want := range map[string]int{
		"/v1/workers/claude-code-01/stop": http.StatusConflict,
		"/v1/workers/missing/stop":        http.StatusNotFound,
	} {
		status, _, raw := call(t, web, "POST", path, map[string]string{"Idempotency-Key": "again" + path}, stop)
		if status != want {
			t.Fatalf("%s: status=%d body=%s", path, status, raw)
		}
	}
}

func TestPauseAndResumeAWorker(t *testing.T) {
	server, web, project := supervisedServer(t, 1)
	start := `{"project_id": "` + project.ID + `", "agent": "claude-code", "count": 1}`
	if status, _, raw := call(t, web, "POST", "/v1/workers", map[string]string{"Idempotency-Key": "s1"}, start); status != http.StatusAccepted {
		t.Fatalf("start: status=%d body=%s", status, raw)
	}
	awaitWorker(t, web, project, "claude-code-01", worker.Idle)

	body := `{"project_id": "` + project.ID + `"}`
	status, envelope, raw := call(t, web, "POST", "/v1/workers/claude-code-01/pause", map[string]string{"Idempotency-Key": "p1"}, body)
	if status != http.StatusOK || decodeWorkers[workerView](t, envelope).State != string(worker.Paused) {
		t.Fatalf("pause: status=%d body=%s", status, raw)
	}
	// A retry with the same key replays the pause; a new one conflicts.
	if replayed, _, again := call(t, web, "POST", "/v1/workers/claude-code-01/pause", map[string]string{"Idempotency-Key": "p1"}, body); replayed != http.StatusOK || string(again) != string(raw) {
		t.Fatalf("replay: status=%d body=%s", replayed, again)
	}
	if used := server.Capacity.Usage()[scheduler.Sessions].Used; used != 1 {
		t.Fatalf("the paused worker released its slot: %d used", used)
	}
	for path, want := range map[string]int{
		"/v1/workers/claude-code-01/pause": http.StatusConflict,
		"/v1/workers/missing/pause":        http.StatusNotFound,
		"/v1/workers/missing/resume":       http.StatusNotFound,
	} {
		status, _, raw := call(t, web, "POST", path, map[string]string{"Idempotency-Key": "again" + path}, body)
		if status != want {
			t.Fatalf("%s: status=%d body=%s", path, status, raw)
		}
	}
	if status, _, raw := call(t, web, "POST", "/v1/workers/claude-code-01/pause", map[string]string{"Idempotency-Key": "bad"}, `{}`); status != http.StatusBadRequest {
		t.Fatalf("pause without a project: status=%d body=%s", status, raw)
	}

	status, envelope, raw = call(t, web, "POST", "/v1/workers/claude-code-01/resume", map[string]string{"Idempotency-Key": "r1"}, body)
	if status != http.StatusAccepted || decodeWorkers[workerView](t, envelope).State != string(worker.Starting) {
		t.Fatalf("resume: status=%d body=%s", status, raw)
	}
	idle := awaitWorker(t, web, project, "claude-code-01", worker.Idle)
	if !strings.Contains(idle.Reason, "resumed") {
		t.Fatalf("idle=%+v", idle)
	}
	if status, _, raw := call(t, web, "POST", "/v1/workers/claude-code-01/resume", map[string]string{"Idempotency-Key": "r2"}, body); status != http.StatusConflict {
		t.Fatalf("resume of an IDLE worker: status=%d body=%s", status, raw)
	}
}
