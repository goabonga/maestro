# Project structure

maestro is a Go command-line client and daemon, with Python tooling for CI,
releases and documentation. This page describes where code lives, which
component owns it, and how the pieces depend on each other.

## Repository layout

| Path | Contents |
| --- | --- |
| `cmd/cli/` | Entry point of the `maestro` command. Only wires flags, signals and `internal/cli`. |
| `cmd/svc/` | Entry point of the `maestro-svc` daemon: flags, user lock, store migration, socket serving. |
| `internal/cli/` | Command logic: `--version`, help and the `init`, `status`, `daemon`, `project`, `worktree list`, `diff`, `backup`, `restore` and `gc` subcommands. |
| `internal/transport/` | Unix socket listener and client, HTTP serving with graceful shutdown, `/healthz`. |
| `internal/worktree/` | Project store: the data directory, the canonical repository import and the private per-worker clones. |
| `internal/state/` | Durable store: the SQLite database, its ordered migrations, the advisory file locks, backup/restore and GC. |
| `internal/ipc/` | The daemon's versioned JSON API: envelope, bounded bodies, persisted idempotency keys. |
| `internal/scheduler/` | Global capacity: atomic slot reservations for sessions, test runs and commands. |
| `internal/launcher/` | Execution confinement: Bubblewrap namespaces, inherited limits, supervised groups. |
| `scripts/` | Python project (`maestro-scripts`): CI detection, release, Dependabot rewrite, signing, licence headers. Has its own uv lockfile and pytest suite. |
| `docs/` | Source of the documentation site, built by Zensical. `development/` holds contributor pages. |
| `assets/maestro.svg` | Canonical logo. `make icons` derives `docs/maestro.svg` and `docs/favicon.ico` from it. |
| `.github/` | CI workflow, Dependabot configuration, issue and pull request templates. |
| `multicz.toml` | Release components: their paths, version files and changelogs. |
| `zensical.toml` | Site configuration, navigation and the version table read by the docs. |
| `Makefile` | Entry points for every local check and build. |

Go dependencies are pinned by `go.mod` and kept minimal: the standard
library plus `modernc.org/sqlite` (SQLite without CGO).

## Components

Each component is versioned independently by multicz from Conventional Commits.
A change under a component's paths bumps that component.

| Component | Path | Produces | Version file |
| --- | --- | --- | --- |
| `maestro` | `cmd/cli`, `internal/cli` | `maestro` binary | `cmd/cli/version.go` |
| `maestro-svc` | `cmd/svc`, `internal/transport` | `maestro-svc` daemon | `cmd/svc/version.go` |
| `maestro-scripts` | `scripts/` | Python automation, not shipped | `scripts/pyproject.toml` |
| `maestro-docs` | `docs/`, `zensical.toml` | documentation site | `zensical.toml` |

`maestro-docs` depends on the other components, so a release also refreshes the
version table.

## Dependencies between packages

- `cmd/*` only assemble a process. Logic stays in `internal/`.
- `internal/cli` imports `internal/transport` to reach the daemon socket. A change
  in `internal/transport` therefore affects both `maestro` and `maestro-svc`.
- CI derives affected Go components from the Go import graph, so an internal
  change runs the checks of every binary that imports it.

## Runtime layout

The daemon and the CLI talk over a Unix socket, `$XDG_RUNTIME_DIR/maestro/svc.sock`,
or a per-user path under the temporary directory when `XDG_RUNTIME_DIR` is unset.
The socket directory is `0700` and the socket is `0600`, so only the owning user can
connect. Pass `--socket` to either command to use another path.

Beside `GET /healthz`, the daemon serves a versioned JSON API under
`/v1/`. Every response carries a stable envelope: `request_id` (echoed
from the `X-Request-Id` header or generated), then `data` or `error`
with a stable `code`. Request bodies are bounded (1 MiB). Mutations
require an `Idempotency-Key` header, persisted in SQLite with a hash of
the request and the stored response: a retry with the same key and body
replays the stored response without re-running the handler, the same
key with a different body is a conflict, and a claim whose response was
never stored reports in progress. Current routes: `GET /v1/projects`
(list) and `POST /v1/projects` (register a repository, as `maestro
init` does). On start the daemon migrates the store under the user
lock before binding the socket.

Project data lives in the [data directory](../initialization.md#data-directory),
one directory per registered project:

```
<data>/projects/<project-id>/
    project.json                    project metadata
    repository.git/                 private canonical repository
    worker-repositories/
        <worker>.git/               private clone of one worker
    worktrees/
        <worker>/tasks/<task-id>/   worktree of one task
    quarantine/
        <entry>/                    abandoned dirty worktree and its record
```

Every repository under a project is a bare clone with fully copied
objects: no hardlinks and no alternates, so no repository can write into
another one through Git metadata. A worker repository keeps no remote;
the daemon moves commits explicitly — it provisions a worker branch from
the canonical integration head and imports a worker branch back under
`refs/maestro/workers/<worker>/`, never the other way around.

Each task works in its own worktree of the worker repository, on the
branch `maestro/task-<id>`. The branch belongs to the task, not to the
worker: it keeps its commits when its worktree is removed and when the
task resumes, so a later checkout carries on from the kept head. Removal
only goes through `git worktree remove` followed by `prune`, and only on
a clean worktree, verified through `git rev-parse` rather than a
reconstructed path. A dirty worktree is never force-deleted: abandoning
one moves it under `quarantine/` with a record of its branch and original
path, still registered with its worker repository.

## Execution confinement

Agent, test and command processes never run bare. `internal/launcher`
probes the host once — Bubblewrap and `prlimit` must exist and a canary
must run inside the full namespace set — and refuses to start anything
when the host cannot confine; there is no permissive fallback. Each
group runs under user, mount, PID, IPC, UTS and (by default) network
namespaces: a read-only system, a namespace-local `/proc`, a private
`/dev` and `/tmp`, only the explicitly bound paths visible, a cleared
environment refilled from an allow-list, and CPU, address-space and
process limits set inside the user namespace so every descendant
inherits them. The group's processes live in their own PID namespace,
so stopping the leader (SIGTERM, then SIGKILL after the grace period)
lets the kernel tear the whole namespace down: descendant termination
is a kernel guarantee, not a best effort. The launcher tests skip on a
host that cannot confine; CI installs the tooling to run them.

## Durable store

All durable state lives in one SQLite database opened with WAL
journaling, `synchronous=FULL` (proofs must survive a crash), enforced
foreign keys and owner-only permissions. The schema is versioned by
ordered, consecutive migrations: each step runs in its own transaction,
a database holding data is backed up (`VACUUM INTO`) before migrating,
and an older daemon refuses a schema newer than it knows — there is no
automatic downgrade. Exclusive advisory file locks (`flock`) guard
cross-process critical sections; the kernel drops them when the holder
dies, so no PID file or lock-file existence is ever trusted.

## Local development

```console
uv tool install multicz --with multicz-go-deps-plugin
go run ./cmd/svc &
go run ./cmd/cli --version
go run ./cmd/cli status
```

`maestro status` asks `GET /v1/status` and prints the daemon identity with the
global capacity: consumption, ceilings and the per-project detail. It reports
liveness, not whether dependencies are ready. The daemon removes a stale socket left by a
crash, refuses to start if another daemon is listening, and removes its socket on
SIGINT or SIGTERM after draining requests.

## Checks

| Command | Runs |
| --- | --- |
| `make check` | Everything below, plus release validation |
| `make go-test` | Unit tests of `cmd/` and `internal/` with `go test -race` |
| `make go-check` | `make go-test`, then `go vet`, build and gosec on `cmd/` and `internal/` |
| `make scripts-check` | Byte-compilation and pytest for `scripts/` |
| `make license-check` | SPDX headers on Go, Python, TOML and YAML files |
| `make release-validate` | `multicz validate --strict` |
| `make docs` | Documentation site build |
| `make icons` | Regenerates the documentation logo and favicon |

Python tests use pytest functions and fixtures. Go tests cover HTTP routing, the
socket lifecycle, `maestro status`, graceful shutdown, and the project store
against temporary Git repositories. See
[GitHub automation](github.md) for change detection, signing and releases.
