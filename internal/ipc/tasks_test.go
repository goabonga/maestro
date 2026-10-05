// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/worktree"
)

// taskServer builds a server serving tasks with one registered project.
func taskServer(t *testing.T) (*Server, *httptest.Server, worktree.Project) {
	t.Helper()
	server, web := newServer(t)
	server.Tasks = &task.Store{DB: server.DB}
	project, _, err := server.Store.Init(repository(t))
	if err != nil {
		t.Fatal(err)
	}
	return server, web, project
}

// decodeTask reads the task view of an envelope.
func decodeTask(t *testing.T, envelope Envelope) taskView {
	t.Helper()
	raw, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var view taskView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("not a task: %s", raw)
	}
	return view
}

// body renders a JSON request body.
func body(t *testing.T, document map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// newTask creates a task over the API.
func newTask(t *testing.T, web *httptest.Server, projectID, key, description string) taskView {
	t.Helper()
	status, envelope, raw := call(t, web, "POST", "/v1/tasks", map[string]string{"Idempotency-Key": key},
		body(t, map[string]string{"project_id": projectID, "description": description}))
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	return decodeTask(t, envelope)
}

// integrationHeadOf reads the integration head of a project with git.
func integrationHeadOf(t *testing.T, project worktree.Project) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "refs/heads/maestro/integration")
	cmd.Dir = project.Repository()
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func TestCreateTaskSnapshotsConfigAndStartsFromIntegration(t *testing.T) {
	server, web, project := taskServer(t)
	headers := map[string]string{"Idempotency-Key": "key-1", "X-Request-Id": "req-1"}
	request := body(t, map[string]string{"project_id": project.ID, "description": "  add a --verbose flag\n"})
	status, envelope, first := call(t, web, "POST", "/v1/tasks", headers, request)
	if status != http.StatusCreated || envelope.Error != nil || envelope.RequestID != "req-1" {
		t.Fatalf("status=%d body=%s", status, first)
	}
	created := decodeTask(t, envelope)
	if created.State != string(task.New) || created.ProjectID != project.ID || created.Description != "add a --verbose flag" ||
		created.BaseSHA != integrationHeadOf(t, project) || created.Branch != worktree.TaskBranch(created.ID) {
		t.Fatalf("created=%+v", created)
	}
	if _, err := config.LoadSnapshot(server.DB, created.ConfigID); err != nil {
		t.Fatalf("snapshot not persisted: %v", err)
	}

	// A retry replays the stored response: no second task is created.
	status, _, second := call(t, web, "POST", "/v1/tasks", headers, request)
	if status != http.StatusCreated || !bytes.Equal(first, second) {
		t.Fatalf("status=%d first=%s second=%s", status, first, second)
	}
	tasks, err := server.Tasks.List(project.ID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
}

func TestCreateTaskErrors(t *testing.T) {
	_, web, project := taskServer(t)
	cases := []struct {
		name    string
		headers map[string]string
		body    string
		status  int
		code    string
	}{
		{"no idempotency key", nil, body(t, map[string]string{"project_id": project.ID, "description": "x"}), http.StatusBadRequest, CodeInvalidRequest},
		{"no project", map[string]string{"Idempotency-Key": "k1"}, body(t, map[string]string{"description": "x"}), http.StatusBadRequest, CodeInvalidRequest},
		{"unknown project", map[string]string{"Idempotency-Key": "k2"}, body(t, map[string]string{"project_id": "missing", "description": "x"}), http.StatusNotFound, CodeNotFound},
		{"blank description", map[string]string{"Idempotency-Key": "k3"}, body(t, map[string]string{"project_id": project.ID, "description": " "}), http.StatusBadRequest, CodeInvalidRequest},
		{"unknown field", map[string]string{"Idempotency-Key": "k4"}, body(t, map[string]string{"project_id": project.ID, "description": "x", "agent": "claude"}), http.StatusBadRequest, CodeInvalidRequest},
		{"not JSON", map[string]string{"Idempotency-Key": "k5"}, "project", http.StatusBadRequest, CodeInvalidRequest},
	}
	for _, c := range cases {
		status, envelope, raw := call(t, web, "POST", "/v1/tasks", c.headers, c.body)
		if status != c.status || envelope.Error == nil || envelope.Error.Code != c.code {
			t.Fatalf("%s: status=%d body=%s", c.name, status, raw)
		}
	}
}

func TestCreateTaskRefusesAnInvalidConfiguration(t *testing.T) {
	_, web, project := taskServer(t)
	workTree := filepath.Dir(project.UserRepository)
	if err := os.WriteFile(filepath.Join(workTree, config.ProjectFile), []byte("unknown_key = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, envelope, raw := call(t, web, "POST", "/v1/tasks", map[string]string{"Idempotency-Key": "k1"},
		body(t, map[string]string{"project_id": project.ID, "description": "x"}))
	if status != http.StatusBadRequest || envelope.Error == nil || envelope.Error.Code != CodeInvalidRequest {
		t.Fatalf("status=%d body=%s", status, raw)
	}
}

func TestCreateTaskRefusesAMissingRepository(t *testing.T) {
	_, web, project := taskServer(t)
	workTree := filepath.Dir(project.UserRepository)
	if err := os.Rename(workTree, workTree+".moved"); err != nil {
		t.Fatal(err)
	}
	status, envelope, raw := call(t, web, "POST", "/v1/tasks", map[string]string{"Idempotency-Key": "k1"},
		body(t, map[string]string{"project_id": project.ID, "description": "x"}))
	if status != http.StatusConflict || envelope.Error == nil || !strings.Contains(envelope.Error.Message, "relocate") {
		t.Fatalf("status=%d body=%s", status, raw)
	}
}

func TestTaskRoutesNeedATaskStore(t *testing.T) {
	_, web := newServer(t)
	status, envelope, raw := call(t, web, "GET", "/v1/tasks?project_id=p", nil, "")
	if status != http.StatusNotFound || envelope.Error == nil || envelope.Error.Code != CodeNotFound {
		t.Fatalf("status=%d body=%s", status, raw)
	}
}

func TestListTasksIsScopedToOneProject(t *testing.T) {
	server, web, project := taskServer(t)
	other, _, err := server.Store.Init(repository(t))
	if err != nil {
		t.Fatal(err)
	}
	first := newTask(t, web, project.ID, "k1", "first")
	newTask(t, web, other.ID, "k2", "elsewhere")
	second := newTask(t, web, project.ID, "k3", "second")

	status, envelope, raw := call(t, web, "GET", "/v1/tasks?project_id="+project.ID, nil, "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	listed, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var views []taskView
	if err := json.Unmarshal(listed, &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].ID != first.ID || views[1].ID != second.ID {
		t.Fatalf("views=%+v", views)
	}

	status, _, raw = call(t, web, "GET", "/v1/tasks", nil, "")
	if status != http.StatusBadRequest {
		t.Fatalf("listing without a project: status=%d body=%s", status, raw)
	}
	status, _, raw = call(t, web, "GET", "/v1/tasks?project_id=missing", nil, "")
	if status != http.StatusNotFound {
		t.Fatalf("listing an unknown project: status=%d body=%s", status, raw)
	}
}

func TestShowTaskReturnsItsEventsWithinItsProject(t *testing.T) {
	server, web, project := taskServer(t)
	other, _, err := server.Store.Init(repository(t))
	if err != nil {
		t.Fatal(err)
	}
	created := newTask(t, web, project.ID, "k1", "first")

	status, envelope, raw := call(t, web, "GET", "/v1/tasks/"+created.ID+"?project_id="+project.ID, nil, "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	shown := decodeTask(t, envelope)
	if shown.ID != created.ID || len(shown.Events) != 1 || shown.Events[0].Event != string(task.Created) ||
		shown.Events[0].To != string(task.New) {
		t.Fatalf("shown=%+v", shown)
	}

	for _, path := range []string{
		"/v1/tasks/" + created.ID + "?project_id=" + other.ID,
		"/v1/tasks/missing?project_id=" + project.ID,
	} {
		status, envelope, raw := call(t, web, "GET", path, nil, "")
		if status != http.StatusNotFound || envelope.Error == nil || envelope.Error.Code != CodeNotFound {
			t.Fatalf("%s: status=%d body=%s", path, status, raw)
		}
	}
}

func TestCancelTaskIsIdempotentAndFinal(t *testing.T) {
	_, web, project := taskServer(t)
	created := newTask(t, web, project.ID, "k1", "first")
	request := body(t, map[string]string{"project_id": project.ID})
	path := "/v1/tasks/" + created.ID + "/cancel"

	status, envelope, first := call(t, web, "POST", path, map[string]string{"Idempotency-Key": "k2"}, request)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, first)
	}
	if cancelled := decodeTask(t, envelope); cancelled.State != string(task.Cancelled) || cancelled.Reason != reasonCancelled {
		t.Fatalf("cancelled=%+v", cancelled)
	}
	status, _, second := call(t, web, "POST", path, map[string]string{"Idempotency-Key": "k2"}, request)
	if status != http.StatusOK || !bytes.Equal(first, second) {
		t.Fatalf("replay: status=%d body=%s", status, second)
	}
	status, envelope, raw := call(t, web, "POST", path, map[string]string{"Idempotency-Key": "k3"}, request)
	if status != http.StatusConflict || envelope.Error == nil || envelope.Error.Code != CodeConflict {
		t.Fatalf("second cancel: status=%d body=%s", status, raw)
	}
	status, _, raw = call(t, web, "POST", path, nil, request)
	if status != http.StatusBadRequest {
		t.Fatalf("cancel without a key: status=%d body=%s", status, raw)
	}
	status, _, raw = call(t, web, "POST", "/v1/tasks/"+created.ID+"/resume", map[string]string{"Idempotency-Key": "k4"}, request)
	if status != http.StatusConflict {
		t.Fatalf("resume after cancel: status=%d body=%s", status, raw)
	}
}

func TestResumeTaskFollowsItsStoredContinuation(t *testing.T) {
	server, web, project := taskServer(t)
	created := newTask(t, web, project.ID, "k1", "first")
	request := body(t, map[string]string{"project_id": project.ID})
	path := "/v1/tasks/" + created.ID + "/resume"

	status, envelope, raw := call(t, web, "POST", path, map[string]string{"Idempotency-Key": "k2"}, request)
	if status != http.StatusConflict || envelope.Error == nil || envelope.Error.Code != CodeConflict {
		t.Fatalf("resume of a task that is not blocked: status=%d body=%s", status, raw)
	}

	if _, err := server.Tasks.Transition(created.ID, task.Input{Event: task.Block, Reason: "turn timeout"}); err != nil {
		t.Fatal(err)
	}
	status, envelope, raw = call(t, web, "POST", path, map[string]string{"Idempotency-Key": "k3"}, request)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if resumed := decodeTask(t, envelope); resumed.State != string(task.New) || resumed.ResumeState != "" || resumed.BlockedReason != "" {
		t.Fatalf("resumed=%+v", resumed)
	}

	// A continuation that is not a state to resume in is refused.
	if _, err := server.Tasks.Transition(created.ID, task.Input{Event: task.Block, Reason: "turn timeout"}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.DB.Exec("UPDATE tasks SET resume_state = 'DONE' WHERE task_id = ?", created.ID); err != nil {
		t.Fatal(err)
	}
	status, envelope, raw = call(t, web, "POST", path, map[string]string{"Idempotency-Key": "k4"}, request)
	if status != http.StatusConflict || envelope.Error == nil || !strings.Contains(envelope.Error.Message, "continuation") {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	stored, err := server.Tasks.Get(created.ID)
	if err != nil || stored.State != task.Blocked {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}

	status, _, raw = call(t, web, "POST", path, map[string]string{"Idempotency-Key": "k5"}, "{}")
	if status != http.StatusBadRequest {
		t.Fatalf("resume without a project: status=%d body=%s", status, raw)
	}
}
