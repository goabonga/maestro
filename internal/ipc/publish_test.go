// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/integration"
	"github.com/goabonga/maestro/internal/worktree"
)

// publishServer builds a server that publishes, with one registered
// project.
func publishServer(t *testing.T) (*Server, *httptest.Server, worktree.Project) {
	t.Helper()
	server, web := newServer(t)
	server.Operations = &integration.Store{DB: server.DB}
	project, _, err := server.Store.Init(repository(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.DB.Exec("INSERT INTO config_snapshots (config_id, created_at, document) VALUES ('sha256-x', 'now', '{}')"); err != nil {
		t.Fatal(err)
	}
	return server, web, project
}

// bareGit runs git against a bare repository and returns its output.
func bareGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_DIR=" + repository, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test",
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// advanceIntegration commits on the canonical integration branch and
// journals the move as a committed, tested SYNC operation.
func advanceIntegration(t *testing.T, store *integration.Store, project worktree.Project) string {
	t.Helper()
	repository := project.Repository()
	base := bareGit(t, repository, "rev-parse", "refs/heads/maestro/integration")
	tree := bareGit(t, repository, "rev-parse", base+"^{tree}")
	head := bareGit(t, repository, "commit-tree", tree, "-p", base, "-m", "feat: synced")
	bareGit(t, repository, "update-ref", "refs/heads/maestro/integration", head, base)
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

// decodePublication reads the publication view of an envelope.
func decodePublication(t *testing.T, envelope Envelope) publicationView {
	t.Helper()
	raw, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var view publicationView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("not a publication: %s", raw)
	}
	return view
}

func TestPublishRoutePublishesOncePerKey(t *testing.T) {
	server, web, project := publishServer(t)
	head := advanceIntegration(t, server.Operations, project)
	path := "/v1/projects/" + project.ID + "/publish"

	status, envelope, raw := call(t, web, "POST", path, map[string]string{"Idempotency-Key": "k1"}, "")
	view := decodePublication(t, envelope)
	if status != http.StatusOK || view.Published != head || view.Previous != "" || view.UpToDate ||
		view.Reference != "refs/heads/maestro/integration" || view.State != "COMMITTED" || view.OperationID == "" {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if published := bareGit(t, project.UserRepository, "rev-parse", "refs/heads/maestro/integration"); published != head {
		t.Fatalf("user branch at %s", published)
	}

	status, _, replay := call(t, web, "POST", path, map[string]string{"Idempotency-Key": "k1"}, "")
	if status != http.StatusOK || string(replay) != string(raw) {
		t.Fatalf("the replay differs: %s", replay)
	}
	status, envelope, raw = call(t, web, "POST", path, map[string]string{"Idempotency-Key": "k2"}, "")
	if view := decodePublication(t, envelope); status != http.StatusOK || !view.UpToDate || view.OperationID != "" {
		t.Fatalf("status=%d body=%s", status, raw)
	}
}

func TestPublishRouteReportsRefusals(t *testing.T) {
	server, web, project := publishServer(t)
	status, envelope, raw := call(t, web, "POST", "/v1/projects/"+project.ID+"/publish", map[string]string{"Idempotency-Key": "k1"}, "")
	if status != http.StatusConflict || envelope.Error == nil || envelope.Error.Code != CodeConflict ||
		!strings.Contains(envelope.Error.Message, "not the result of a committed operation") {
		t.Fatalf("an untested head: status=%d body=%s", status, raw)
	}
	status, envelope, raw = call(t, web, "POST", "/v1/projects/unknown/publish", map[string]string{"Idempotency-Key": "k2"}, "")
	if status != http.StatusNotFound || envelope.Error == nil || envelope.Error.Code != CodeNotFound {
		t.Fatalf("an unknown project: status=%d body=%s", status, raw)
	}
	server.Operations = nil
	status, _, raw = call(t, web, "POST", "/v1/projects/"+project.ID+"/publish", map[string]string{"Idempotency-Key": "k3"}, "")
	if status != http.StatusNotFound {
		t.Fatalf("without operations: status=%d body=%s", status, raw)
	}
}
