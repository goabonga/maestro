// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package state

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/worktree"
)

// dataDirectory builds a populated data directory: one imported
// project and a migrated database with one idempotency row.
func dataDirectory(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	user := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.test"},
		{"config", "commit.gpgsign", "false"},
		{"commit", "-q", "--allow-empty", "-m", "feat: initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = user
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if _, _, err := (worktree.Store{Base: base}).Init(user); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(base, "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(Migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO idempotency_keys (key, request_hash, created_at) VALUES ('k', 'h', 'now')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestBackupAndRestoreRoundTrip(t *testing.T) {
	base := dataDirectory(t)
	backup := filepath.Join(t.TempDir(), "backup")
	manifest, err := BackupData(base, backup)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != len(Migrations) || manifest.Files["maestro.db"] == "" {
		t.Fatalf("manifest %+v", manifest)
	}

	restored := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreData(backup, restored); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(restored, "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var key string
	if err := db.QueryRow("SELECT key FROM idempotency_keys").Scan(&key); err != nil || key != "k" {
		t.Fatalf("key=%q err=%v", key, err)
	}
	projects, err := (worktree.Store{Base: restored}).Projects()
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects=%v err=%v", projects, err)
	}
	fsck := exec.Command("git", "fsck", "--strict")
	fsck.Dir = projects[0].Repository()
	if output, err := fsck.CombinedOutput(); err != nil {
		t.Fatalf("fsck: %v: %s", err, output)
	}
}

func TestBackupRequiresAStoppedDaemon(t *testing.T) {
	base := dataDirectory(t)
	lock, err := Acquire(filepath.Join(base, "daemon.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	_, err = BackupData(base, filepath.Join(t.TempDir(), "backup"))
	if err == nil || !strings.Contains(err.Error(), "stop the daemon first") {
		t.Fatalf("error %v", err)
	}
}

func TestBackupRefusesAnExistingDestination(t *testing.T) {
	base := dataDirectory(t)
	if _, err := BackupData(base, t.TempDir()); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error %v", err)
	}
}

func TestRestoreRefusesANonEmptyDestination(t *testing.T) {
	base := dataDirectory(t)
	backup := filepath.Join(t.TempDir(), "backup")
	if _, err := BackupData(base, backup); err != nil {
		t.Fatal(err)
	}
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreData(backup, full); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("error %v", err)
	}
}

func TestRestoreDetectsATamperedBackup(t *testing.T) {
	base := dataDirectory(t)
	backup := filepath.Join(t.TempDir(), "backup")
	manifest, err := BackupData(base, backup)
	if err != nil {
		t.Fatal(err)
	}
	var tampered string
	for path := range manifest.Files {
		if filepath.Base(path) == "project.json" {
			tampered = path
		}
	}
	if err := os.WriteFile(filepath.Join(backup, tampered), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	_, err = RestoreData(backup, restored)
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("error %v", err)
	}
	if _, err := os.Stat(restored); !os.IsNotExist(err) {
		t.Fatal("a failed restore left a partial copy")
	}
}
