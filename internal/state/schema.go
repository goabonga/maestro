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
	{Version: 7, SQL: `ALTER TABLE tasks ADD COLUMN project_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE tasks ADD COLUMN description TEXT NOT NULL DEFAULT '';
	CREATE INDEX tasks_by_project ON tasks (project_id, created_at)`},
	{Version: 8, SQL: `CREATE TABLE task_config_updates (
		event_id INTEGER PRIMARY KEY REFERENCES task_events (event_id),
		task_id TEXT NOT NULL REFERENCES tasks (task_id),
		old_config_id TEXT NOT NULL REFERENCES config_snapshots (config_id),
		new_config_id TEXT NOT NULL REFERENCES config_snapshots (config_id),
		impact TEXT NOT NULL,
		changes TEXT NOT NULL,
		CHECK (old_config_id <> new_config_id)
	);
	CREATE INDEX task_config_updates_by_task ON task_config_updates (task_id, event_id)`},
	{Version: 9, SQL: `CREATE TABLE operations (
		id TEXT PRIMARY KEY,
		type TEXT NOT NULL CHECK (type IN ('INTEGRATE', 'SYNC', 'PUBLISH')),
		state TEXT NOT NULL CHECK (state IN
			('PREPARED', 'STARTED', 'APPLIED', 'TESTED', 'COMMITTED', 'FAILED', 'ROLLED_BACK')),
		version INTEGER NOT NULL CHECK (version >= 1),
		project_id TEXT NOT NULL CHECK (project_id <> ''),
		config_id TEXT NOT NULL REFERENCES config_snapshots (config_id),
		task_id TEXT REFERENCES tasks (task_id),
		worker_id TEXT NOT NULL,
		attempt_id TEXT NOT NULL,
		supersedes_operation_id TEXT UNIQUE REFERENCES operations (id),
		task_base_sha TEXT NOT NULL DEFAULT '',
		integration_base_sha TEXT NOT NULL CHECK (integration_base_sha <> ''),
		source_head_sha TEXT NOT NULL DEFAULT '',
		source_commits TEXT NOT NULL DEFAULT '[]',
		candidate_ref TEXT NOT NULL DEFAULT '',
		candidate_tree_sha TEXT NOT NULL DEFAULT '',
		result_sha TEXT NOT NULL DEFAULT '',
		commit_metadata TEXT NOT NULL DEFAULT '',
		test_report_ids TEXT NOT NULL DEFAULT '[]',
		review_artifact_ids TEXT NOT NULL DEFAULT '[]',
		approval_artifact_ids TEXT NOT NULL DEFAULT '[]',
		error TEXT NOT NULL DEFAULT '',
		started_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		CHECK (type <> 'INTEGRATE' OR (task_id IS NOT NULL AND commit_metadata <> '')),
		CHECK (type <> 'PUBLISH' OR state <> 'TESTED'),
		CHECK (state NOT IN ('APPLIED', 'TESTED', 'COMMITTED') OR result_sha <> ''),
		CHECK (state NOT IN ('FAILED', 'ROLLED_BACK') OR error <> '')
	);
	CREATE INDEX operations_by_project ON operations (project_id, started_at);
	CREATE INDEX operations_by_task ON operations (task_id);
	CREATE TABLE operation_events (
		event_id INTEGER PRIMARY KEY AUTOINCREMENT,
		operation_id TEXT NOT NULL REFERENCES operations (id),
		event TEXT NOT NULL,
		from_state TEXT NOT NULL,
		to_state TEXT NOT NULL,
		reason TEXT NOT NULL,
		at TEXT NOT NULL
	);
	CREATE INDEX operation_events_by_operation ON operation_events (operation_id, event_id)`},
	{Version: 10, SQL: `CREATE TABLE workers (
		project_id TEXT NOT NULL CHECK (project_id <> ''),
		name TEXT NOT NULL CHECK (name <> ''),
		agent TEXT NOT NULL CHECK (agent <> ''),
		agent_kind TEXT NOT NULL CHECK (agent_kind <> ''),
		driver TEXT NOT NULL CHECK (driver <> ''),
		repository TEXT NOT NULL CHECK (repository <> ''),
		state TEXT NOT NULL CHECK (state IN ('STOPPED', 'STARTING', 'IDLE', 'BUSY', 'WAITING_INPUT',
			'ATTACHED', 'PAUSED', 'DRAINING', 'FAILED')),
		version INTEGER NOT NULL CHECK (version >= 1),
		task_id TEXT REFERENCES tasks (task_id),
		role TEXT NOT NULL DEFAULT '',
		turn_id TEXT UNIQUE REFERENCES turns (turn_id),
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (project_id, name),
		CHECK ((task_id IS NULL) = (turn_id IS NULL) AND (task_id IS NULL) = (role = '')),
		CHECK (state NOT IN ('BUSY', 'WAITING_INPUT') OR turn_id IS NOT NULL),
		CHECK (state NOT IN ('STOPPED', 'STARTING', 'IDLE', 'PAUSED') OR turn_id IS NULL)
	);
	CREATE TABLE worker_events (
		event_id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id TEXT NOT NULL,
		name TEXT NOT NULL,
		event TEXT NOT NULL,
		from_state TEXT NOT NULL,
		to_state TEXT NOT NULL,
		task_id TEXT NOT NULL DEFAULT '',
		turn_id TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL,
		at TEXT NOT NULL,
		FOREIGN KEY (project_id, name) REFERENCES workers (project_id, name)
	);
	CREATE INDEX worker_events_by_worker ON worker_events (project_id, name, event_id)`},
}
