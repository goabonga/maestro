// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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
