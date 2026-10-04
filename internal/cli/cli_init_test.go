// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo creates a Git repository with one commit and returns its path.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"commit", "--quiet", "--allow-empty", "--no-gpg-sign", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	return dir
}

func TestInitRegistersRepository(t *testing.T) {
	data := t.TempDir()
	t.Setenv("MAESTRO_DATA_HOME", data)
	repo := gitRepo(t)

	var output bytes.Buffer
	if err := Run(context.Background(), []string{"init", repo}, &output, "0.0.0"); err != nil {
		t.Fatalf("init: %v: %s", err, output.String())
	}
	if !strings.HasPrefix(output.String(), "project registered: ") {
		t.Fatalf("unexpected output: %q", output.String())
	}
	id := strings.TrimSpace(strings.TrimPrefix(output.String(), "project registered: "))
	repository := filepath.Join(data, "projects", id, "repository.git")
	cmd := exec.Command("git", "--git-dir", repository, "rev-parse", "refs/heads/maestro/integration")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("imported repository missing: %v: %s", err, out)
	}

	output.Reset()
	if err := Run(context.Background(), []string{"init", repo}, &output, "0.0.0"); err != nil {
		t.Fatalf("second init: %v: %s", err, output.String())
	}
	if output.String() != "project already registered: "+id+"\n" {
		t.Fatalf("unexpected output: %q", output.String())
	}
}

func TestInitRejectsNonRepository(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", t.TempDir())
	var output bytes.Buffer
	err := Run(context.Background(), []string{"init", t.TempDir()}, &output, "0.0.0")
	if err == nil || !strings.Contains(err.Error(), "not a Git repository") {
		t.Fatalf("expected a not-a-repository error, got %v", err)
	}
}

func TestInitRejectsExtraArguments(t *testing.T) {
	var output bytes.Buffer
	err := Run(context.Background(), []string{"init", "a", "b"}, &output, "0.0.0")
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("expected an argument error, got %v", err)
	}
}
