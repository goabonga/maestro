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

↑/↓ select · enter tasks · r refresh · q quit
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
  prints them.

## Keys

| Key | Action |
| --- | --- |
| `↑` / `↓` (or `k` / `j`) | move the selection |
| `enter` | open the selected project or task |
| `esc` / `backspace` | go back to the previous screen |
| `r` | refresh now |
| `q` / `ctrl+c` | quit |

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
