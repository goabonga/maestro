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

func TestTasksReferenceTheirSnapshotAndKeepTheirContinuation(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(Migrations); err != nil {
		t.Fatal(err)
	}
	insert := "INSERT INTO tasks (task_id, config_id, task_base_sha, branch, state, version, resume_state, blocked_reason, max_fix_cycles, created_at, updated_at) VALUES (?, 'sha256-x', 'b', ?, ?, 1, ?, ?, 3, 'now', 'now')"
	if _, err := db.Exec(insert, "t1", "maestro/task-t1", "NEW", "", ""); err == nil {
		t.Fatal("a task without its configuration snapshot was accepted")
	}
	if _, err := db.Exec("INSERT INTO config_snapshots (config_id, created_at, document) VALUES ('sha256-x', 'now', '{}')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert, "t1", "maestro/task-t1", "NEW", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert, "t2", "maestro/task-t1", "NEW", "", ""); err == nil {
		t.Fatal("a second task on the same branch was accepted")
	}
	if _, err := db.Exec(insert, "t2", "maestro/task-t2", "BLOCKED", "", "timeout"); err == nil {
		t.Fatal("a blocked task without its resume state was accepted")
	}
	if _, err := db.Exec(insert, "t2", "maestro/task-t2", "BLOCKED", "TESTING", ""); err == nil {
		t.Fatal("a blocked task without its reason was accepted")
	}
	if _, err := db.Exec(insert, "t2", "maestro/task-t2", "BLOCKED", "TESTING", "timeout"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO task_events (task_id, event, from_state, to_state, reason, at) VALUES ('missing', 'create', '', 'NEW', '', 'now')"); err == nil {
		t.Fatal("an event for an unknown task was accepted")
	}
}

func TestBudgetTablesKeyReservationsAndOpenIntervals(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(Migrations); err != nil {
		t.Fatal(err)
	}
	reserve := "INSERT INTO budget_turns (task_id, reservation_key, agent, reserved_at) VALUES (?, ?, ?, 'now')"
	if _, err := db.Exec(reserve, "t1", "k1", "coder"); err == nil {
		t.Fatal("a reservation for an unknown task was accepted")
	}
	if _, err := db.Exec("INSERT INTO config_snapshots (config_id, created_at, document) VALUES ('sha256-x', 'now', '{}')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO tasks (task_id, config_id, task_base_sha, branch, state, version, max_fix_cycles, created_at, updated_at) VALUES ('t1', 'sha256-x', 'b', 'maestro/task-t1', 'NEW', 1, 3, 'now', 'now')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(reserve, "t1", "k1", "coder"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(reserve, "t1", "k1", "reviewer"); err == nil {
		t.Fatal("a reservation key was reserved twice")
	}
	if _, err := db.Exec(reserve, "t1", "", "coder"); err == nil {
		t.Fatal("an empty reservation key was accepted")
	}
	clock := "INSERT INTO budget_time (task_id, open_step, open_since, open_mark, updated_at) VALUES ('t1', ?, ?, ?, 'now')"
	if _, err := db.Exec(clock, "coding", "", ""); err == nil {
		t.Fatal("an open interval without its timestamps was accepted")
	}
	if _, err := db.Exec(clock, "coding", "now", "now"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE budget_time SET active_ns = -1"); err == nil {
		t.Fatal("a negative active time was accepted")
	}
}

func TestTasksCarryTheirProjectAndDescription(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(Migrations[:5]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO config_snapshots (config_id, created_at, document) VALUES ('sha256-x', 'now', '{}')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO tasks (task_id, config_id, task_base_sha, branch, state, version, max_fix_cycles, created_at, updated_at) VALUES ('t1', 'sha256-x', 'b', 'maestro/task-t1', 'NEW', 1, 3, 'now', 'now')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(Migrations); err != nil {
		t.Fatal(err)
	}
	// A task stored before the migration keeps empty values.
	var project, description string
	if err := db.QueryRow("SELECT project_id, description FROM tasks WHERE task_id = 't1'").Scan(&project, &description); err != nil {
		t.Fatal(err)
	}
	if project != "" || description != "" {
		t.Fatalf("project=%q description=%q", project, description)
	}
	if _, err := db.Exec("INSERT INTO tasks (task_id, project_id, description, config_id, task_base_sha, branch, state, version, max_fix_cycles, created_at, updated_at) VALUES ('t2', 'p1', 'add a flag', 'sha256-x', 'b', 'maestro/task-t2', 'NEW', 1, 3, 'now', 'now')"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM tasks WHERE project_id = 'p1' AND description = 'add a flag'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	var index string
	if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'tasks_by_project'").Scan(&index); err != nil {
		t.Fatal(err)
	}
}

func TestTaskConfigUpdatesReferenceTheirEventAndSnapshots(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(Migrations); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sha256-x", "sha256-y"} {
		if _, err := db.Exec("INSERT INTO config_snapshots (config_id, created_at, document) VALUES (?, 'now', '{}')", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO tasks (task_id, config_id, task_base_sha, branch, state, version, max_fix_cycles, created_at, updated_at) VALUES ('t1', 'sha256-x', 'b', 'maestro/task-t1', 'NEW', 1, 3, 'now', 'now')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO task_events (event_id, task_id, event, from_state, to_state, reason, at) VALUES (1, 't1', 'config-update', 'NEW', 'NEW', '', 'now')"); err != nil {
		t.Fatal(err)
	}
	insert := "INSERT INTO task_config_updates (event_id, task_id, old_config_id, new_config_id, impact, changes) VALUES (?, 't1', ?, ?, 'objective', '[]')"
	if _, err := db.Exec(insert, 2, "sha256-x", "sha256-y"); err == nil {
		t.Fatal("an update without its task event was accepted")
	}
	if _, err := db.Exec(insert, 1, "sha256-x", "sha256-missing"); err == nil {
		t.Fatal("an update to an unknown snapshot was accepted")
	}
	if _, err := db.Exec(insert, 1, "sha256-x", "sha256-x"); err == nil {
		t.Fatal("an update to the same snapshot was accepted")
	}
	if _, err := db.Exec(insert, 1, "sha256-x", "sha256-y"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert, 1, "sha256-x", "sha256-y"); err == nil {
		t.Fatal("a second update for the same event was accepted")
	}
}

func TestOperationsJournalKeepsItsInvariants(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(Migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO config_snapshots (config_id, created_at, document) VALUES ('sha256-x', 'now', '{}')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO tasks (task_id, config_id, task_base_sha, branch, state, version, max_fix_cycles, created_at, updated_at) VALUES ('t1', 'sha256-x', 'b', 'maestro/task-t1', 'NEW', 1, 3, 'now', 'now')"); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO operations (id, type, state, version, project_id, config_id, task_id, worker_id, attempt_id,
		supersedes_operation_id, integration_base_sha, result_sha, commit_metadata, error, started_at, updated_at)
		VALUES (?, ?, ?, 1, 'p1', ?, ?, 'w', 'a', ?, 'base', ?, ?, ?, 'now', 'now')`
	refused := []struct {
		name string
		args []any
	}{
		{"an unknown type", []any{"o1", "MERGE", "PREPARED", "sha256-x", "t1", nil, "", "{}", ""}},
		{"an unknown state", []any{"o1", "SYNC", "DONE", "sha256-x", nil, nil, "", "", ""}},
		{"an unknown snapshot", []any{"o1", "SYNC", "PREPARED", "sha256-missing", nil, nil, "", "", ""}},
		{"an unknown task", []any{"o1", "SYNC", "PREPARED", "sha256-x", "missing", nil, "", "", ""}},
		{"an integration without its task", []any{"o1", "INTEGRATE", "PREPARED", "sha256-x", nil, nil, "", "{}", ""}},
		{"an integration without its commit metadata", []any{"o1", "INTEGRATE", "PREPARED", "sha256-x", "t1", nil, "", "", ""}},
		{"a tested publication", []any{"o1", "PUBLISH", "TESTED", "sha256-x", nil, nil, "r", "", ""}},
		{"an applied operation without its result", []any{"o1", "SYNC", "APPLIED", "sha256-x", nil, nil, "", "", ""}},
		{"a failed operation without its error", []any{"o1", "SYNC", "FAILED", "sha256-x", nil, nil, "", "", ""}},
		{"a superseded unknown operation", []any{"o1", "SYNC", "PREPARED", "sha256-x", nil, "missing", "", "", ""}},
	}
	for _, c := range refused {
		if _, err := db.Exec(insert, c.args...); err == nil {
			t.Fatalf("%s was accepted", c.name)
		}
	}
	if _, err := db.Exec(insert, "o1", "INTEGRATE", "PREPARED", "sha256-x", "t1", nil, "", "{}", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert, "o2", "INTEGRATE", "PREPARED", "sha256-x", "t1", "o1", "", "{}", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert, "o3", "INTEGRATE", "PREPARED", "sha256-x", "t1", "o1", "", "{}", ""); err == nil {
		t.Fatal("an operation was superseded twice")
	}
	if _, err := db.Exec("INSERT INTO operation_events (operation_id, event, from_state, to_state, reason, at) VALUES ('missing', 'prepare', '', 'PREPARED', '', 'now')"); err == nil {
		t.Fatal("an event for an unknown operation was accepted")
	}
	if _, err := db.Exec("INSERT INTO operation_events (operation_id, event, from_state, to_state, reason, at) VALUES ('o1', 'prepare', '', 'PREPARED', '', 'now')"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkersKeepTheirIdentityAndAssignment(t *testing.T) {
	db := openDB(t)
	if err := db.Migrate(Migrations); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO config_snapshots (config_id, created_at, document) VALUES ('sha256-x', 'now', '{}')",
		"INSERT INTO tasks (task_id, config_id, task_base_sha, branch, state, version, max_fix_cycles, created_at, updated_at) VALUES ('t1', 'sha256-x', 'b', 'maestro/task-t1', 'NEW', 1, 3, 'now', 'now')",
		"INSERT INTO turns (turn_id, attempt_id, task_id, agent, config_id, state, turn_timeout_ns, input_wait_timeout_ns, created_at, updated_at) VALUES ('u1', 'x1', 't1', 'claude', 'sha256-x', 'PREPARED', 1, 1, 'now', 'now')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	insert := `INSERT INTO workers (project_id, name, agent, agent_kind, driver, repository, state, version,
		task_id, role, turn_id, created_at, updated_at) VALUES ('p1', ?, 'claude', 'claude-code', 'claude-code-2.1', '/r', ?, 1, ?, ?, ?, 'now', 'now')`
	refused := []struct {
		name string
		args []any
	}{
		{"an unknown state", []any{"w1", "RUNNING", nil, "", nil}},
		{"a busy worker without its assignment", []any{"w1", "BUSY", nil, "", nil}},
		{"an idle worker with an assignment", []any{"w1", "IDLE", "t1", "implementation", "u1"}},
		{"an assignment without its turn", []any{"w1", "ATTACHED", "t1", "implementation", nil}},
		{"an assignment without its role", []any{"w1", "BUSY", "t1", "", "u1"}},
		{"an assignment of an unknown task", []any{"w1", "BUSY", "missing", "implementation", "u1"}},
		{"an assignment of an unknown turn", []any{"w1", "BUSY", "t1", "implementation", "missing"}},
	}
	for _, c := range refused {
		if _, err := db.Exec(insert, c.args...); err == nil {
			t.Fatalf("%s was accepted", c.name)
		}
	}
	if _, err := db.Exec(insert, "w1", "BUSY", "t1", "implementation", "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert, "w1", "STOPPED", nil, "", nil); err == nil {
		t.Fatal("a second worker with the same name in the project was accepted")
	}
	if _, err := db.Exec(insert, "w2", "BUSY", "t1", "implementation", "u1"); err == nil {
		t.Fatal("a turn assigned to two workers was accepted")
	}
	if _, err := db.Exec(insert, "w2", "STOPPED", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	event := "INSERT INTO worker_events (project_id, name, event, from_state, to_state, reason, at) VALUES ('p1', ?, 'register', '', 'STOPPED', '', 'now')"
	if _, err := db.Exec(event, "missing"); err == nil {
		t.Fatal("an event for an unknown worker was accepted")
	}
	if _, err := db.Exec(event, "w1"); err != nil {
		t.Fatal(err)
	}
}
