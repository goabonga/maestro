// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestIntegrationHeadIgnoresAnInheritedGitDir(t *testing.T) {
	_, _, project := taskServer(t)
	want := integrationHeadOf(t, project)
	other := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = other
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	if got, err := integrationHead(project); err != nil || got != want {
		t.Fatalf("integrationHead = %q, %v; want %s", got, err, want)
	}
}
