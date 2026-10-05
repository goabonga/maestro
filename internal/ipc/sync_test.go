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
	"time"

	"github.com/goabonga/maestro/internal/integration"
	"github.com/goabonga/maestro/internal/scheduler"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/testrun"
	"github.com/goabonga/maestro/internal/worktree"
)

// passingTester is a fixture test runner reporting every command as
// passed on the revision it is given.
type passingTester struct{}

func (passingTester) Run(_, sha, clone string, commands []testrun.Command) (testrun.Run, error) {
	run := testrun.Run{TestedSHA: sha, Clone: clone}
	for _, command := range commands {
		run.Results = append(run.Results, testrun.Result{Name: command.Name, Argv: command.Argv, TestedSHA: sha})
	}
	return run, nil
}

// userGit runs git in the user repository and returns its trimmed
// output.
func userGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// commitFile writes and commits one file in the user repository.
func commitFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	userGit(t, dir, "add", name)
	userGit(t, dir, "commit", "-q", "-m", "feat: "+name)
	return userGit(t, dir, "rev-parse", "HEAD")
}

// syncServer builds a server serving tasks and syncs with one
// registered project whose user repository configures a test command.
func syncServer(t *testing.T) (*Server, *httptest.Server, worktree.Project, string) {
	t.Helper()
	server, web := newServer(t)
	server.Tasks = &task.Store{DB: server.DB}
	server.Sync = &integration.Syncer{Store: integration.Store{DB: server.DB}, Tester: passingTester{}}
	t.Cleanup(server.Wait)
	user := repository(t)
	project, _, err := server.Store.Init(user)
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, user, ".maestro.toml", "[tests.unit]\nargv = [\"/bin/true\"]\n")
	return server, web, project, user
}

// decodeSync reads the sync view of an envelope.
func decodeSync(t *testing.T, envelope Envelope) syncView {
	t.Helper()
	raw, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var view syncView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("not a sync: %s", raw)
	}
	return view
}

// awaitSync polls a sync until it reaches a terminal state.
func awaitSync(t *testing.T, web *httptest.Server, projectID, id string) syncView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, envelope, raw := call(t, web, "GET", "/v1/syncs/"+id+"?project_id="+projectID, nil, "")
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, raw)
		}
		view := decodeSync(t, envelope)
		if integration.State(view.State).Terminal() {
			return view
		}
		if time.Now().After(deadline) {
			t.Fatalf("sync %s still %s", id, view.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSyncAdvancesTheIntegrationAndKeepsTaskBases(t *testing.T) {
	server, web, project, user := syncServer(t)
	previous := integrationHeadOf(t, project)
	started := newTask(t, web, project.ID, "task-key", "a task started before the sync")
	synced := userGit(t, user, "rev-parse", "HEAD")

	headers := map[string]string{"Idempotency-Key": "sync-1"}
	request := body(t, map[string]string{"project_id": project.ID, "branch": "main"})
	status, envelope, first := call(t, web, "POST", "/v1/syncs", headers, request)
	if status != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", status, first)
	}
	view := decodeSync(t, envelope)
	if view.Branch != "main" || view.State != string(integration.Prepared) || view.PreviousSHA != previous ||
		view.SyncedSHA != synced || view.ProjectID != project.ID || view.OperationID == "" {
		t.Fatalf("view %+v", view)
	}
	done := awaitSync(t, web, project.ID, view.OperationID)
	if done.State != string(integration.Committed) || done.SyncedSHA != synced || len(done.TestReportIDs) != 1 {
		t.Fatalf("sync %+v", done)
	}
	if head := integrationHeadOf(t, project); head != synced {
		t.Fatalf("integration at %s, want %s", head, synced)
	}

	// A retry replays the answer: no second sync is started.
	status, _, second := call(t, web, "POST", "/v1/syncs", headers, request)
	if status != http.StatusAccepted || !bytes.Equal(first, second) {
		t.Fatalf("status=%d first=%s second=%s", status, first, second)
	}
	if ops, err := server.Sync.Store.List(project.ID); err != nil || len(ops) != 1 {
		t.Fatalf("operations %v %v", ops, err)
	}
	// The running task keeps its base; a new task starts from the new
	// integration.
	kept, err := server.Tasks.Get(started.ID)
	if err != nil || kept.BaseSHA != previous {
		t.Fatalf("task %+v %v", kept, err)
	}
	if fresh := newTask(t, web, project.ID, "task-key-2", "a task after the sync"); fresh.BaseSHA != synced {
		t.Fatalf("new task base %s, want %s", fresh.BaseSHA, synced)
	}
}

func TestSyncErrors(t *testing.T) {
	server, web, project, user := syncServer(t)
	userGit(t, user, "switch", "-q", "-c", "side", "HEAD~1")
	side := commitFile(t, user, "side.txt", "side\n")
	userGit(t, user, "switch", "-q", "main")
	status, envelope, raw := call(t, web, "POST", "/v1/syncs", map[string]string{"Idempotency-Key": "k0"},
		body(t, map[string]string{"project_id": project.ID, "branch": "main"}))
	if status != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	synced := awaitSync(t, web, project.ID, decodeSync(t, envelope).OperationID)

	cases := []struct {
		name   string
		body   string
		status int
		code   string
		says   []string
	}{
		{"diverged", body(t, map[string]string{"project_id": project.ID, "branch": "side"}), http.StatusConflict, CodeConflict,
			[]string{side, synced.SyncedSHA}},
		{"up to date", body(t, map[string]string{"project_id": project.ID, "branch": "main"}), http.StatusConflict, CodeConflict, nil},
		{"missing branch", body(t, map[string]string{"project_id": project.ID, "branch": "nope"}), http.StatusBadRequest, CodeInvalidRequest, nil},
		{"no branch", body(t, map[string]string{"project_id": project.ID}), http.StatusBadRequest, CodeInvalidRequest, nil},
		{"no project", body(t, map[string]string{"branch": "main"}), http.StatusBadRequest, CodeInvalidRequest, nil},
		{"unknown project", body(t, map[string]string{"project_id": "nope", "branch": "main"}), http.StatusNotFound, CodeNotFound, nil},
		{"unknown field", `{"project_id": "x", "branch": "main", "force": true}`, http.StatusBadRequest, CodeInvalidRequest, nil},
	}
	for i, c := range cases {
		status, envelope, raw := call(t, web, "POST", "/v1/syncs", map[string]string{"Idempotency-Key": "k" + c.name}, c.body)
		if status != c.status || envelope.Error == nil || envelope.Error.Code != c.code {
			t.Fatalf("case %d %s: status=%d body=%s", i, c.name, status, raw)
		}
		for _, want := range c.says {
			if !strings.Contains(envelope.Error.Message, want) {
				t.Fatalf("%s: message misses %s: %s", c.name, want, envelope.Error.Message)
			}
		}
	}
	if ops, err := server.Sync.Store.List(project.ID); err != nil || len(ops) != 1 {
		t.Fatalf("a refused sync journals nothing: %v %v", ops, err)
	}
	for _, path := range []string{"/v1/syncs/nope?project_id=" + project.ID, "/v1/syncs/" + synced.OperationID + "?project_id=other"} {
		if status, envelope, raw := call(t, web, "GET", path, nil, ""); status != http.StatusNotFound || envelope.Error.Code != CodeNotFound {
			t.Fatalf("%s: status=%d body=%s", path, status, raw)
		}
	}
}

func TestSyncNeedsConfiguredTestsAndCapacity(t *testing.T) {
	server, web, project, user := syncServer(t)
	request := body(t, map[string]string{"project_id": project.ID, "branch": "main"})

	capacity, err := scheduler.NewCapacity(1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := capacity.Reserve(scheduler.Tests, "another")
	if err != nil {
		t.Fatal(err)
	}
	server.Capacity = capacity
	status, envelope, raw := call(t, web, "POST", "/v1/syncs", map[string]string{"Idempotency-Key": "full"}, request)
	if status != http.StatusConflict || envelope.Error.Code != CodeConflict || !strings.Contains(envelope.Error.Message, "capacity") {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	slot.Release()

	userGit(t, user, "rm", "-q", ".maestro.toml")
	userGit(t, user, "commit", "-q", "-m", "chore: drop the tests")
	status, envelope, raw = call(t, web, "POST", "/v1/syncs", map[string]string{"Idempotency-Key": "untested"}, request)
	if status != http.StatusConflict || !strings.Contains(envelope.Error.Message, "no test command") {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if usage := capacity.Usage()[scheduler.Tests]; usage.Used != 0 {
		t.Fatalf("a refused sync holds no slot: %+v", usage)
	}

	server.Sync = nil
	status, envelope, raw = call(t, web, "POST", "/v1/syncs", map[string]string{"Idempotency-Key": "disabled"}, request)
	if status != http.StatusNotFound || envelope.Error.Code != CodeNotFound {
		t.Fatalf("status=%d body=%s", status, raw)
	}
}
