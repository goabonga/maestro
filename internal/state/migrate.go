// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package state

import (
	"errors"
	"fmt"
	"time"
)

// ErrNewerSchema reports a database written by a newer daemon: an older
// daemon never downgrades a schema.
var ErrNewerSchema = errors.New("the database schema is newer than this daemon supports")

// Migration is one ordered schema step. Versions are consecutive and
// start at 1.
type Migration struct {
	Version int
	SQL     string
}

// Migrate brings the database to the latest known version. A database
// already holding data is backed up first. Each step runs in its own
// transaction, so a crash leaves a consistent prefix of the schema. A
// schema newer than the known migrations is refused with ErrNewerSchema.
// The caller holds the user lock.
func (db *DB) Migrate(migrations []Migration) error {
	for i, migration := range migrations {
		if migration.Version != i+1 {
			return fmt.Errorf("migrations are not consecutive at index %d: version %d", i, migration.Version)
		}
	}
	current, err := db.Version()
	if err != nil {
		return err
	}
	if current > len(migrations) {
		return fmt.Errorf("%w: database version %d, daemon supports up to %d",
			ErrNewerSchema, current, len(migrations))
	}
	if current == len(migrations) {
		return nil
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	if current > 0 {
		if _, err := db.Backup(); err != nil {
			return fmt.Errorf("pre-migration backup: %w", err)
		}
	}
	for _, migration := range migrations[current:] {
		if err := db.apply(migration); err != nil {
			return fmt.Errorf("migration %d: %w", migration.Version, err)
		}
	}
	return nil
}

// apply runs one migration step in its own transaction.
func (db *DB) apply(migration Migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(migration.SQL); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
		migration.Version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", migration.Version)); err != nil {
		return err
	}
	return tx.Commit()
}
