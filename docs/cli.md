# Command reference

`maestro` is the command-line client of Maestro. Most commands talk to
the [daemon](daemon.md) over its Unix socket. The binary describes
itself:

```console
$ maestro             # or: maestro help, maestro --help
$ maestro help task   # usage and flags of one command
$ maestro --version
```

An unknown command is refused and points to `maestro help`.

Commands that work on a project use the project of the repository
holding the current directory, or the one named by `--project <id>`;
outside a registered repository and without `--project`, they fail.
Commands that reach the daemon accept `--socket <path>`.

## Projects

| Command | What it does |
| --- | --- |
| `maestro init [path]` | Registers the repository containing `path` (default: the current directory) and imports it into a private clone. [Project initialization](initialization.md) |
| `maestro project list` | Lists the registered projects, their repository and state. |
| `maestro project relocate <id> <path>` | Points a project at the new location of its moved repository. |

## Daemon

| Command | What it does |
| --- | --- |
| `maestro daemon start [--binary <path>]` | Starts `maestro-svc` in the background, found next to `maestro` or on `PATH`. [Running the daemon](daemon.md) |
| `maestro daemon stop` | Stops the daemon after draining its requests. |
| `maestro daemon status` | Tells whether the daemon answers on its socket. |
| `maestro status` | Shows the daemon and its machine-wide capacity. |

## Tasks

| Command | What it does |
| --- | --- |
| `maestro task new "<description>"` | Creates a task on the project's integration head, frozen with its configuration. [Managing tasks](tasks.md) |
| `maestro task show <id>` | Shows a task and its history. |
| `maestro task list` | Lists the project's tasks. |
| `maestro task cancel <id>` | Cancels a task. |
| `maestro task resume <id>` | Resumes a blocked task at its stored continuation. |
| `maestro task config update <id>` | Adopts the project's current configuration for a task, explicitly. |

## Workers

| Command | What it does |
| --- | --- |
| `maestro worker list` | Lists the project's workers by name: agent, driver, state and current assignment. [Worker registry](development/workers.md) |
| `maestro worker show <name>` | Shows a worker, its current assignment and its recent events. |
| `maestro worker start <agent> [--count <n>]` | Starts `n` workers (default 1) of an agent in confined sessions, bounded by the daemon's session ceiling, and waits until each one is `IDLE` or `FAILED`. [Starting and stopping](development/workers.md#starting-and-stopping) |
| `maestro worker stop <name>` | Drains a worker and terminates its session; it ends `STOPPED`. |
| `maestro pause <worker>` | Pauses a worker: the turn it runs is interrupted and its task blocked, its agent and every process it started are stopped, and it ends `PAUSED`, keeping its session for its resume. [Pausing and resuming](development/workers.md#pausing-and-resuming) |
| `maestro resume <worker>` | Resumes a paused worker's exact conversation, read only, and waits until it is `IDLE` or `FAILED`; a task its pause blocked waits for `maestro task resume`. |

## Your repository

| Command | What it does |
| --- | --- |
| `maestro sync --from <branch>` | Imports the commit your branch points to, runs the configured tests, and advances the project's integration. [Syncing from your repository](sync.md) |
| `maestro publish` | Fast-forwards `maestro/integration` in your repository. [Publishing the integration](publish.md) |

## Inspection

| Command | What it does |
| --- | --- |
| `maestro tui [--project <id>] [--interval <duration>]` | Opens the live dashboard. [Terminal dashboard](tui.md) |
| `maestro worktree list [--path <repository>]` | Lists the task worktrees of a project. [Inspecting worktrees](worktrees.md) |
| `maestro diff <worker> [task] [--path <repository>]` | Shows the pending changes of a worker's task worktree. |
| `maestro attach <session>` | Attaches the terminal to a session; `Ctrl-]` detaches. |
| `maestro agent doctor` | Checks the sandbox and the installed agent versions. [Checking agents](agents.md) |

## Data

| Command | What it does |
| --- | --- |
| `maestro backup <destination>` | Snapshots the data directory; the daemon must be stopped. [Backup, restore and cleanup](maintenance.md) |
| `maestro restore <backup> <new-data-directory>` | Restores a backup into a new data directory. |
| `maestro gc --dry-run` / `--apply` | Previews, then removes superseded data. |
