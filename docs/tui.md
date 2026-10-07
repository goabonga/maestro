# Terminal dashboard

`maestro tui` opens a full-screen dashboard on the running
[daemon](daemon.md). It reads the same API as the other commands, over
the daemon's socket, and refreshes itself every two seconds. Closing it
never affects the daemon or its tasks.

```console
$ maestro tui
```

```text
maestro · dashboard

daemon: maestro-svc 0.1.0

CAPACITY  USED  LIMIT  BY PROJECT
commands  0     3
sessions  1     3      6b1f6d3a-…: 1
tests     0     1

  PROJECT     STATE  REPOSITORY
> 6b1f6d3a-…  ok     /home/me/src/app/.git

↑/↓ select · enter tasks · w workers · : command · r refresh · q quit
```

## Screens

- **Dashboard**: the daemon's name and version, its capacity — each
  ceiling, its consumption and the per-project detail, as
  `maestro status` prints them — and the registered projects with
  their state.
- **Tasks**: the tasks of the selected project, oldest first, with
  their state, creation time and the first line of their description,
  as `maestro task list` prints them.
- **Task detail**: one task with its branch, base, configuration, fix
  cycles, full description and recorded events, as `maestro task show`
  prints them, and the workers assigned to it with their role and
  state (`-` when none is).
- **Workers**: the [workers](cli.md#workers) of the selected project, by
  name, with their agent, state, assigned task and role, last update
  and the reason of their last change, such as the cause of a failed
  start. It opens with `w` on a project of the dashboard or from the
  project's tasks, and `esc` returns there.
- **Worker detail**: one worker with its agent, driver, state, reason,
  assignment (task, role and turn), repository and its 20 most recent
  events, as `maestro worker show` prints them.

## Keys

| Key | Action |
| --- | --- |
| `↑` / `↓` (or `k` / `j`) | move the selection |
| `enter` | open the selected project, task or worker |
| `w` | open the workers of the selected project, or of the project whose tasks are shown |
| `a` | pilot the selected worker, the worker shown, or the worker driving the task shown |
| `:` | open the command bar |
| `esc` / `backspace` | go back to the previous screen |
| `r` | refresh now |
| `q` / `ctrl+c` | quit |

## Attach

`a` attaches the terminal to a worker's session, as
[`maestro attach`](cli.md#workers) does: the selected worker on the
workers screen, the worker shown on its detail, or the worker driving
the task shown on a task's detail. You then pilot the agent yourself:
the worker is `ATTACHED`, and Maestro sends it no turn and writes
nothing to its terminal until you leave. Only an `IDLE` worker, or one
whose turn waits for an input, can be attached, by one pilot at a
time; a worker running a turn is refused.

The dashboard is suspended for the time of the attach, which ends on
`Ctrl-]` or with the session. The worker is then handed back — `IDLE`,
or `WAITING_INPUT` with its task, or `FAILED` when its session ended —
and the dashboard resumes and reports `detached from <worker>`, or the
error that ended or refused the attach, under its title.

## Command bar

`:` opens a command bar at the bottom of the screen. A command acts on
the project shown, or on the project selected on the dashboard; `enter`
runs it and `esc` closes the bar without running anything.

| Command | Action |
| --- | --- |
| `start <agent> [n]` | starts `n` workers of the agent (default 1), as `maestro worker start --count <n>` does; the daemon refuses a start above its session ceiling |
| `stop <worker>` | stops the worker, as `maestro worker stop` does |

The outcome replaces the line under the title: the names of the
starting workers, the stopped worker's state, or the daemon's refusal.
A started worker goes from `STARTING` to `IDLE`, or `FAILED` with its
reason, on the workers screen as the dashboard refreshes.

## Options

- `--project <id>` opens the dashboard directly on that project's tasks;
  `maestro project list` prints the ids.
- `--interval <duration>` changes the refresh period (default `2s`,
  minimum `100ms`).
- `--socket <path>` names the daemon socket, as for the other commands.

When the daemon is not running, the dashboard says so and keeps
retrying at every refresh; it shows the daemon again as soon as it
answers. An error the daemon returns for a screen, such as an unknown
project, is shown in place of that screen.
