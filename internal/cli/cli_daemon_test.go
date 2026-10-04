// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce   sync.Once
	builtDaemon string
	buildErr    error
)

// daemonBinary builds maestro-svc once for the lifecycle tests.
func daemonBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "maestro-cli")
		if err != nil {
			buildErr = err
			return
		}
		builtDaemon = filepath.Join(dir, "maestro-svc")
		command := exec.Command("go", "build", "-o", builtDaemon, "../../cmd/svc")
		if output, err := command.CombinedOutput(); err != nil {
			buildErr = err
			builtDaemon = string(output)
		}
	})
	if buildErr != nil {
		t.Fatalf("building maestro-svc: %v: %s", buildErr, builtDaemon)
	}
	return builtDaemon
}

// shortSocket returns a socket path short enough for sockaddr_un.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "maestro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "svc.sock")
}

func TestDaemonLifecycle(t *testing.T) {
	data, err := os.MkdirTemp("", "maestro-data")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(data) })
	t.Setenv("MAESTRO_DATA_HOME", data)
	binary := daemonBinary(t)
	socket := shortSocket(t)
	ctx := context.Background()

	var output bytes.Buffer
	if err := Run(ctx, []string{"daemon", "status", "--socket", socket}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "not running") {
		t.Fatalf("unexpected status: %q", output.String())
	}

	output.Reset()
	if err := Run(ctx, []string{"daemon", "start", "--socket", socket, "--binary", binary}, &output, "0.0.0"); err != nil {
		t.Fatalf("start: %v: %s", err, output.String())
	}
	if !strings.Contains(output.String(), "daemon running: maestro-svc") {
		t.Fatalf("unexpected start output: %q", output.String())
	}
	t.Cleanup(func() {
		_ = Run(ctx, []string{"daemon", "stop", "--socket", socket}, io.Discard, "0.0.0")
	})

	// Starting again is a friendly no-op.
	output.Reset()
	if err := Run(ctx, []string{"daemon", "start", "--socket", socket, "--binary", binary}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "already running") {
		t.Fatalf("unexpected output: %q", output.String())
	}

	output.Reset()
	if err := Run(ctx, []string{"daemon", "status", "--socket", socket}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "daemon running: maestro-svc") {
		t.Fatalf("unexpected status: %q", output.String())
	}

	output.Reset()
	if err := Run(ctx, []string{"daemon", "stop", "--socket", socket}, &output, "0.0.0"); err != nil {
		t.Fatalf("stop: %v: %s", err, output.String())
	}
	if !strings.Contains(output.String(), "daemon stopped") {
		t.Fatalf("unexpected stop output: %q", output.String())
	}

	// Stopping a stopped daemon is a no-op, and the socket is gone.
	output.Reset()
	if err := Run(ctx, []string{"daemon", "stop", "--socket", socket}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "not running") {
		t.Fatalf("unexpected output: %q", output.String())
	}
}

func TestDaemonCommandErrors(t *testing.T) {
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"daemon"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "usage: maestro daemon") {
		t.Fatalf("error %v", err)
	}
	if err := Run(context.Background(), []string{"daemon", "reload"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "unknown daemon command") {
		t.Fatalf("error %v", err)
	}
	socket := shortSocket(t)
	err := Run(context.Background(), []string{"daemon", "start", "--socket", socket, "--binary", "/nonexistent/maestro-svc"}, &output, "0.0.0")
	if err == nil {
		t.Fatal("start with a missing binary succeeded")
	}
}
