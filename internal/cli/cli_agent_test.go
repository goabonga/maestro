// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/launcher"
)

// fakeAgents puts version-printing stand-ins first on PATH, so the
// doctor never runs a real agent CLI.
func fakeAgents(t *testing.T, claude, codex string) {
	t.Helper()
	bin := t.TempDir()
	for name, line := range map[string]string{"claude": claude, "codex": codex} {
		if line == "" {
			continue
		}
		script := "#!/bin/sh\necho '" + line + "'\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// PATH holds only the stand-ins and links to the sandbox tools: no
	// directory that could hold a real agent binary.
	for _, tool := range []string{"bwrap", "prlimit"} {
		if path, err := exec.LookPath(tool); err == nil {
			if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("PATH", bin)
}

func TestAgentDoctorReportsValidatedAgents(t *testing.T) {
	if _, err := launcher.New(); errors.Is(err, launcher.ErrUnsupported) {
		t.Skipf("host cannot confine: %v", err)
	}
	fakeAgents(t, "2.1.289 (Claude Code)", "codex-cli 0.160.0")
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"agent", "doctor"}, &output, "0.0.0"); err != nil {
		t.Fatalf("doctor: %v: %s", err, output.String())
	}
	report := output.String()
	for _, want := range []string{"sandbox", "claude-code-2.1", "codex-0.160"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report misses %q: %s", want, report)
		}
	}
}

func TestAgentDoctorRefusesUnvalidatedOrMissingAgents(t *testing.T) {
	fakeAgents(t, "2.2.0 (Claude Code)", "")
	var output bytes.Buffer
	err := Run(context.Background(), []string{"agent", "doctor"}, &output, "0.0.0")
	if err == nil || !strings.Contains(err.Error(), "checks refused") {
		t.Fatalf("error %v", err)
	}
	report := output.String()
	if !strings.Contains(report, "2.2.0") || !strings.Contains(report, "codex not found") {
		t.Fatalf("report %s", report)
	}
}

func TestAgentCommandUsage(t *testing.T) {
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"agent"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "usage: maestro agent doctor") {
		t.Fatalf("error %v", err)
	}
}
