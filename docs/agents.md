# Checking agents

Maestro only drives agent CLIs whose exact version it has validated.
`maestro agent doctor` checks a host before anything is started:

```console
$ maestro agent doctor
CHECK        STATUS   DETAIL
sandbox      ok       user, mount, PID and network namespaces available
claude-code  ok       2.1.289 → driver claude-code-2.1
codex        refused  no validated driver for codex 0.161.0 (validated: [0.160.0–0.160.0])
```

- **sandbox** — the confinement every agent runs in must be available
  (Bubblewrap and `prlimit`, able to create user, mount, PID and network
  namespaces). Without it nothing starts; there is no permissive mode.
- **one line per agent** — the binary is looked up on `PATH`, its
  `--version` output is read, and the version must fall inside the
  validated range of a driver. A missing binary, an unreadable version
  or a version outside every validated range is refused with the reason,
  never driven blindly.

The command exits with an error when any check is refused, so it can
gate a script.

## Validated versions

| Agent | Binary | Validated versions |
| --- | --- | --- |
| Claude Code | `claude` | 2.1.289 |
| Codex | `codex` | 0.160.0 |

Supporting a new version means recording it against the driver
validation matrix first; the range only grows once that version passes.
