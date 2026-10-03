// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	handler := Health("maestro-svc", "0.0.0")
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/healthz", http.StatusOK}, {"POST", "/healthz", http.StatusMethodNotAllowed}, {"GET", "/resources", http.StatusNotFound},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if recorder.Code != tc.status {
			t.Fatalf("%s %s: %d", tc.method, tc.path, recorder.Code)
		}
		if recorder.Code == http.StatusOK {
			var body map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["service"] != "maestro-svc" || body["version"] != "0.0.0" || body["status"] != "ok" {
				t.Fatal(body)
			}
		}
	}
}

func TestRunFlags(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"--help"}, {"--unknown"}, {"extra"}} {
		var output bytes.Buffer
		err := Run(context.Background(), args, &output, "maestro-svc", "0.0.0", filepath.Join(t.TempDir(), "svc.sock"), Health("maestro-svc", "0.0.0"))
		valid := args[0] == "--version" || args[0] == "--help"
		if (err == nil) != valid {
			t.Fatalf("%v: %v", args, err)
		}
		if args[0] == "--version" && !strings.Contains(output.String(), "maestro-svc 0.0.0") {
			t.Fatal(output.String())
		}
	}
}

func TestServeCancellationDrainsActiveRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	finished := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	go func() { finished <- Serve(ctx, listener, handler) }()
	client := &http.Client{Timeout: 2 * time.Second}
	response := make(chan error, 1)
	go func() {
		result, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			err = result.Body.Close()
		}
		response <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-finished:
		t.Fatalf("server stopped before draining: %v", err)
	default:
	}
	release <- struct{}{}
	select {
	case err := <-response:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}
