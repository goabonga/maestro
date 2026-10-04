// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package state

import "testing"

func TestCanonicalMigrationsApply(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(Migrations); err != nil {
		t.Fatal(err)
	}
	if version, err := db.Version(); err != nil || version != len(Migrations) {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if _, err := db.Exec("INSERT INTO idempotency_keys (key, request_hash, created_at) VALUES ('k', 'h', 'now')"); err != nil {
		t.Fatal(err)
	}
	// The key is unique: a second claim conflicts.
	if _, err := db.Exec("INSERT INTO idempotency_keys (key, request_hash, created_at) VALUES ('k', 'h', 'now')"); err == nil {
		t.Fatal("duplicate key accepted")
	}
}

func TestArtifactsTableKeepsOneRowPerArtifact(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(Migrations); err != nil {
		t.Fatal(err)
	}
	insert := "INSERT INTO artifacts (artifact_id, kind, task_id, turn_id, attempt_id, worker_id, config_id, digest, received_at, document) VALUES ('a1', 'PLAN', 't', 'u', 'x', 'w', 'c', 'd', 'now', '{}')"
	if _, err := db.Exec(insert); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert); err == nil {
		t.Fatal("a second row for the same artifact was accepted")
	}
}
