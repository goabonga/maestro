// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/transport"
)

func shortDir(t *testing.T) string {
	// Unix socket paths are limited to 108 bytes, so avoid long test temp names.
	dir, err := os.MkdirTemp("", "maestro-cli")
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
	if output.String() != "maestro "+Version+"\n" {
		t.Fatalf("output %q", output.String())
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var output bytes.Buffer
	err := run(context.Background(), []string{"create"}, &output)
	if err == nil || err.Error() != "unknown command: create" {
		t.Fatalf("error %v", err)
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"--unknown"}, &output); err == nil {
		t.Fatal("unknown flag was accepted")
	}
}

func TestRunWithoutArgumentsPrintsUsage(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), nil, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Usage of maestro:") {
		t.Fatalf("output %q", output.String())
	}
}

func TestStatusReadsDaemonHealth(t *testing.T) {
	socket := filepath.Join(shortDir(t), "svc.sock")
	listener, err := transport.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := &ipc.Server{Service: "maestro-svc", Version: Version}
	go func() { _ = http.Serve(listener, server.Handler()) }()

	var output bytes.Buffer
	if err := run(context.Background(), []string{"status", "--socket", socket}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "daemon: maestro-svc "+Version) {
		t.Fatalf("output %q", output.String())
	}
}

func TestStatusFailsWithoutDaemon(t *testing.T) {
	var output bytes.Buffer
	err := run(context.Background(), []string{"status", "--socket", filepath.Join(shortDir(t), "absent.sock")}, &output)
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("error %v", err)
	}
}
