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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/transport"
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
