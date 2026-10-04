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
}
