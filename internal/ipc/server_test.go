// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/worktree"
)

// repository creates a Git repository with one commit.
func repository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.test"},
		{"config", "commit.gpgsign", "false"},
		{"commit", "-q", "--allow-empty", "-m", "feat: initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	return dir
}

// newServer builds a server on a fresh store and migrated database.
func newServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	base := t.TempDir()
	db, err := state.Open(filepath.Join(base, "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	server := &Server{DB: db, Store: worktree.Store{Base: base}, Service: "maestro-svc", Version: "0.0.0"}
	web := httptest.NewServer(server.Handler())
	t.Cleanup(web.Close)
	return server, web
}

// call sends one request and decodes the envelope.
func call(t *testing.T, web *httptest.Server, method, path string, headers map[string]string, body string) (int, Envelope, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, web.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := web.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("not an envelope: %s", raw)
	}
	return response.StatusCode, envelope, raw
}

func TestProjectsRoutesListAndRegister(t *testing.T) {
	_, web := newServer(t)
	status, envelope, _ := call(t, web, "GET", "/v1/projects", map[string]string{"X-Request-Id": "req-1"}, "")
	if status != http.StatusOK || envelope.RequestID != "req-1" || envelope.Error != nil {
		t.Fatalf("status=%d envelope=%+v", status, envelope)
	}

	repo := repository(t)
	body, err := json.Marshal(map[string]string{"path": repo})
	if err != nil {
		t.Fatal(err)
	}
	status, envelope, _ = call(t, web, "POST", "/v1/projects",
		map[string]string{"Idempotency-Key": "key-1"}, string(body))
	if status != http.StatusCreated || envelope.Error != nil {
		t.Fatalf("status=%d envelope=%+v", status, envelope)
	}

	status, envelope, _ = call(t, web, "GET", "/v1/projects", nil, "")
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	listed, err := json.Marshal(envelope.Data)
	if err != nil || !bytes.Contains(listed, []byte(repo)) {
		t.Fatalf("listing misses the project: %s (err=%v)", listed, err)
	}
}

func TestRegisterReplaysTheSameIdempotencyKey(t *testing.T) {
	_, web := newServer(t)
	repo := repository(t)
	body, err := json.Marshal(map[string]string{"path": repo})
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Idempotency-Key": "key-1", "X-Request-Id": "req-1"}
	status, _, first := call(t, web, "POST", "/v1/projects", headers, string(body))
	if status != http.StatusCreated {
		t.Fatalf("status=%d", status)
	}
	// A plain re-run would answer 200 (already registered); the replay
	// answers 201 with the stored body, proving the handler did not run.
	status, _, second := call(t, web, "POST", "/v1/projects", headers, string(body))
	if status != http.StatusCreated || !bytes.Equal(first, second) {
		t.Fatalf("status=%d first=%s second=%s", status, first, second)
	}
}

func TestIdempotencyKeyConflictsAndRequirements(t *testing.T) {
	server, web := newServer(t)
	repo := repository(t)
	body, err := json.Marshal(map[string]string{"path": repo})
	if err != nil {
		t.Fatal(err)
	}

	status, envelope, _ := call(t, web, "POST", "/v1/projects", nil, string(body))
	if status != http.StatusBadRequest || envelope.Error == nil || envelope.Error.Code != CodeInvalidRequest {
		t.Fatalf("status=%d envelope=%+v", status, envelope)
	}

	headers := map[string]string{"Idempotency-Key": "key-1"}
	if status, _, _ := call(t, web, "POST", "/v1/projects", headers, string(body)); status != http.StatusCreated {
		t.Fatalf("status=%d", status)
	}
	status, envelope, _ = call(t, web, "POST", "/v1/projects", headers, `{"path": "/elsewhere"}`)
	if status != http.StatusConflict || envelope.Error == nil || envelope.Error.Code != CodeConflict {
		t.Fatalf("status=%d envelope=%+v", status, envelope)
	}

	// A claim without a stored response is still running.
	if _, err := server.DB.Exec("INSERT INTO idempotency_keys (key, request_hash, created_at) VALUES ('key-2', ?, 'now')",
		requestHashForTest("POST", "/v1/projects", body)); err != nil {
		t.Fatal(err)
	}
	status, envelope, _ = call(t, web, "POST", "/v1/projects", map[string]string{"Idempotency-Key": "key-2"}, string(body))
	if status != http.StatusConflict || envelope.Error == nil || envelope.Error.Code != CodeInProgress {
		t.Fatalf("status=%d envelope=%+v", status, envelope)
	}
}

func TestInvalidBodiesAndUnknownRoutes(t *testing.T) {
	_, web := newServer(t)
	headers := map[string]string{"Idempotency-Key": "key-1"}

	status, envelope, _ := call(t, web, "POST", "/v1/projects", headers, `{"path": ""}`)
	if status != http.StatusBadRequest || envelope.Error == nil {
		t.Fatalf("status=%d envelope=%+v", status, envelope)
	}

	status, envelope, _ = call(t, web, "POST", "/v1/projects", map[string]string{"Idempotency-Key": "key-2"},
		`{"path": "`+t.TempDir()+`"}`)
	if status != http.StatusBadRequest || envelope.Error == nil || envelope.Error.Code != CodeInvalidRequest {
		t.Fatalf("status=%d envelope=%+v", status, envelope)
	}

	status, envelope, _ = call(t, web, "GET", "/v1/unknown", nil, "")
	if status != http.StatusNotFound || envelope.Error == nil || envelope.Error.Code != CodeNotFound {
		t.Fatalf("status=%d envelope=%+v", status, envelope)
	}

	huge := strings.Repeat("x", maxBody+1)
	status, envelope, _ = call(t, web, "POST", "/v1/projects", map[string]string{"Idempotency-Key": "key-3"}, huge)
	if status != http.StatusRequestEntityTooLarge || envelope.Error == nil {
		t.Fatalf("status=%d envelope=%+v", status, envelope)
	}
}

// requestHashForTest mirrors the middleware's request hash.
func requestHashForTest(method, path string, body []byte) string {
	sum := sha256.Sum256(append([]byte(method+" "+path+"\n"), body...))
	return hex.EncodeToString(sum[:])
}
