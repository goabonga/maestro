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
}
