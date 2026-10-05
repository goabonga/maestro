# Task API

The daemon serves tasks on its versioned JSON API (`internal/ipc`,
`tasks.go`), on top of the task store of `internal/task`. `maestro-svc`
wires `ipc.Server.Tasks` to a `task.Store` on its state database; a
server without a task store answers every task route with `not_found`.

## Routes

Every response uses the common envelope (`request_id`, then `data` or
`error`). Every route names its project: `project_id` in the query for
reads, in the JSON body for mutations. Mutations go through the
persisted `Idempotency-Key` middleware, so a client retry replays the
stored response instead of creating a second task or applying a second
transition.

| Route | Body | Success |
| --- | --- | --- |
| `GET /v1/tasks?project_id=` | — | `200`, the project's tasks, oldest first |
| `POST /v1/tasks` | `{"project_id", "description"}` | `201`, the created task |
| `GET /v1/tasks/{id}?project_id=` | — | `200`, the task with its `events` |
| `POST /v1/tasks/{id}/cancel` | `{"project_id"}` | `200`, the cancelled task |
| `POST /v1/tasks/{id}/resume` | `{"project_id"}` | `200`, the resumed task |

Bodies refuse unknown fields. Errors map onto the stable codes:

| Code | Status | Cause |
| --- | --- | --- |
| `invalid_request` | 400 | missing `project_id` or key, malformed body, empty or oversized description, invalid project configuration |
| `not_found` | 404 | unknown project, or a task unknown in that project (a task of another project is not disclosed) |
| `conflict` | 409 | missing user repository, or a transition the workflow refuses (`task.ErrTransition`, `task.ErrGuard`) |

## Creation

`POST /v1/tasks` resolves the project in the `worktree.Store`, then:

1. takes the configuration snapshot (`config.Take`) of the working tree
   of the user repository — the parent of the recorded common Git
   directory; a bare repository has none and is refused — and persists
   it (`config.Persist`), which yields the `config_id`;
2. reads `refs/heads/maestro/integration` in the canonical repository as
   `task_base_sha`;
3. calls `task.Store.Create(projectID, description, configID, baseSHA)`,
   which validates the request (`task.ErrInvalid`: no project, blank,
   non-UTF-8 or over `task.MaxDescription` bytes), trims the
   description and stores the `NEW` task with its `create` event.

Schema migration 7 adds the `project_id` and `description` columns to
`tasks`, with an index on `(project_id, created_at)` behind
`task.Store.List`. Tasks stored before it keep empty values.

## Cancel and resume

Both are plain events of the task workflow, applied with the store's
compare-and-set `Transition`, so they serialize against any concurrent
transition of the same task. The guard inputs are explicit:

- `cancel` passes `Published: false`, `ProcessesStopped: true` and
  `OperationSettled: true`: no publication, process or integration
  operation is attached to a task by the daemon. The transition clears
  the continuation and records `cancelled by the user`.
- `resume` passes `CauseLifted: true` (the client asserts it) and
  `Reconciled: true` (no interrupted step holds effects to reconcile).
  The workflow guard also validates the stored continuation: a
  `resume_state` that is empty, unknown, terminal or `BLOCKED` is
  refused with `task.ErrGuard` and the task stays blocked.

## Client

`internal/cli/task.go` implements `maestro task new|show|list|cancel|resume`
over the socket. The project comes from `--project`, or from
`worktree.Store.Find` on the working directory; outside a repository, or
in an unregistered one, the command fails before calling the daemon.
Each mutation sends a fresh random `Idempotency-Key`. Flags may appear
before or after the positional argument.

The tests run the routes against `httptest` servers on migrated SQLite
databases and temporary Git repositories (`internal/ipc/tasks_test.go`),
and the commands against an in-process server on a Unix socket
(`internal/cli/cli_task_test.go`).
