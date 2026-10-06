# Running the daemon

`maestro-svc` is the daemon behind the `maestro` client. The CLI manages
it without a PID file: liveness is only ever decided by connecting to
its Unix socket.

```console
$ maestro daemon start
daemon running: maestro-svc 0.1.0
$ maestro daemon status
daemon running: maestro-svc 0.1.0
$ maestro daemon stop
daemon stopped
```

- `start` spawns `maestro-svc` detached — the binary next to `maestro`,
  then `$PATH`, or an explicit `--binary` — and waits until the socket
  answers. Its output goes to `svc.log` in the data directory. Starting
  a running daemon is a no-op.
- `status` connects to the socket and reports the daemon's service name
  and version, or `not running`.
- `stop` asks the daemon to shut down over its API, then waits until
  the socket stops answering. In-flight requests are drained; stopping
  a stopped daemon is a no-op.

Exactly one daemon runs per user, whatever socket path it was given: the
daemon holds an exclusive kernel lock in the data directory for its
whole life. The kernel drops that lock when the process dies, so a
crashed daemon never blocks a restart, and a stale socket file left by
a crash is detected by a failed connection and cleaned up under the
user lock at the next start.

## Restart

A daemon that starts holds none of the sessions of the previous one:
before it answers any request, it reconciles the workers it has
recorded. It never signals a process and never resumes a conversation
on its own:

- a worker that held a turn becomes `FAILED` and keeps that turn, and
  the turn's task is blocked with the worker's reason;
- a worker with processes still working in its repository becomes
  `FAILED`, so that two runtimes never share one workspace;
- any other worker that was running is `STOPPED`; a paused worker stays
  `PAUSED`.

The daemon prints one line per worker it moved or found processes for,
also written to `svc.log` when started by `maestro daemon start`:

```console
worker 6b1f6d3a-…/claude-01: BUSY -> FAILED: runtime lost with the previous daemon while holding turn … of task …
worker 6b1f6d3a-…/codex-01: IDLE -> STOPPED: runtime lost with the previous daemon
```

`maestro worker show <name>` gives the reason recorded on each worker.

## Capacity

The daemon bounds concurrent work machine-wide: `--max-sessions`
(default 3), `--max-test-jobs` (default 1) and `--max-command-jobs`
(default 3). The ceilings belong to the daemon's own configuration —
never to a project's versioned file — are validated at startup, and are
reserved atomically across projects, so they cannot be overallocated.
`maestro status` shows each ceiling, its consumption and the
per-project detail:

```console
$ maestro status
daemon: maestro-svc 0.1.0
CAPACITY  USED  LIMIT  BY PROJECT
commands  0     3
sessions  1     3      6b1f6d3a-…: 1
tests     0     1
```

All commands accept `--socket`; the default lives under
`$XDG_RUNTIME_DIR/maestro/svc.sock`, or a per-user directory in the
system temporary directory when `XDG_RUNTIME_DIR` is unset.
