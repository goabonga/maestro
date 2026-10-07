# Attach client

`internal/attach` is the client side of a worker's session stream
(`GET /v1/workers/{name}/stream?project_id=` upgraded to the maestro
stream protocol), through which the user pilots the worker
([Attach](workers.md#attach)). Both `maestro attach <worker>`
(`internal/cli`) and the terminal dashboard (`internal/tui`) use it;
attach stays a dedicated terminal mode, never drawn inside a widget.

## API

- `attach.Worker(ctx, socket, project, name, attach.Terminal)` dials
  the daemon's socket and asks for the worker's stream: the daemon
  moves the worker to `ATTACHED` before upgrading. It then switches
  `Terminal.In` to raw mode, sends the terminal size, then relays
  keystrokes as input frames and output frames to `Terminal.Out` until
  the user detaches, the session ends or the connection fails; the
  daemon then hands the worker back.
- `attach.Terminal` carries the input terminal (`*os.File`), the output
  writer and an optional `Winch` channel: each value received sends the
  current terminal size again.
- `attach.Resizes()` returns a channel of the process's `SIGWINCH` and
  the function that stops it.
- `attach.DetachKey` is Ctrl-] (`0x1d`): the bytes typed before it are
  sent, then a detach frame, and `Worker` returns `nil` without touching
  the session.

A refused attach is reported with the message of the daemon's error
envelope before the terminal is touched: an unknown worker, a worker
whose turn is running, one that already has a pilot or has no live
session. A session that ends while attached is reported as
`worker <name>: <reason>`.

## Terminal ownership

- The raw-mode restore is a deferred call, so the terminal gets its
  previous settings back on every path, errors included.
- Input is read only after `poll(2)` reports it ready, with a 50ms
  timeout, and the relay stops when `Worker` returns; `Worker` waits
  for it before restoring the terminal. Nothing reads the terminal
  after `Worker` returns, so the next reader (the dashboard, or the
  shell after `maestro attach`) gets every key typed afterwards.

## Attach from the dashboard

The dashboard attaches the worker its screen points at, on the project
shown:

- `a` on the workers screen attaches the selected worker, on a worker's
  detail that worker, and on a task's detail the worker driving the
  task; a task no worker drives reports `no worker drives this task`.
  The dashboard and the tasks screen show no worker and ignore the key.
- The key returns a `tea.Exec` command. Bubble Tea stops its input
  reader, leaves the alternate screen and restores the terminal, then
  runs the attach on the program's own input and output:
  `attach.Worker` with the program input as `*os.File` and `SIGWINCH`
  forwarded. An input that is not a file fails with
  `tui.ErrNoTerminal`.
- Whatever the attach returns, Bubble Tea takes the terminal back,
  re-enters the alternate screen and delivers the outcome to the model,
  which shows `detached from <worker>` or the error (control characters
  replaced) above the screen, and refreshes it.

## Tests

`internal/attach` tests run against a confined `/bin/sh` or `/bin/cat`
session served as the session of a worker by an in-process
`ipc.Server`, over a registered temporary repository, on a temporary
socket, with a PTY pair standing in for the user's terminal: relay and
detach, the hand-back reported as a detach, resize propagation,
terminal restoration after a detach, an error and a refused attach, and
no read of the terminal once `Worker` has returned. They skip when the
host cannot confine a process.

`internal/tui` drives the `a` key through `Update` on each screen, and
runs the whole program with `tui.Run` on the terminal side of a PTY
pair against the same kind of fixture session: it opens the workers,
attaches the selected one, runs a command in the session, detaches with
Ctrl-], has an attach refused by the daemon, then attaches to a session
that exits during the attach, checking each time that the dashboard
resumes, and that the terminal settings are restored after `q`.

`internal/worker` tests the transitions against a supervisor running a
fixture agent: an `IDLE` worker attached and handed back on a detach
and on a lost connection, a second pilot and an assignment refused
while attached, a `BUSY` worker refused, a `WAITING_INPUT` worker
handed back with its assignment, and a session that ends while attached
failing the worker.
