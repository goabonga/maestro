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

All commands accept `--socket`; the default lives under
`$XDG_RUNTIME_DIR/maestro/svc.sock`, or a per-user directory in the
system temporary directory when `XDG_RUNTIME_DIR` is unset.
