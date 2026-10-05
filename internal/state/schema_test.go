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

func TestConfigSnapshotsKeepOneRowPerID(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(Migrations); err != nil {
		t.Fatal(err)
	}
	insert := "INSERT INTO config_snapshots (config_id, created_at, document) VALUES ('sha256-x', 'now', '{}')"
	if _, err := db.Exec(insert); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert); err == nil {
		t.Fatal("a second row for the same config_id was accepted")
	}
}

func TestTurnsReferenceTheirSnapshotAndPreviousTurn(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(Migrations); err != nil {
		t.Fatal(err)
	}
	insert := "INSERT INTO turns (turn_id, attempt_id, task_id, agent, config_id, previous_turn_id, state, turn_timeout_ns, input_wait_timeout_ns, created_at, updated_at) VALUES (?, ?, 't', 'a', 'sha256-x', ?, 'PREPARED', 1, 1, 'now', 'now')"
	if _, err := db.Exec(insert, "u1", "x1", nil); err == nil {
		t.Fatal("a turn without its configuration snapshot was accepted")
	}
	if _, err := db.Exec("INSERT INTO config_snapshots (config_id, created_at, document) VALUES ('sha256-x', 'now', '{}')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert, "u1", "x1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert, "u2", "x1", "u1"); err == nil {
		t.Fatal("a reused attempt_id was accepted")
	}
	if _, err := db.Exec(insert, "u2", "x2", "missing"); err == nil {
		t.Fatal("a retry of an unknown turn was accepted")
	}
	if _, err := db.Exec(insert, "u2", "x2", "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert, "u3", "x3", "u1"); err == nil {
		t.Fatal("a second retry of the same turn was accepted")
	}
	if _, err := db.Exec("INSERT INTO turn_events (turn_id, from_state, to_state, reason, at) VALUES ('missing', '', 'PREPARED', '', 'now')"); err == nil {
		t.Fatal("an event for an unknown turn was accepted")
	}
}
