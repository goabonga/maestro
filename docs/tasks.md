# Managing tasks

A task is one unit of work of a registered project: a description, the
commit it starts from and the configuration it runs with. The `maestro
task` commands talk to the running [daemon](daemon.md) over its socket.

## Selecting the project

Every task command works on one project:

- inside a repository registered with [`maestro init`](initialization.md),
  the project of that repository is used;
- `--project <id>` names a project explicitly, from anywhere;
  `maestro project list` prints the ids.

Outside a repository and without `--project`, the command fails: Maestro
never falls back to the last project used. All commands also accept
`--socket`, as the daemon commands do.

## Create

```console
$ maestro task new "add a --verbose flag to the CLI"
task created: 3f2b8c1e-…
state: NEW
branch: maestro/task-3f2b8c1e-…
base: 2c87cbbc71c5…
```

At creation the daemon freezes what the task depends on:

- the configuration snapshot of the repository as it is now —
  `.maestro.toml`, `.maestro.local.toml` and the instruction files under
  `maestro/` — stored under its `config_id`. Later edits to these files
  apply to new tasks only;
- the base commit: the head of the project's integration branch;
- the task branch, `maestro/task-<id>`.

An invalid configuration refuses the creation with its reason, and so
does a project whose repository is missing (see
`maestro project relocate`).

## List and show

`maestro task list` prints the project's tasks, oldest first, with the
first line of their description:

```console
$ maestro task list
ID            STATE      CREATED              DESCRIPTION
3f2b8c1e-…    NEW        2026-10-05 14:02:11  add a --verbose flag to the CLI
```

`maestro task show <id>` prints one task: its state, the reason and the
state to resume in when it is blocked, its branch, base commit,
configuration id, fix cycles, the full description, and every recorded
event of its history.

## Cancel and resume

`maestro task cancel <id>` moves a task to `CANCELLED`, from any state
except `DONE` and `CANCELLED`. A cancelled task stays cancelled: it can
neither be resumed nor cancelled again.

`maestro task resume <id>` resumes a `BLOCKED` task in the state its
block recorded, once you have lifted the cause shown by `task show`.
Resuming a task that is not blocked, or whose recorded state is not one
to resume in, is refused and leaves the task blocked.

```console
$ maestro task resume 3f2b8c1e-…
task 3f2b8c1e-…: NEW
```

## Progress

A task advances on the project's started workers
(`maestro worker start`, see the [command reference](cli.md#workers)).
The daemon drives the project's tasks when a task is created or
resumed, when a worker becomes `IDLE`, and every minute: oldest first,
one at a time, each on the first `IDLE` worker of the project.

A task goes through planning, implementation, tests, review and
corrections, one agent turn at a time, until it is `READY_TO_INTEGRATE`
or `BLOCKED`:

- each turn writes the role's prompt into the worker's agent session
  and waits for the agent to end its turn, then reads the handoff
  document the prompt asks for; a missing or invalid document gets one
  repair turn;
- the agent writes the task's sources only during its turns: before its
  first turn, between two turns and while the end of a turn is checked,
  its session runs with read-only sources, and the session of a worker
  that fails is ended;
- the project's configured test commands run on each implemented
  revision;
- a failed turn, a turn timeout, an agent asking for input, an invalid
  document after its repair, or an exhausted budget blocks the task with
  its reason.

Without a started worker, a task stays `NEW`. `maestro task show` lists
every step in the task's history, and `maestro worker show` the events
of a worker with the task and turn they are about. Once the cause of a
block is lifted, `maestro task resume` continues the task from the step
that stopped. A task blocked on an agent asking for input stays held by
its waiting worker: a resume takes effect only once the input wait has
expired and failed the worker, or the worker has released the
assignment.

## Update the configuration

A task keeps the configuration snapshot it was created with. `maestro
task config update <id>` asks it to adopt the project's configuration and
instruction files as they are now:

```console
$ maestro task config update 3f2b8c1e-…
task 3f2b8c1e-…: configuration updated
previous:  sha256-6eaf35ae…
config:    sha256-d572a868…
impact:    objective
state:     PLANNING

KEY                         CHANGE   IMPACT
budgets.max_turns_per_task  changed  ceiling
maestro/coder.md            added    objective
```

The daemon takes a new snapshot, lists every key that differs from the
task's own and applies the update. When nothing differs, it reports
`configuration unchanged` and changes nothing.

Each change has an impact, and the strongest one decides where the task
continues:

| Impact | Changes | The task continues from |
| --- | --- | --- |
| `objective` | instruction files, agents, MCP servers, a lowered or added budget bound | `PLANNING` at the latest |
| `verification` | test commands | `TESTING` at the latest |
| `ceiling` | a raised or removed budget ceiling: turn caps, timeouts, `wall_timeout` | its current state |

A task never moves forward: a `NEW` or `PLANNING` task stays where it
is. A `BLOCKED` task stays blocked and its resume state moves instead;
`maestro task resume` then continues from there. Raising a budget
ceiling is the way to let a task blocked on that budget go on.

An objective or verification change also drops the approvals and the
integration candidate the task held: results produced under the old
configuration no longer count for it. The turns and time the task
consumed are never reset, and its fix cycles are kept.

The update is refused for a `DONE` or `CANCELLED` task and while one of
the task's turns has not ended. Every update is recorded in the task's
history (`maestro task show`) with the old and new configuration ids
and the changes.

Refusals are reported with the daemon's error code — `not_found` for an
unknown task or project, `conflict` for a refused transition,
`invalid_request` for a malformed request.
