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

	"github.com/goabonga/maestro/internal/transport"
)

func TestStatusReadsDaemonHealthOverSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.sock")
	listener, err := transport.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() { _ = http.Serve(listener, transport.Health("maestro-svc", "0.0.0")) }()
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"status", "--socket", path}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"service":"maestro-svc"`) {
		t.Fatal(output.String())
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
