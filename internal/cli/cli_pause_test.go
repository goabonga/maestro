// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/scheduler"
	"github.com/goabonga/maestro/internal/worker"
)

// drawingClaude is claudeFixture drawing its terminal, as a resumed
// agent must before it is ready.
const drawingClaude = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$2" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$2.jsonl"
echo READY
exec cat
`

// runControl runs one maestro command and returns its output.
func runControl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := Run(context.Background(), args, &output, "0.0.0")
	return output.String(), err
}

func TestPauseAndResumeAWorker(t *testing.T) {
	confined, err := launcher.New()
	if errors.Is(err, launcher.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(drawingClaude), 0o700); err != nil { // #nosec G306 -- an executable fixture
		t.Fatal(err)
	}
	_, socket, repo, project := workerDaemonWith(t, func(server *ipc.Server) {
		capacity, err := scheduler.NewCapacity(1, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		server.Capacity = capacity
		server.Supervisor = &worker.Supervisor{
			Store: *server.Workers, Projects: server.Store, Capacity: capacity, Launcher: confined,
			LookPath:     func(name string) (string, error) { return filepath.Join(bin, name), nil },
			StartTimeout: 15 * time.Second, StopGrace: 200 * time.Millisecond,
		}
		t.Cleanup(server.Supervisor.Close)
	})
	t.Chdir(repo)
	if output, err := runWorker(t, "start", "claude-code", "--socket", socket); err != nil {
		t.Fatalf("start: %v: %s", err, output)
	}

	output, err := runControl(t, "pause", "claude-code-01", "--socket", socket)
	if err != nil || output != "claude-code-01: PAUSED\n" {
		t.Fatalf("pause: %q %v", output, err)
	}
	if _, err := runControl(t, "pause", "claude-code-01", "--socket", socket); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("a paused worker was paused again: %v", err)
	}
	output, err = runControl(t, "resume", "--project", project.ID, "claude-code-01", "--socket", socket)
	if err != nil || output != "claude-code-01: IDLE\n" {
		t.Fatalf("resume: %q %v", output, err)
	}
	if _, err := runControl(t, "resume", "claude-code-01", "--socket", socket); err == nil || !strings.Contains(err.Error(), "not PAUSED") {
		t.Fatalf("an IDLE worker was resumed: %v", err)
	}
	if _, err := runControl(t, "pause", "missing", "--socket", socket); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("unknown worker: %v", err)
	}
}

func TestPauseAndResumeErrors(t *testing.T) {
	_, socket, repo, _ := workerDaemon(t)
	t.Chdir(repo)
	for _, verb := range []string{"pause", "resume"} {
		for _, args := range [][]string{nil, {"a", "b"}} {
			if _, err := runControl(t, append([]string{verb}, args...)...); err == nil || !strings.Contains(err.Error(), "usage: maestro "+verb) {
				t.Fatalf("%s %v: %v", verb, args, err)
			}
		}
		if _, err := runControl(t, verb, "claude-code-01", "--socket", socket); err == nil || !strings.Contains(err.Error(), "this daemon does not start workers") {
			t.Fatalf("%s on a daemon without supervisor: %v", verb, err)
		}
		if output, err := runControl(t, verb, "--help"); err != nil || !strings.Contains(output, "-project") {
			t.Fatalf("%s help: %q %v", verb, output, err)
		}
	}
}
