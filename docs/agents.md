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

## Native session drivers

The `internal/agent` library constructs confined interactive commands for
these versions. `OpenClaude` records a chosen UUID before launch;
`ClaudeStart` uses `--session-id` and explicitly selects `acceptEdits`.
`ConfirmClaude` checks the ID, worktree and version in the worker's private
native transcript. Mode records without that provenance cannot confirm it.
Automatic recovery uses only `claude --resume <confirmed-uuid>`.

`OpenCodex` starts without an ID. `CodexStart` sets a private `CODEX_HOME`,
uses `workspace-write` with `never` approvals, and passes `--no-daemon` so
the TUI cannot connect to a shared host daemon. `ConfirmCodex` reads the
local `session_meta` headers: exactly one UUID, matching the worktree and
version, must exist. It persists that ID before returning. Recovery uses
only `codex resume <confirmed-uuid>`; missing, malformed, conflicting or
ambiguous metadata blocks it. Neither driver uses a latest-session selector.

Each worker needs an exclusively allocated private HOME and a separate
supervisor state directory. Both must be private directories; sources,
HOME and supervisor state cannot overlap. The identity record carries the
worker, configuration and version, is atomically published and fsynced,
and survives a crash. A kernel lock prevents concurrent supervisors from
opening the same state directory. The command builder refuses writable
mounts that expose the supervisor record. Native credentials, onboarding,
folder trust, instruction files and MCP settings are supplied by the caller;
drivers do not inherit the user's HOME or copy credentials. A trust or
approval dialog that still appears is an input wait.

`ClaudeResumeSpec` and `CodexResumeSpec` restore the private environment
and mounts after daemon recovery. Their exact-ID callbacks also work with
`session.Epoch.Restart`. Metadata is checked again before constructing a
resume command; deleting a saved native conversation never silently starts
another. Callers must confirm both native identity and the permissions
profile before assigning work, and use the epoch barrier before releasing
the worker. These APIs are library primitives; `agent doctor` remains the
public agent command.

## Detecting turns

`NewTurnDetector` accepts only a validated kind/version. It reconstructs
the visible terminal, including fragmented escape sequences, cursor
redraws and resizes, then combines the CLI's current ready marker with PTY
inactivity and process liveness. An older summary above a newer prompt or
partial answer cannot finish that answer; neither can a visible working
indicator, silence alone or an exit with code zero.

The observations are `RUNNING`, `WAITING_INPUT`, `COMPLETED`, `INTERRUPTED`,
`FAILED` and `UNKNOWN`. `COMPLETED` means ready for artifact and Git
validation, never task success. Trust/approval dialogs and questions in
assistant text produce conservative input waits. A question mark in an
answer can therefore require human inspection rather than automatic
completion. Unsupported rendering stays unknown and fails at the timeout.

Call `Begin` for a new turn, `Feed` for every output chunk, `Resize` when
the PTY changes size, and `Poll` regularly even during silence. An admitted
answer uses `Continue`, which preserves the original turn deadline.
Supervisor interruption uses `Interrupt`; the detector never approves a
tool or supplies an answer itself. Defaults are 300 ms of inactivity,
20 minutes per turn and 5 minutes per input wait. Callers pass the frozen
configuration snapshot's bounds when constructing a detector.
