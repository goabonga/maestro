// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectListShowsProjectsAndStates(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", t.TempDir())
	repo := gitRepo(t)

	var output bytes.Buffer
	if err := Run(context.Background(), []string{"project", "list"}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "no projects") {
		t.Fatalf("unexpected output: %q", output.String())
	}

	output.Reset()
	if err := Run(context.Background(), []string{"init", repo}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(strings.TrimPrefix(output.String(), "project registered: "))

	output.Reset()
	if err := Run(context.Background(), []string{"project", "list"}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	listing := output.String()
	if !strings.Contains(listing, id) || !strings.Contains(listing, "ok") {
		t.Fatalf("listing misses the project: %s", listing)
	}

	if err := os.Rename(repo, repo+".moved"); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := Run(context.Background(), []string{"project", "list"}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "missing") {
		t.Fatalf("listing misses the missing state: %s", output.String())
	}
}

func TestProjectRelocateUpdatesTheRegisteredPath(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", t.TempDir())
	repo := gitRepo(t)
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"init", repo}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(strings.TrimPrefix(output.String(), "project registered: "))

	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(repo, moved); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := Run(context.Background(), []string{"project", "relocate", id, moved}, &output, "0.0.0"); err != nil {
		t.Fatalf("relocate: %v: %s", err, output.String())
	}
	if !strings.Contains(output.String(), "now at") {
		t.Fatalf("unexpected output: %q", output.String())
	}

	// A second init at the new path resolves to the same project.
	output.Reset()
	if err := Run(context.Background(), []string{"init", moved}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if output.String() != "project already registered: "+id+"\n" {
		t.Fatalf("unexpected output: %q", output.String())
	}
}

func TestProjectCommandErrors(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", t.TempDir())
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"project"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "usage: maestro project") {
		t.Fatalf("error %v", err)
	}
	if err := Run(context.Background(), []string{"project", "destroy"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "unknown project command") {
		t.Fatalf("error %v", err)
	}
	if err := Run(context.Background(), []string{"project", "relocate", "0000"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "usage: maestro project relocate") {
		t.Fatalf("error %v", err)
	}
	if err := Run(context.Background(), []string{"project", "relocate", "0000", t.TempDir()}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "unknown project") {
		t.Fatalf("error %v", err)
	}
}
