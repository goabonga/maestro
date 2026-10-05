// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package state

// Migrations is the ordered schema of the store. Every release appends
// here and never edits an applied step.
var Migrations = []Migration{
	{Version: 1, SQL: `CREATE TABLE idempotency_keys (
		key TEXT PRIMARY KEY,
		request_hash TEXT NOT NULL,
		response_status INTEGER,
		response_body BLOB,
		created_at TEXT NOT NULL
	)`},
	{Version: 2, SQL: `CREATE TABLE artifacts (
		artifact_id TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		task_id TEXT NOT NULL,
		turn_id TEXT NOT NULL,
		attempt_id TEXT NOT NULL,
		worker_id TEXT NOT NULL,
		config_id TEXT NOT NULL,
		digest TEXT NOT NULL,
		received_at TEXT NOT NULL,
		document BLOB NOT NULL
	);
	CREATE INDEX artifacts_by_turn ON artifacts (task_id, turn_id, attempt_id)`},
	{Version: 3, SQL: `CREATE TABLE config_snapshots (
		config_id TEXT PRIMARY KEY,
		created_at TEXT NOT NULL,
		document BLOB NOT NULL
	)`},
	{Version: 4, SQL: `CREATE TABLE turns (
		turn_id TEXT PRIMARY KEY,
		attempt_id TEXT NOT NULL UNIQUE,
		task_id TEXT NOT NULL,
		agent TEXT NOT NULL,
		config_id TEXT NOT NULL REFERENCES config_snapshots (config_id),
		previous_turn_id TEXT UNIQUE REFERENCES turns (turn_id),
		state TEXT NOT NULL,
		turn_timeout_ns INTEGER NOT NULL,
		input_wait_timeout_ns INTEGER NOT NULL,
		created_at TEXT NOT NULL,
		admitted_at TEXT,
		waiting_since TEXT,
		updated_at TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX turns_by_task ON turns (task_id);
	CREATE TABLE turn_events (
		event_id INTEGER PRIMARY KEY AUTOINCREMENT,
		turn_id TEXT NOT NULL REFERENCES turns (turn_id),
		from_state TEXT NOT NULL,
		to_state TEXT NOT NULL,
		reason TEXT NOT NULL,
		at TEXT NOT NULL
	);
	CREATE INDEX turn_events_by_turn ON turn_events (turn_id, event_id)`},
}
