<h1 align="center">
  <img src="docs/maestro.svg" alt="maestro" width="120" /><br/>
  maestro
</h1>

<p align="center">
  <em>A multi-model orchestrator for software development.</em>
</p>

<p align="center">
  <a href="https://github.com/goabonga/maestro/actions/workflows/ci.yml?query=branch%3Amain"><img src="https://github.com/goabonga/maestro/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI"/></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-blue.svg" alt="License: MIT"/></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/go-1.26%2B-00ADD8.svg?logo=go" alt="Go"/></a>
  <a href="https://github.com/goabonga/multicz"><img src="https://img.shields.io/badge/versioning-multicz-blue.svg" alt="multicz"/></a>
</p>

Maestro is a local orchestrator for coding agents: a `maestro` command-line
client and a `maestro-svc` daemon that keep each project's work in private
Git repositories and worktrees, so the repository you work in is never
modified behind your back.

## What it does today

- **Registers a repository** (`maestro init`): Maestro imports it into a
  private canonical clone with fully copied objects, and leaves your
  repository untouched. `maestro project list` and `maestro project
  relocate` follow a repository across moves.
  [Project initialization](docs/initialization.md)
- **Runs one daemon per user** (`maestro daemon start|stop|status`) on a
  private Unix socket; `maestro status` shows the machine-wide capacity it
  enforces. At each start it reconciles the recorded workers with the
  sessions lost by the previous daemon. [Running the daemon](docs/daemon.md)
- **Records tasks** (`maestro task new|show|list|cancel|resume` and
  `maestro task config update`): each task is frozen with the
  configuration and instruction files it was created with, starts from
  the project's integration head on its own branch, and moves only
  through the transitions Maestro allows. [Managing tasks](docs/tasks.md)
- **Shows its workers** (`maestro worker list|show`): each worker of a
  project with its agent, driver, state, current assignment and recent
  events. [Command reference](docs/cli.md#workers)
- **Shows a live dashboard** (`maestro tui`) of the daemon's status and
  capacity, the projects and their tasks with their history.
  [Terminal dashboard](docs/tui.md)
- **Syncs from your repository** (`maestro sync --from <branch>`): it
  imports the commit your branch points to, runs the project's configured
  tests on it, and advances the project's private integration branch only
  if that commit descends from it. [Syncing from your repository](docs/sync.md)
- **Publishes the integration** (`maestro publish`): it fast-forwards the
  `maestro/integration` branch of your repository, never your current
  branch or worktree. [Publishing the integration](docs/publish.md)
- **Shows its worktrees** (`maestro worktree list`, `maestro diff`) without
  entering them. [Inspecting worktrees](docs/worktrees.md)
- **Checks a host** (`maestro agent doctor`): the sandbox it confines
  agents in, and which installed Claude Code and Codex versions it has
  validated. [Checking agents](docs/agents.md)
- **Backs up and cleans its data** (`maestro backup`, `maestro restore`,
  `maestro gc`). [Backup, restore and cleanup](docs/maintenance.md)

## Requirements

- Linux with unprivileged user namespaces, Bubblewrap (`bwrap`) and
  `prlimit` (util-linux).
- Git.
- Go 1.26 or later to build from source.

## Build

```console
$ make build
$ ./bin/maestro daemon start
$ ./bin/maestro agent doctor
```

`make build` writes `bin/maestro` and `bin/maestro-svc`; `maestro daemon
start` finds the daemon next to the client.

`./bin/maestro help` lists every command; `./bin/maestro help <command>`
describes one.

## Documentation

The [command reference](docs/cli.md) lists every command, and the
[documentation](docs/index.md) covers each of them; the
[development pages](docs/development/structure.md) describe the
architecture, the checks and the release process.

## License

[MIT](LICENSE)
