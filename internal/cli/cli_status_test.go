// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/scheduler"
	"github.com/goabonga/maestro/internal/transport"
)

func TestStatusReportsDaemonAndCapacityOverSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.sock")
	listener, err := transport.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	capacity, err := scheduler.NewCapacity(3, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capacity.Reserve(scheduler.Sessions, "p1"); err != nil {
		t.Fatal(err)
	}
	server := &ipc.Server{Service: "maestro-svc", Version: "0.0.0", Capacity: capacity}
	go func() { _ = http.Serve(listener, server.Handler()) }()

	var output bytes.Buffer
	if err := Run(context.Background(), []string{"status", "--socket", path}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	report := output.String()
	for _, want := range []string{"daemon: maestro-svc 0.0.0", "CAPACITY", "sessions", "p1: 1", "tests"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report misses %q: %s", want, report)
		}
	}
}

func TestStatusReportsMissingDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.sock")
	var output bytes.Buffer
	err := Run(context.Background(), []string{"status", "--socket", path}, &output, "0.0.0")
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("error %v", err)
	}
}
