# Terminal dashboard

`internal/tui` is the dashboard behind `maestro tui`: a
[Bubble Tea](https://github.com/charmbracelet/bubbletea) program styled
with [Lip Gloss](https://github.com/charmbracelet/lipgloss). It is a
plain client of the daemon's versioned API and owns no state.

## Data

The dashboard reads only existing routes, over the Unix socket with
`transport.Client`:

| Screen | Routes |
| --- | --- |
| dashboard | `GET /v1/status`, `GET /v1/projects` |
| tasks | `GET /v1/status`, `GET /v1/tasks?project_id=` |
| task detail | `GET /v1/status`, `GET /v1/tasks/{id}?project_id=` |

Every refresh starts with `/v1/status`: a failed connection there (or on
the screen's route) is reported as `tui.ErrUnreachable` and the screen
shows the daemon as down. An error envelope on the screen's route is
shown in place of the screen, the daemon line staying visible. Each
refresh is bounded by a five-second timeout.

## Model

`tui.New(Options)` returns the `Model`; `tui.Run` runs it on an input
and an output with the alternate screen, and returns when the user quits
or its context is cancelled. `Options` carries the socket, an optional
project to open on, the refresh interval (`DefaultInterval`, 2s) and an
optional Lip Gloss renderer; without one, `New` renders plain text.

- `Init` starts the first refresh and arms a `tea.Tick` timer; each
  tick refreshes the current screen and re-arms the timer. A tick while
  a refresh is in flight only re-arms the timer.
- Every refresh carries a sequence number. Navigating starts a new
  refresh, and a result whose number is not the latest is dropped, so a
  slow answer for a previous screen never overwrites the current one.
- Cursors are clamped to the refreshed lists, so a list that shrinks
  keeps a valid selection.
- Text that comes from a user or from disk (descriptions, reasons,
  repository paths) has its control characters replaced before being
  drawn, so it cannot drive the terminal. A known window width
  truncates the lines to fit.

## Tests

The tests drive the model without a terminal: they send key, tick,
resize and refresh messages to `Update`, run the command a message
returns, and assert on `View()`, against an in-process `ipc.Server`
served on a temporary Unix socket with tasks created over the API. The
program itself is run through `tui.Run` on a pipe and a recording
writer, and must stop on `q` and on context cancellation.
