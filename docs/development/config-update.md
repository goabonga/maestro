# Configuration update

A task runs on the configuration snapshot it was created with
(`config_id`, see `internal/config`). Adopting another snapshot is an
explicit, journaled operation: `maestro task config update <id>`, served
by `POST /v1/tasks/{id}/config`.

## Comparing snapshots

`config.Diff(old, new)` compares two snapshots key by key and returns
`config.Changes`, sorted by key. Each `config.Change` carries its `key`
(`budgets.max_turns_per_task`, `agents.coder.model`, `mcp.github`,
`tests.unit`, `maestro/coder.md`, `handoff_schema`, `drivers`), its
`kind` (`added`, `removed`, `changed`) and its `impact`:

| Impact | Keys |
| --- | --- |
| `ceiling` | a budget bound raised or removed: `budgets.max_turns_per_task`, `turn_timeout`, `input_wait_timeout`, `task_timeout` raised; `budgets.wall_timeout` raised or removed; `agents.<name>.max_turns_per_task` raised or set to 0 (no per-agent cap); the effective `agents.<name>.turn_timeout` raised or kept |
| `verification` | `tests.<name>` added, removed or changed |
| `objective` | everything else: instruction files, agents added or removed and their driver, `base_url`, model and `api_key_env`, MCP servers, the handoff schema version, the drivers, and any budget bound lowered or added |

`Changes.Impact()` is the strongest impact (`objective` over
`verification` over `ceiling`), empty without change.

## Adopting a snapshot

`task.AdoptConfig(task, configID, changes, update, at)` is the pure
rule; `task.Store.UpdateConfig(taskID, task.ConfigUpdate)` loads both
snapshots, computes the changes and applies the rule:

- a task already on the requested snapshot is returned unchanged with no
  change, and nothing is stored;
- a `DONE` or `CANCELLED` task, a published task (`Published`) or one
  with a pending integration operation (`!OperationSettled`) is refused
  with `task.ErrGuard`;
- the task must be quiescent: inside the update's transaction, a turn of
  the task that is not `SUCCEEDED`, `INTERRUPTED` or `FAILED`, or an
  active time interval still open in `budget_time`, refuses it with
  `task.ErrGuard`;
- the continuation moves back, never forward, along `NEW` < `PLANNING` <
  `IMPLEMENTING` < `TESTING` < `FIXING` = `REVIEWING` <
  `READY_TO_INTEGRATE` < `INTEGRATING` = `MERGE_CONFLICT` = `VALIDATING`:
  to `PLANNING` for an objective change, to `TESTING` for a verification
  change, nowhere for a ceiling change. A `BLOCKED` task keeps `BLOCKED`
  and its `resume_state` moves instead;
- an objective or verification change clears `approved_sha`,
  `result_sha` and `resolution_approved_sha`. `head_sha`, `fix_cycles`,
  `conflict_base` and `conflict_failures` are kept, as are the turn
  reservations of `budget_turns` and the active time of `budget_time`:
  nothing consumed is refunded.

The update is a compare-and-set on the task's state, version and
`config_id`, like the transitions: a concurrent transition or update
makes it fail with `task.ErrTransition`. It records a `config-update`
event in `task_events`, whose reason names both `config_id`s and the
changes, and a row of `task_config_updates` (schema migration 8) keyed
by that event: old and new `config_id`, impact and the changes as JSON.
`task.Store.ConfigUpdates(taskID)` reads them back.

## Daemon route

`POST /v1/tasks/{id}/config` goes through the `Idempotency-Key`
middleware. It takes and persists a snapshot of the project's user
working tree as task creation does (an invalid configuration is
`invalid_request`), then, when that snapshot differs from the task's:

1. reconciles the time budget with `budget.Store.Recover`, which charges
   an interval a crash left open; an exceeded budget does not stop the
   update — raising a ceiling is what it is for — and a clock regression
   is a `conflict`;
2. calls `task.Store.UpdateConfig` with `Published: false` and
   `OperationSettled: true`: no publication or integration operation is
   attached to a task by the daemon.

The response is `{"task", "previous_config_id", "config_id", "updated",
"impact", "changes"}`. Refusals are `conflict`.

## Results bound to a configuration

Once a task adopts a snapshot, results produced under the old one no
longer count for it:

- `handoff.Envelope.ForTask(taskID, configID)` refuses a document of
  another task (`handoff.ErrInvalid`) or of another `config_id`
  (`handoff.ErrConfig`);
- `testrun.ForTask(document, taskID, configID, revision)` applies it to
  a `TEST_REPORT` before checking its tested revision as
  `testrun.ForRevision` does.

Callers pass the task's current `config_id`.
