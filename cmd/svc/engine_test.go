// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/transport"
)

// silentAgent answers its version like Claude Code, records its chosen
// or resumed session, then ends every pasted prompt like Claude Code
// ending a turn, without ever writing a handoff document.
const silentAgent = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$2" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$2.jsonl"
echo READY
while IFS= read -r line; do
	case "$line" in
	*201~*) printf '\342\227\217 nothing to hand off\n\342\234\273 Baked for 1s \302\267 done (fixture)\n\342\235\257 \n' ;;
	esac
done
`

// gitRepository creates a repository with one commit, without the
// caller's Git environment or configuration.
func gitRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"},
		{"config", "commit.gpgsign", "false"}, {"commit", "-q", "--allow-empty", "-m", "feat: initial"},
	} {
		command := exec.Command("git", args...)
		command.Dir = dir
		for _, entry := range os.Environ() {
			if name, _, _ := strings.Cut(entry, "="); !strings.HasPrefix(name, "GIT_") {
				command.Env = append(command.Env, entry)
			}
		}
		command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	return dir
}

// call sends one JSON request to the daemon, decodes the data of its
// answer and returns its status with its error message, if any.
func call(t *testing.T, socket, method, path, body string, data any) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, "http://maestro"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		request.Header.Set("Idempotency-Key", path+body)
	}
	response, err := transport.Client(socket).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	envelope := struct {
		Data  any `json:"data"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}{Data: data}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, envelope.Error.Message
}

func TestRunDrivesTheTasksOnTheStartedWorkers(t *testing.T) {
	if _, err := launcher.New(); errors.Is(err, launcher.ErrUnsupported) {
		t.Skip(err)
	}
	bin := shortDir(t)
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(silentAgent), 0o700); err != nil { // #nosec G306 -- an executable fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MAESTRO_DATA_HOME", shortDir(t))
	socket := filepath.Join(shortDir(t), "svc.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	var output bytes.Buffer
	go func() { finished <- run(ctx, []string{"--socket", socket}, &output) }()
	waitForSocket(t, socket, finished)

	var project struct {
		ID string `json:"project_id"`
	}
	path, err := json.Marshal(map[string]string{"path": gitRepository(t)})
	if err != nil {
		t.Fatal(err)
	}
	if status, problem := call(t, socket, http.MethodPost, "/v1/projects", string(path), &project); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, problem)
	}
	// The task is created before any worker runs: it stays NEW until
	// the worker that starts afterwards becomes IDLE.
	var created struct {
		ID string `json:"task_id"`
	}
	if status, problem := call(t, socket, http.MethodPost, "/v1/tasks",
		`{"project_id": "`+project.ID+`", "description": "add a flag file"}`, &created); status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, problem)
	}
	var started []json.RawMessage
	if status, problem := call(t, socket, http.MethodPost, "/v1/workers",
		`{"project_id": "`+project.ID+`", "agent": "claude-code", "count": 1}`, &started); status != http.StatusAccepted {
		t.Fatalf("start: %d %s", status, problem)
	}

	// The worker plans the task without a handoff, then fails its
	// single format repair: the task is assigned, then blocked.
	blocked := func(times int) []string {
		t.Helper()
		var shown struct {
			State         string `json:"state"`
			BlockedReason string `json:"blocked_reason"`
			Events        []struct {
				Event string `json:"event"`
				To    string `json:"to"`
			} `json:"events"`
		}
		// Well within the sweep interval: only the request or the
		// worker that became IDLE can have driven the task.
		deadline := time.Now().Add(30 * time.Second)
		for {
			call(t, socket, http.MethodGet, "/v1/tasks/"+created.ID+"?project_id="+project.ID, "", &shown)
			var steps []string
			count := 0
			for _, event := range shown.Events {
				steps = append(steps, event.Event+">"+event.To)
				if event.To == "BLOCKED" {
					count++
				}
			}
			if shown.State == "BLOCKED" && count == times {
				if !strings.Contains(shown.BlockedReason, "format repair failed") {
					t.Fatalf("blocked with %q", shown.BlockedReason)
				}
				return steps
			}
			if time.Now().After(deadline) {
				t.Fatalf("the task is %s after %v\n%s", shown.State, steps, output.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// The worker plans the task without a handoff, then fails its
	// single format repair: the task is assigned, then blocked.
	if steps := blocked(1); !slices.Contains(steps, "assign>PLANNING") {
		t.Fatalf("events %v", steps)
	}
	// A resumed task is driven again on the worker released by the
	// rejected turn.
	var resumed json.RawMessage
	if status, problem := call(t, socket, http.MethodPost, "/v1/tasks/"+created.ID+"/resume",
		`{"project_id": "`+project.ID+`"}`, &resumed); status != http.StatusOK {
		t.Fatalf("resume: %d %s", status, problem)
	}
	blocked(2)

	cancel()
	if err := <-finished; err != nil {
		t.Fatalf("daemon stopped with %v", err)
	}
}
