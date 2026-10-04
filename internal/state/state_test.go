// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openDB opens a database in a temporary directory.
func openDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

var testMigrations = []Migration{
	{Version: 1, SQL: "CREATE TABLE first (id INTEGER PRIMARY KEY)"},
	{Version: 2, SQL: "CREATE TABLE second (id INTEGER PRIMARY KEY)"},
}

func TestOpenConfiguresDurability(t *testing.T) {
	db := openDB(t)
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal_mode=%q err=%v", journal, err)
	}
	var synchronous int
	if err := db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("synchronous=%d err=%v", synchronous, err)
	}
	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d err=%v", foreignKeys, err)
	}
	info, err := os.Stat(db.Path())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode(), err)
	}
}

func TestMigrateAppliesOrderedStepsOnce(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(testMigrations); err != nil {
		t.Fatal(err)
	}
	if version, err := db.Version(); err != nil || version != 2 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&count); err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if _, err := db.Exec("INSERT INTO second (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	// A second run is a no-op.
	if err := db.Migrate(testMigrations); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateRefusesANewerSchema(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(testMigrations); err != nil {
		t.Fatal(err)
	}
	err := db.Migrate(testMigrations[:1])
	if !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("expected ErrNewerSchema, got %v", err)
	}
}

func TestMigrateRejectsNonConsecutiveVersions(t *testing.T) {
	db := openDB(t)
	bad := []Migration{{Version: 1, SQL: "CREATE TABLE first (id INTEGER)"}, {Version: 3, SQL: "CREATE TABLE third (id INTEGER)"}}
	if err := db.Migrate(bad); err == nil || !strings.Contains(err.Error(), "not consecutive") {
		t.Fatalf("error %v", err)
	}
}

func TestFailedMigrationLeavesAConsistentPrefix(t *testing.T) {
	db := openDB(t)
	broken := []Migration{
		testMigrations[0],
		{Version: 2, SQL: "CREATE SYNTAX ERROR"},
	}
	if err := db.Migrate(broken); err == nil || !strings.Contains(err.Error(), "migration 2") {
		t.Fatalf("error %v", err)
	}
	if version, err := db.Version(); err != nil || version != 1 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	// The fixed step resumes from the kept prefix.
	if err := db.Migrate(testMigrations); err != nil {
		t.Fatal(err)
	}
	if version, err := db.Version(); err != nil || version != 2 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestMigrateBacksANonEmptyDatabaseUp(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(testMigrations[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO first (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(testMigrations); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(db.Path() + ".backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups=%v err=%v", backups, err)
	}
	copy, err := Open(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = copy.Close() }()
	if version, err := copy.Version(); err != nil || version != 1 {
		t.Fatalf("backup version=%d err=%v", version, err)
	}
	var count int
	if err := copy.QueryRow("SELECT count(*) FROM first").Scan(&count); err != nil || count != 1 {
		t.Fatalf("backup count=%d err=%v", count, err)
	}
}

func TestDataSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(testMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO first (id) VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var id int
	if err := reopened.QueryRow("SELECT id FROM first").Scan(&id); err != nil || id != 7 {
		t.Fatalf("id=%d err=%v", id, err)
	}
}

func TestAcquireIsExclusiveUntilRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); err == nil || !strings.Contains(err.Error(), "holds the lock") {
		t.Fatalf("error %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
}
