// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/worktree"
)

func TestBackupRestoreAndGCCommands(t *testing.T) {
	data := t.TempDir()
	t.Setenv("MAESTRO_DATA_HOME", data)
	repo := gitRepo(t)
	ctx := context.Background()

	var output bytes.Buffer
	if err := Run(ctx, []string{"init", repo}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}

	// Age the database so gc has a superseded backup to collect.
	db, err := state.Open(filepath.Join(data, "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Backup(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Backup(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	backupDir := filepath.Join(t.TempDir(), "backup")
	output.Reset()
	if err := Run(ctx, []string{"backup", backupDir}, &output, "0.0.0"); err != nil {
		t.Fatalf("backup: %v: %s", err, output.String())
	}
	if !strings.Contains(output.String(), "backup written to") {
		t.Fatalf("unexpected output: %q", output.String())
	}

	restored := filepath.Join(t.TempDir(), "restored")
	output.Reset()
	if err := Run(ctx, []string{"restore", backupDir, restored}, &output, "0.0.0"); err != nil {
		t.Fatalf("restore: %v: %s", err, output.String())
	}
	projects, err := (worktree.Store{Base: restored}).Projects()
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects=%v err=%v", projects, err)
	}

	output.Reset()
	if err := Run(ctx, []string{"gc", "--dry-run"}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "superseded") || !strings.Contains(output.String(), "nothing was removed") {
		t.Fatalf("unexpected dry run: %q", output.String())
	}
	output.Reset()
	if err := Run(ctx, []string{"gc", "--apply"}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "journal at") {
		t.Fatalf("unexpected apply: %q", output.String())
	}
	output.Reset()
	if err := Run(ctx, []string{"gc", "--dry-run"}, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "nothing to collect") {
		t.Fatalf("unexpected output: %q", output.String())
	}
}

func TestMaintenanceCommandErrors(t *testing.T) {
	t.Setenv("MAESTRO_DATA_HOME", t.TempDir())
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"backup"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "usage: maestro backup") {
		t.Fatalf("error %v", err)
	}
	if err := Run(context.Background(), []string{"restore", "only"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "usage: maestro restore") {
		t.Fatalf("error %v", err)
	}
	if err := Run(context.Background(), []string{"gc"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "usage: maestro gc") {
		t.Fatalf("error %v", err)
	}
	if err := Run(context.Background(), []string{"restore", t.TempDir(), t.TempDir()}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "not a backup") {
		t.Fatalf("error %v", err)
	}
}
