// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package state owns Maestro's durable store: one SQLite database per
// user, versioned by ordered migrations, durable enough to hold proofs.
package state

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // database/sql driver
)

// DB is the open durable store.
type DB struct {
	*sql.DB
	path string
}

// Open opens or creates the database at path with the durability
// settings required for proofs: WAL journaling with synchronous=FULL,
// foreign keys enforced and a bounded busy timeout. The database file
// is readable by its owner only.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &DB{DB: db, path: path}, nil
}

// Path returns the database file path.
func (db *DB) Path() string {
	return db.path
}

// Version returns the current schema version of the database.
func (db *DB) Version() (int, error) {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

// Backup writes a consistent copy of the database next to it and
// returns the copy's path.
func (db *DB) Backup() (string, error) {
	backup := fmt.Sprintf("%s.backup-%d", db.path, time.Now().UnixNano())
	if _, err := db.Exec("VACUUM INTO ?", backup); err != nil {
		return "", err
	}
	if err := os.Chmod(backup, 0o600); err != nil {
		return "", err
	}
	return backup, nil
}
