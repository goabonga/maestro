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
	{Version: 5, SQL: `CREATE TABLE tasks (
		task_id TEXT PRIMARY KEY,
		config_id TEXT NOT NULL REFERENCES config_snapshots (config_id),
		task_base_sha TEXT NOT NULL,
		branch TEXT NOT NULL UNIQUE,
		state TEXT NOT NULL,
		version INTEGER NOT NULL,
		resume_state TEXT NOT NULL DEFAULT '',
		blocked_reason TEXT NOT NULL DEFAULT '',
		head_sha TEXT NOT NULL DEFAULT '',
		approved_sha TEXT NOT NULL DEFAULT '',
		result_sha TEXT NOT NULL DEFAULT '',
		fix_cycles INTEGER NOT NULL DEFAULT 0 CHECK (fix_cycles >= 0),
		max_fix_cycles INTEGER NOT NULL CHECK (max_fix_cycles >= 0),
		conflict_base TEXT NOT NULL DEFAULT '',
		conflict_failures INTEGER NOT NULL DEFAULT 0 CHECK (conflict_failures >= 0),
		resolution_approved_sha TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		CHECK (state <> 'BLOCKED' OR (resume_state <> '' AND blocked_reason <> ''))
	);
	CREATE TABLE task_events (
		event_id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL REFERENCES tasks (task_id),
		event TEXT NOT NULL,
		from_state TEXT NOT NULL,
		to_state TEXT NOT NULL,
		revision TEXT NOT NULL DEFAULT '',
		stale INTEGER NOT NULL DEFAULT 0,
		reason TEXT NOT NULL,
		at TEXT NOT NULL
	);
	CREATE INDEX task_events_by_task ON task_events (task_id, event_id)`},
	{Version: 6, SQL: `CREATE TABLE budget_turns (
		task_id TEXT NOT NULL REFERENCES tasks (task_id),
		reservation_key TEXT NOT NULL CHECK (reservation_key <> ''),
		agent TEXT NOT NULL CHECK (agent <> ''),
		reserved_at TEXT NOT NULL,
		PRIMARY KEY (task_id, reservation_key)
	);
	CREATE INDEX budget_turns_by_agent ON budget_turns (task_id, agent);
	CREATE TABLE budget_time (
		task_id TEXT PRIMARY KEY REFERENCES tasks (task_id),
		active_ns INTEGER NOT NULL DEFAULT 0 CHECK (active_ns >= 0),
		open_step TEXT NOT NULL DEFAULT '',
		open_since TEXT NOT NULL DEFAULT '',
		open_mark TEXT NOT NULL DEFAULT '',
		open_ns INTEGER NOT NULL DEFAULT 0 CHECK (open_ns >= 0),
		updated_at TEXT NOT NULL,
		CHECK (open_step = '' OR (open_since <> '' AND open_mark <> ''))
	)`},
}
