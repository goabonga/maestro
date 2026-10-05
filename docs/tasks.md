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

Refusals are reported with the daemon's error code — `not_found` for an
unknown task or project, `conflict` for a refused transition,
`invalid_request` for a malformed request.
