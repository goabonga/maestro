# Worker registry

A worker is one work slot of a project: an agent, its driver and a
private repository. Package `internal/worker` registers workers, holds
their lifecycle state machine, and persists both in the `workers` and
`worker_events` tables of the state database (schema version 10). It
records state only: it starts no process and calls no agent.

## Identity

`Store.Register` stores a worker with:

- its project ID and its name, unique in the project; the name is a
  safe identifier (lowercase letters and digits, single hyphens between
  them, at most 64 bytes), so it is also a path component;
- its agent, the agent's kind and the driver name;
- the path of its private repository, `worktree.Project.WorkerRepository`.

A name already registered in the project fails with `worker.ErrExists`;
an invalid name or a missing field fails with `worker.ErrInvalid`. The
same name may exist in another project. Registration is recorded as a
`register` event from the empty state to `STOPPED`.

`Store.Get` reads one worker, `Store.List` the workers of a project by
name, and `Store.Events` the recorded events of a worker, oldest first.

## States

| State | Meaning |
| --- | --- |
| `STOPPED` | Registered, no session. |
| `STARTING` | Its session is being started. |
| `IDLE` | Ready, without an assignment. |
| `BUSY` | Running its assigned turn. |
| `WAITING_INPUT` | Its turn waits for an input; the assignment is kept. |
| `ATTACHED` | A human drives the session exclusively. |
| `PAUSED` | Paused; it takes no new assignment. |
| `DRAINING` | Leaving the pool; it takes no new assignment. |
| `FAILED` | Stopped by an error or an inconsistency. |

Every state but `STOPPED` and `FAILED` is active.

## Transition table

The table is the single authority. Each row is an event accepted in a
set of states, with a guard and an effect. An event a state does not
list fails with `worker.ErrTransition`; an unmet guard fails with
`worker.ErrGuard`. Neither leaves a trace.

| From | Event | Guard | Effect and next state |
| --- | --- | --- | --- |
| `STOPPED` | `start` | a session slot is reserved | `STARTING` |
| `STARTING` | `ready` | the session is ready and its profile confirmed | `IDLE` |
| `IDLE` | `assign` | the assignment and its turn are persisted | take the assignment; `BUSY` |
| `BUSY` | `accept-turn` | the turn's rights are revoked and its effects reconciled | release the assignment; `IDLE` |
| `BUSY` | `request-input` | the input request is recognized | keep the assignment; `WAITING_INPUT` |
| `IDLE`, `BUSY`, `WAITING_INPUT` | `attach` | the turn is quiescent or its interruption confirmed | `ATTACHED` |
| `ATTACHED` | `detach`, `connection-lost` | none | by the continuation (below) |
| `IDLE`, `BUSY`, `WAITING_INPUT` | `pause` | the interruption and the stop of the descendants are confirmed | release the assignment; `PAUSED` |
| `PAUSED` | `resume` | the profile and the continuation are validated | `STARTING` |
| `IDLE`, `BUSY`, `WAITING_INPUT`, `ATTACHED`, `PAUSED` | `scale-down` | none | keep the assignment; `DRAINING` |
| `DRAINING` | `drained` | the turn is finished or cancelled, no client is attached and the descendants are stopped | release the assignment; `STOPPED` |
| every state but `STOPPED` | `stop` | the effects are reconciled | release the assignment; `STOPPED` |
| every active state | `fail` | a reason is given | keep the assignment; `FAILED` |
| `FAILED` | `recover` | the cause is lifted and no descendant remains | release the assignment; `STOPPED` |

A detached worker is reconciled by its continuation: without an
assignment it returns to `IDLE`; with one and a valid continuation it
returns to `WAITING_INPUT`; when the effects are not reconciled, or the
continuation of its assignment is not valid, it goes to `FAILED` with
the cause in its reason.

The guards read their facts from `worker.Guard`: capacity, sessions,
turns, process supervision and attach report them with the event. A
fact left unset does not hold.

## Assignments

An assignment links a worker to one turn of a task for a role
(`planning`, `implementation`, `review` or `correction`). A worker
holds at most one: it takes one only from `IDLE`, and the database
refuses an assignment on a `STOPPED`, `STARTING`, `IDLE` or `PAUSED`
worker and requires one on a `BUSY` or `WAITING_INPUT` worker.

An assignment always names a stored turn: `Store.Transition` checks
that the turn exists, belongs to the assigned task and runs the
worker's agent, and fails with `worker.ErrInvalid` otherwise. A turn is
assigned to one worker at most; a second worker fails with
`worker.ErrAssigned`. Since an assignment needs a turn, a task that
waits without a live turn reserves no worker, and an accepted turn
frees its worker at once.

A failed worker keeps its assignment, so the concerned task is known;
`recover` or `stop` releases it.

## Persistence

`Store.Transition` evaluates the event, then writes the worker and its
`worker_events` row in one transaction. The update applies only if the
worker is still at the state and version it read, so a concurrent
transition makes the later one fail with `ErrTransition` instead of
overwriting it. Each event row records the task and turn of the
assignment the worker held or took.

## Daemon API

The daemon serves the registry read-only on its versioned JSON API
(`internal/ipc`, `workers.go`). `maestro-svc` wires `ipc.Server.Workers`
to a `worker.Store` on its state database; a server without one answers
every worker route with `not_found`. Like the task routes, both name
their project with `project_id` in the query, and a worker of another
project is not disclosed.

| Route | Success |
| --- | --- |
| `GET /v1/workers?project_id=` | `200`, the project's workers, by name |
| `GET /v1/workers/{name}?project_id=` | `200`, the worker with its `events` |

A worker carries its `name`, `agent`, `agent_kind`, `driver`,
`repository`, `state`, `version`, `reason`, `created_at`, `updated_at`
and, when it holds one, its `assignment` (`task_id`, `role`,
`turn_id`). The events of `show` are the most recent ones, at most
`ipc.RecentWorkerEvents` (20), oldest first; each names the task and
turn it is about, when there is one.

| Code | Status | Cause |
| --- | --- | --- |
| `invalid_request` | 400 | missing `project_id` |
| `not_found` | 404 | unknown project, or a worker unknown in that project |

`maestro worker list` and `maestro worker show <name>` print these
documents on the project of the current repository, or the one named
by `--project <id>`.
