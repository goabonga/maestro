# Project structure

maestro is a Go command-line client and daemon, with Python tooling for CI,
releases and documentation. This page describes where code lives, which
component owns it, and how the pieces depend on each other.

## Repository layout

| Path | Contents |
| --- | --- |
| `cmd/cli/` | Entry point of the `maestro` command. Only wires flags, signals and `internal/cli`. |
| `cmd/svc/` | Entry point of the `maestro-svc` daemon: flags, user lock, store migration, socket serving. |
| `internal/cli/` | Command logic: `--version`, help and the `init`, `status`, `daemon`, `project`, `worktree list`, `diff`, `attach`, `agent doctor`, `task`, `worker`, `tui`, `sync`, `publish`, `backup`, `restore` and `gc` subcommands. |
| `internal/transport/` | Unix socket listener and client, HTTP serving with graceful shutdown, `/healthz`. |
| `internal/worktree/` | Project store: the data directory, the canonical repository import and the private per-worker clones. |
| `internal/state/` | Durable store: the SQLite database, its ordered migrations, the advisory file locks, backup/restore and GC. |
| `internal/ipc/` | The daemon's versioned JSON API: envelope, bounded bodies, persisted idempotency keys. |
| `internal/scheduler/` | Global capacity: atomic slot reservations for sessions, test runs and commands. |
| `internal/launcher/` | Execution confinement: Bubblewrap namespaces, inherited limits, supervised groups. |
| `internal/session/` | Persistent PTY sessions: bounded output, resize, stop, exit reconciliation, permissions epochs. |
| `internal/fixture/` | PTY fixtures: record a session, strip secrets, replay it with expected outcomes. |
| `internal/agent/` | Versioned registry, host doctor, durable native Claude/Codex identities and PTY turn detection. |
| `internal/handoff/` | Handoff contracts: the versioned envelope, typed payloads, and their checks against the assignment and Git. |
| `internal/config/` | Layered project configuration and the immutable snapshots identified by `config_id`. |
| `internal/turn/` | Turn lifecycle: explicit transitions, persisted timeouts, retries as linked attempts ([details](turns.md)). |
| `internal/provision/` | Agent provisioning: native instruction files ([details](instructions.md)), MCP translation ([details](mcp-translation.md)), runtime path control ([details](runtime-paths.md)). |
| `internal/task/` | Task workflow: the authoritative transition table, guards, durable continuations ([details](tasks.md)). |
| `internal/testrun/` | Test runner: configured argv commands in a sandbox on a private clone of one SHA ([details](test-runner.md)). |
| `internal/budget/` | Durable budgets: atomic turn reservations against task and agent caps, active and calendar time ([details](budgets.md)). |
| `internal/integration/` | Integration: the operations journal and durable result references ([details](operations.md)), source chain validation ([details](source-chain.md)), the candidate build ([details](integration-candidate.md)), publication and finalization ([details](publication.md)), sync ([details](sync.md)) publication to the user repository ([details](user-publication.md)), conflict resolution ([details](conflict-resolution.md)) and the startup recovery of operations ([details](recovery.md)). |
| `internal/attach/` | The raw-terminal attach client shared by the CLI and the dashboard ([details](attach.md)). |
| `internal/tui/` | The terminal dashboard over the daemon API ([details](tui.md)). |
| `internal/worker/` | Worker registry: identities, the lifecycle transition table and assignments, persisted with their events, and the supervisor starting and stopping the workers' confined agent sessions ([details](workers.md)). |
| `scripts/` | Python project (`maestro-scripts`): CI detection, release, Dependabot rewrite, signing, licence headers. Has its own uv lockfile and pytest suite. |
| `docs/` | Source of the documentation site, built by Zensical. `development/` holds contributor pages. |
| `assets/maestro.svg` | Canonical logo. `make icons` derives `docs/maestro.svg` and `docs/favicon.ico` from it. |
| `.github/` | CI workflow, Dependabot configuration, issue and pull request templates. |
| `multicz.toml` | Release components: their paths, version files and changelogs. |
| `zensical.toml` | Site configuration, navigation and the version table read by the docs. |
| `Makefile` | Entry points for every local check and build. |

Go dependencies are pinned by `go.mod` and kept minimal: the standard
library plus `modernc.org/sqlite` (SQLite without CGO),
`github.com/creack/pty` (PTY allocation), `github.com/BurntSushi/toml`
(project configuration), and `github.com/charmbracelet/bubbletea` with
`github.com/charmbracelet/lipgloss` (terminal dashboard).

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
    workers/
        <worker>/worktree/          worktree a worker's session starts in
        <worker>/home/              private HOME of its agent
        <worker>/supervisor/        native session identity, daemon only
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
`launcher.Survivors` lists the processes still working in a directory,
by their working directory as the host sees it; it only observes, for
the startup reconciliation of workers ([details](workers.md#startup-reconciliation)).

## PTY sessions

Each agent process runs on a real terminal the daemon owns, inside the
confinement above. A session keeps only the last bytes of output in a
bounded ring (1 MiB by default) while counting everything it ever saw,
propagates terminal resizes to the PTY, and reconciles its state when
the process exits on its own — phase, exit code, output totals. An
explicit stop signals the group and returns only once the leader is
reaped, including a repeated stop while termination is still in progress.
The master uses pollable I/O so closing it releases a stalled input writer;
resizes preserve that mode. Reaping tears the group's PID namespace down;
writes and resizes
on a finished session are refused, and stopping it again is a no-op.

Live output reaches clients through bounded subscriptions: the PTY
master is always drained into the ring first, and a subscriber whose
queue overflows is disconnected rather than allowed to block the read.
Subscribers learn that a session ended only once its exit is
reconciled.

Clients stream a session over a dedicated connection, never mixed with
RPC responses: `GET /v1/sessions/{id}/stream` with
`Upgrade: maestro-stream/1` switches the connection to typed frames —
a one-byte type (`o` output, `i` input, `r` resize, `d` detach,
`e` error), a big-endian 32-bit length and a payload bounded to
64 KiB; unknown types and oversized lengths are refused. Every frame
write has a deadline: a client that stops reading is hung up on, and
the session keeps running. A detach frame ends the stream only; the
session survives it.

`maestro attach <session>` is the client side of that stream. It
upgrades the connection first and only then switches the user's
terminal to raw mode, so a refused attach never touches the terminal.
It relays keystrokes as input frames and output frames to the screen,
sends the terminal size on attach and again on every `SIGWINCH`, and
leaves on `Ctrl-]` with a detach frame. The previous terminal settings
are restored by a deferred call on every exit path — detach, session
end, connection error. The daemon does not create sessions yet; the
command is exercised against fixture sessions in the tests.

## Permissions epochs

`session.NewProfile` freezes a permissions epoch, role, source revision,
PTY configuration, Git metadata directory and optional handoff directory.
The profile owns its slices and environment map; changing an input or an
exported copy cannot change its mounts. Paths are resolved before admission,
and overlapping writable/protected mounts are refused, including symlink
aliases. The caller supplies the actual Git metadata path, rather than a
`.git` path reconstructed from a worktree name, and materializes the revision
it declares before constructing the profile.

Coding grants source and Git writes while extra instruction mounts remain
read-only. Review protects both sources and Git. Repair protects them too
and requires its own handoff directory: that directory alone is writable
inside the source tree. Explicit private state and cache mounts may remain
writable outside the protected paths. Nested protections are mounted after
the working directory so that its bind cannot hide them.

`session.StartEpoch` starts a confined PTY for a validated profile. Session
startup waits for a private FIFO acknowledgement from a fixed Maestro
wrapper running inside the sandbox, after mount and limit setup and before
exec of the agent. Bubblewrap's child-PID report arrives too early to prove
mount setup; application text is not proof either. Failure or a five-second
confirmation timeout
refuses admission and stops the group. The epoch is `active` only after this
confirmation; driver readiness and revision verification remain the caller's
responsibility.

`Epoch.End(reason, grace, reconcile)` permanently fences input to the old
PTY, including clients retaining its pointer, then stops and reaps the whole
PID namespace. Only then does the required reconciler inspect stable Git and
handoffs and persist its proofs. Success changes the epoch to `revoked` and
authorizes release of the assignment; a failed stop or reconciliation leaves
it `blocked`, with the error exposed in its state. An unreconciled boundary
can be retried, but a successful boundary does not repeat its callback.
Callbacks may inspect state, but must not recursively request another epoch
transition. Completion, interruption, pause, role change and human detach
use this same barrier. A reconciler that sees human code changes must
invalidate the corresponding test and review proofs before returning.

Bubblewrap does not change mounts in place here: the implementation always
uses the stop-and-recreate strategy. `Epoch.ConfirmNativeID` binds the exact
conversation ID that a driver has chosen or observed; it refuses rebinding.
After a successful boundary, `Epoch.Restart` requires a strictly newer
profile and a driver that constructs the resume argv from that confirmed ID.
An absent ID, a driver error, stale epoch or an invalid replacement cannot
start a new conversation implicitly. A replacement gets a new PTY and must
confirm its sandbox again; the old PTY stays fenced. Live-output clients
subscribe to the new PTY explicitly.

These are session primitives, exercised with confined fixture processes.
The daemon uses them to start and stop workers, with native agent ID
discovery ([Worker registry](workers.md#starting-and-stopping)); it does
not yet wire worker assignments, Git/handoff persistence or human-pilot
ownership into them. A caller must
supply the reconciler and the exact-ID resume driver; there is no default
no-op reconciler or latest-session fallback. Tests prove descendant writes
cease before reconciliation, read-only review/repair, writable private state
and handoffs, immutable instructions, refusal of changed mounts and failure
paths, with the race detector.

## PTY fixtures

Driver behavior is tested against recorded sessions, never against the
real agent CLIs. `internal/fixture` records a live session — output
from a subscription, input and resizes sent through the recorder, the
final exit — as JSON Lines: a header (agent, version, case, terminal
size, expected driver states at given offsets) and one timed event per
line. Recordings stay local until sanitized: the sanitizer coalesces
output bursts so no secret straddles two chunks, strips exact secret
values, maps host paths to placeholders and replaces known secret
shapes (API keys, bearer and GitHub tokens, AWS key ids, e-mail
addresses). Saving refuses a fixture that is not marked sanitized or in
which a secret shape is still detectable across event boundaries.
Replay feeds a fixture to a sink on the recorded clock, instantly or in
scaled real time, so a detector can measure idle periods as recorded.
Real sessions recorded with util-linux `script` are imported with
`ImportScript`; the resulting fixtures and verdicts are listed in the
[driver validation matrix](validation.md).

## Handoff contracts

Agents never hand results over through terminal text. Every result is a
JSON document: a common envelope (`schema_version`, artifact, kind,
task, turn, attempt, worker and configuration identifiers, input
artifacts, and for code roles the task base and source head) around a
payload typed by its kind — `PLAN`, `IMPLEMENTATION`, `REVIEW`,
`TEST_REPORT`, `FIX_REQUEST`, `CONFLICT_RESOLUTION`. Decoding is
strict: unknown fields, trailing data, an unsupported schema version,
malformed identifiers or object ids are refused, and each payload is
checked against its kind (plan sections in order, review verdict and
issues, commit ids). The envelope must then repeat exactly the
assignment it answers — a document from another task, turn, attempt,
worker or configuration never satisfies a turn — and an implementation's
commit list must be exactly Git's first-parent chain from the task base
to the source head.

On disk, an agent writes `.maestro/handoff/<turn>/<attempt>.json.tmp`
in its worktree and renames it to `.json`, so a partial file never has
the final name. Maestro reads only that exact path, built from
validated identifiers: every component must be a real entry (no
symbolic link, even one pointing inside the worktree), the file is
opened without following links and without blocking (a named pipe
planted there cannot stall the daemon), must be a regular file of at
most 1 MiB, and must name the current attempt. A missing document is
reported as such, distinct from an invalid one. An accepted document is
stored in SQLite with its receipt time and the SHA-256 of its content,
once: a second document under the same artifact id is refused, never
overwritten.

A missing or invalid document gets one bounded second chance. Maestro
fingerprints the worktree — HEAD, index, and every change to tracked
or untracked files, the handoff directory aside — and asks for a format
repair in a new attempt. The repair is accepted only if the fingerprint
is unchanged and its document is valid: a new or moved commit, a staged
change, an edited or added file, a second missing or invalid document,
or an error that is neither missing nor invalid blocks the task instead.

## Configuration snapshots

`internal/config` loads a project's configuration in layers, the
stronger replacing the weaker key by key: Maestro's defaults, the
versioned `.maestro.toml`, then the unversioned `.maestro.local.toml`.
It covers the project budgets (`max_turns_per_task`, `turn_timeout`,
`input_wait_timeout`, `task_timeout`), named agents (driver, base URL,
model, `api_key_env`, per-agent turn limits) and MCP servers (command,
arguments, scope `shared`, `agent:<name>` or `role:<name>`). Unknown
keys are refused. Secrets never enter configuration: a key named like a
secret (except the `*_env` keys naming a variable) or a value shaped
like one is refused in either file.

A snapshot freezes the effective configuration with the instruction
files under `maestro/` (regular UTF-8 files only, bounded, no secret
shape), the handoff contract version and the validated drivers. Its
`config_id` is the SHA-256 of its canonical form, so equal snapshots
share one id; it is stored once in SQLite, and loading it checks that
the content still hashes to that id. Tasks will carry this id so that a
file changed on disk never changes a running task.

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
| `make build` | Builds `bin/maestro` and `bin/maestro-svc` |
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

The suite is safe to run from a command spawned by Git, such as
`git rebase --exec 'make go-check'`, which exports `GIT_DIR` and its siblings.
Every package whose tests run Git clears all inherited `GIT_*` variables in its
`TestMain` and ignores the global and system Git configuration, so tests only
touch the temporary repositories they create. Maestro's own Git commands that
target an explicit directory likewise drop the variables through which Git
locates another repository (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`,
`GIT_COMMON_DIR` and the rest of `git rev-parse --local-env-vars`).
