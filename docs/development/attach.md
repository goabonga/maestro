# Attach client

`internal/attach` is the client side of a session's stream
(`GET /v1/sessions/{id}/stream` upgraded to the maestro stream
protocol). Both `maestro attach <session>` (`internal/cli`) and the
terminal dashboard (`internal/tui`) use it; attach stays a dedicated
terminal mode, never drawn inside a widget.

## API

- `attach.Run(ctx, socket, session, attach.Terminal)` dials the
  daemon's socket, upgrades to the session's stream, switches
  `Terminal.In` to raw mode, sends the terminal size, then relays
  keystrokes as input frames and output frames to `Terminal.Out` until
  the user detaches, the session ends or the connection fails.
- `attach.Terminal` carries the input terminal (`*os.File`), the output
  writer and an optional `Winch` channel: each value received sends the
  current terminal size again.
- `attach.Resizes()` returns a channel of the process's `SIGWINCH` and
  the function that stops it.
- `attach.DetachKey` is Ctrl-] (`0x1d`): the bytes typed before it are
  sent, then a detach frame, and `Run` returns `nil` without touching
  the session.

An unknown session (`404`) is reported as `unknown session: <id>` before
the terminal is touched. A session that ends while attached is reported
as `session <id>: <reason>`.

## Terminal ownership

- The raw-mode restore is a deferred call, so the terminal gets its
  previous settings back on every path, errors included.
- Input is read only after `poll(2)` reports it ready, with a 50ms
  timeout, and the relay stops when `Run` returns; `Run` waits for it
  before restoring the terminal. Nothing reads the terminal after `Run`
  returns, so the next reader (the dashboard, or the shell after
  `maestro attach`) gets every key typed afterwards.

## Attach from the dashboard

The daemon does not list sessions yet, so the dashboard attaches by id:

- `a`, on any screen, opens a prompt reading a session id. It accepts
  printable, non-space characters up to 128 bytes; `backspace` edits,
  `esc` cancels, `enter` with an empty id closes the prompt, `ctrl+c`
  quits. While the prompt is open, other keys (`q` included) are typed
  into it.
- `enter` returns a `tea.Exec` command. Bubble Tea stops its input
  reader, leaves the alternate screen and restores the terminal, then
  runs the attach on the program's own input and output: `attach.Run`
  with the program input as `*os.File` and `SIGWINCH` forwarded. An
  input that is not a file fails with `tui.ErrNoTerminal`.
- Whatever the attach returns, Bubble Tea takes the terminal back,
  re-enters the alternate screen and delivers the outcome to the model,
  which shows `detached from <id>` or the error (control characters
  replaced) above the screen, and refreshes it.

## Tests

`internal/attach` tests run against a confined `/bin/sh` session served
by an in-process `ipc.Server` on a temporary socket, with a PTY pair
standing in for the user's terminal: relay and detach, resize
propagation, terminal restoration after a detach, an error and a refused
attach, and no read of the terminal once `Run` has returned. They skip
when the host cannot confine a process.

`internal/tui` drives the prompt through `Update`, and runs the whole
program with `tui.Run` on the terminal side of a PTY pair against the
same kind of fixture session: it attaches, runs a command in the
session, detaches with Ctrl-], attaches to an unknown session, then to a
session that exits during the attach, checking each time that the
dashboard resumes, and that the terminal settings are restored after
`q`.
