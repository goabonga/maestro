// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/worker"
	"github.com/goabonga/maestro/internal/worktree"
)

func shortDir(t *testing.T) string {
	// Unix socket paths are limited to 108 bytes, so avoid long test temp names.
	dir, err := os.MkdirTemp("", "maestro-svc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestRunVersion(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "maestro-svc "+Version+"\n" {
		t.Fatalf("output %q", output.String())
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"--unknown"}, &output); err == nil {
		t.Fatal("unknown flag was accepted")
	}
}

func TestRunRejectsExtraArgument(t *testing.T) {
	var output bytes.Buffer
	err := run(context.Background(), []string{"extra"}, &output)
	if err == nil || !strings.Contains(err.Error(), "unexpected argument: extra") {
		t.Fatalf("error %v", err)
	}
}

func TestRunServesHealthOnSocketAndStopsOnCancel(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", shortDir(t))
	socket := filepath.Join(shortDir(t), "svc.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	var output bytes.Buffer
	go func() { finished <- run(ctx, []string{"--socket", socket}, &output) }()

	waitForSocket(t, socket, finished)
	request, err := http.NewRequest(http.MethodGet, "http://maestro/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.Client(socket).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || body["service"] != "maestro-svc" || body["status"] != "ok" {
		t.Fatalf("%d %v", response.StatusCode, body)
	}

	// The versioned API answers on the same socket with its envelope.
	listing, err := http.NewRequest(http.MethodGet, "http://maestro/v1/projects", nil)
	if err != nil {
		t.Fatal(err)
	}
	listing.Header.Set("X-Request-Id", "req-1")
	answer, err := transport.Client(socket).Do(listing)
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	var envelope map[string]any
	if err := json.NewDecoder(answer.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if answer.StatusCode != http.StatusOK || envelope["request_id"] != "req-1" {
		t.Fatalf("%d %v", answer.StatusCode, envelope)
	}

	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("daemon stopped with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not stop after cancellation")
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestRunRefusesSocketOfLiveDaemon(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", shortDir(t))
	socket := filepath.Join(shortDir(t), "svc.sock")
	live, err := transport.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	var output bytes.Buffer
	err = run(context.Background(), []string{"--socket", socket}, &output)
	if err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("error %v", err)
	}
}

func waitForSocket(t *testing.T, socket string, finished <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", socket); err == nil {
			_ = conn.Close()
			return
		}
		select {
		case err := <-finished:
			t.Fatalf("daemon exited before listening: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("daemon did not start listening")
}

func TestRunRefusesASecondDaemonForTheSameUser(t *testing.T) {
	data := shortDir(t)
	t.Setenv("MAESTRO_DATA_HOME", data)
	socket := filepath.Join(shortDir(t), "svc.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	var output bytes.Buffer
	go func() { finished <- run(ctx, []string{"--socket", socket}, &output) }()
	waitForSocket(t, socket, finished)

	// A different socket does not allow a second daemon.
	other := filepath.Join(shortDir(t), "other.sock")
	var second bytes.Buffer
	err := run(context.Background(), []string{"--socket", other}, &second)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("error %v", err)
	}

	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("daemon stopped with %v", err)
	}
}

func TestRunStopsOnTheStopRoute(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", shortDir(t))
	socket := filepath.Join(shortDir(t), "svc.sock")
	finished := make(chan error, 1)
	var output bytes.Buffer
	go func() { finished <- run(context.Background(), []string{"--socket", socket}, &output) }()
	waitForSocket(t, socket, finished)

	request, err := http.NewRequest(http.MethodPost, "http://maestro/v1/daemon/stop", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.Client(socket).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("daemon stopped with %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not stop after the stop route")
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestRunValidatesCapacityCeilings(t *testing.T) {
	var output bytes.Buffer
	err := run(context.Background(), []string{"--max-sessions", "0"}, &output)
	if err == nil || !strings.Contains(err.Error(), "at least 1") {
		t.Fatalf("error %v", err)
	}
}

func TestRunServesTasks(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", shortDir(t))
	socket := filepath.Join(shortDir(t), "svc.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	var output bytes.Buffer
	go func() { finished <- run(ctx, []string{"--socket", socket}, &output) }()
	waitForSocket(t, socket, finished)

	// The task routes are served: an unknown project is reported as
	// such, not as a daemon without tasks.
	request, err := http.NewRequest(http.MethodGet, "http://maestro/v1/tasks?project_id=missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.Client(socket).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound || envelope.Error.Message != "unknown project: missing" {
		t.Fatalf("%d %+v", response.StatusCode, envelope)
	}

	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("daemon stopped with %v", err)
	}
}

func TestRunServesSyncs(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", shortDir(t))
	socket := filepath.Join(shortDir(t), "svc.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	var output bytes.Buffer
	go func() { finished <- run(ctx, []string{"--socket", socket}, &output) }()
	waitForSocket(t, socket, finished)

	// The sync routes are served: an unknown project is reported as
	// such, not as a daemon that does not sync.
	request, err := http.NewRequest(http.MethodGet, "http://maestro/v1/syncs/any?project_id=missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.Client(socket).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound || envelope.Error.Message != "unknown project: missing" {
		t.Fatalf("%d %+v", response.StatusCode, envelope)
	}

	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("daemon stopped with %v", err)
	}
}

func TestRunServesPublication(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", shortDir(t))
	socket := filepath.Join(shortDir(t), "svc.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	var output bytes.Buffer
	go func() { finished <- run(ctx, []string{"--socket", socket}, &output) }()
	waitForSocket(t, socket, finished)

	// The publication route is served: an unknown project is reported
	// as such, not as a daemon that does not publish.
	request, err := http.NewRequest(http.MethodPost, "http://maestro/v1/projects/missing/publish", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Idempotency-Key", "publish-1")
	response, err := transport.Client(socket).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound || envelope.Error.Message != "unknown project: missing" {
		t.Fatalf("%d %+v", response.StatusCode, envelope)
	}

	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("daemon stopped with %v", err)
	}
}

func TestRunServesWorkers(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", shortDir(t))
	socket := filepath.Join(shortDir(t), "svc.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	var output bytes.Buffer
	go func() { finished <- run(ctx, []string{"--socket", socket}, &output) }()
	waitForSocket(t, socket, finished)

	// The worker routes are served: an unknown project is reported as
	// such, not as a daemon without workers.
	request, err := http.NewRequest(http.MethodGet, "http://maestro/v1/workers?project_id=missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.Client(socket).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound || envelope.Error.Message != "unknown project: missing" {
		t.Fatalf("%d %+v", response.StatusCode, envelope)
	}

	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("daemon stopped with %v", err)
	}
}

func TestRunReconcilesTheRecordedWorkersBeforeServing(t *testing.T) {
	base := shortDir(t)
	t.Setenv("MAESTRO_DATA_HOME", base)
	project := worktree.Project{ID: "project-1", Dir: filepath.Join(base, "projects", "project-1")}
	if err := os.MkdirAll(project.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project.Dir, "project.json"), []byte(`{"id":"project-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(filepath.Join(base, "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	workers := worker.Store{DB: db}
	for _, name := range []string{"lost", "survived"} {
		if _, err := workers.Register(project, worker.Spec{
			Name: name, Agent: "claude", AgentKind: "claude-code", Driver: "claude-code-2.1",
		}); err != nil {
			t.Fatal(err)
		}
		for _, in := range []worker.Input{
			{Event: worker.Start, Guard: worker.Guard{CapacityReserved: true}},
			{Event: worker.Ready, Guard: worker.Guard{SessionReady: true, ProfileConfirmed: true}},
		} {
			if _, err := workers.Transition(project.ID, name, in); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = db.Close()
	// A fixture process still works in the repository of one worker.
	repository := project.WorkerRepository("survived")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	survivor := exec.Command("sleep", "30")
	survivor.Dir = repository
	if err := survivor.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = survivor.Process.Kill()
		_ = survivor.Wait()
	}()

	socket := filepath.Join(shortDir(t), "svc.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	var output bytes.Buffer
	go func() { finished <- run(ctx, []string{"--socket", socket}, &output) }()
	waitForSocket(t, socket, finished)
	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("daemon stopped with %v", err)
	}

	db, err = state.Open(filepath.Join(base, "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	workers = worker.Store{DB: db}
	lost, err := workers.Get(project.ID, "lost")
	if err != nil {
		t.Fatal(err)
	}
	survived, err := workers.Get(project.ID, "survived")
	if err != nil {
		t.Fatal(err)
	}
	if lost.State != worker.Stopped || survived.State != worker.Failed {
		t.Fatalf("lost %s, survived %s", lost.State, survived.State)
	}
	pid := strconv.Itoa(survivor.Process.Pid)
	if !strings.Contains(survived.Reason, pid) {
		t.Fatalf("reason %q does not name survivor %s", survived.Reason, pid)
	}
	report := output.String()
	if !strings.Contains(report, "worker project-1/lost: IDLE -> STOPPED") ||
		!strings.Contains(report, "worker project-1/survived: IDLE -> FAILED") {
		t.Fatalf("report %q", report)
	}
}
