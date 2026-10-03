// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package transport

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestListenCreatesOwnerOnlySocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "svc.sock")
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
}

func TestListenRefusesLiveDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.sock")
	live, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := Listen(path); err == nil {
		t.Fatal("second daemon replaced a live socket")
	}
}

func TestListenRefusesRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.sock")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path); err == nil {
		t.Fatal("regular file was replaced")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "data" {
		t.Fatalf("regular file changed: %q %v", data, err)
	}
}

func TestClientReachesHealthOverSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.sock")
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() { _ = http.Serve(listener, Health("maestro-svc", "0.0.0")) }()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://maestro/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := Client(path).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || body["service"] != "maestro-svc" {
		t.Fatalf("%d %v", response.StatusCode, body)
	}
}

func TestDefaultSocketUsesRuntimeDirectory(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := DefaultSocket(); got != "/run/user/1000/maestro/svc.sock" {
		t.Fatal(got)
	}
}

func TestListenRejectsOverlongPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), string(make([]byte, 120)))
	if _, err := Listen(path); err == nil {
		t.Fatal("overlong socket path was accepted")
	}
}
