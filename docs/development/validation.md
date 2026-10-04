# Driver validation matrix

Before a driver may drive an agent CLI, its behavior is checked on a
reference version against a fixed set of cases. Each case is recorded
from a real session with util-linux `script` (advanced format: input,
output and timing logs), imported with `fixture.ImportScript`, sanitized
and stored under `internal/fixture/testdata/<agent>/<version>/`. Raw
recordings never enter the repository; a guard test reloads every
committed fixture and checks that it is sanitized, free of detectable
secrets, labelled after its path and replayable.

A failed or unexercised mandatory case blocks the driver for that
version.

## Reference versions

| Agent | Version | Verdict |
| --- | --- | --- |
| Claude Code | 2.1.289 | **blocked** — tool approval not exercised |
| Codex | 0.160.0 | **blocked** — tool approval, crash and resume not exercised |

## Cases

| Case | Claude Code 2.1.289 | Codex 0.160.0 |
| --- | --- | --- |
| prompt | pass | pass |
| multiline prompt | pass | pass |
| tool approval | not exercised: the default mode accepts edits without asking | not exercised: `workspace-write` writes without asking |
| free question | pass: asks, then waits for the answer | pass: asks, then waits for the answer |
| error (unknown model) | pass: explicit error | pass: explicit error |
| network silence | pass: explicit, bounded retries | degraded: no error, works forever |
| interruption | pass | pass |
| resize | pass | pass |
| crash | pass: SIGKILL ends the agent | not exercised: only the TUI was killed |
| resume by id | pass: the conversation survives the crash | not exercised: reattached to a live task |

## Observations for the drivers

- **Claude Code starts in an automatic mode.** Version 2.1.289 starts
  with edits accepted automatically. A driver must pass the permission
  mode explicitly and never rely on the default.
- **Claude Code queries the terminal at startup** (`XTVERSION`,
  `DECRQM`); during the recordings a real terminal answered. Inside a
  daemon-owned PTY nobody answers unless a client is attached: the
  driver must either answer these queries or prove that the CLI starts
  without replies.
- **Claude Code reports network loss explicitly**: "Can't reach the API
  server … Retrying in 1s · attempt 1/10", with a bounded number of
  attempts — a marker the turn detector can recognize.
- **Codex gives no signal on network loss**: the turn stays "Working"
  with no error and no timeout. Only Maestro's own turn timeout can end
  such a turn; the driver must report it as unknown, never as completed.
- **Codex runs turns in an app-server daemon, not in its TUI.** Quitting
  the TUI prints "Any running work continues", and killing the TUI with
  SIGKILL left the turn running: the resume reattached to it. Under
  Maestro the daemon must start inside the worker's sandbox, from the
  worker's private home, so that stopping the group stops the work; the
  crash case must kill the daemon too before resume is accepted.
- **Codex resume needs its own session id**: it is chosen by Codex and
  read back from the conversation file it writes, never from
  `resume --last`.
- An interactive `dash` inside the sandbox cannot hand the terminal back
  to its original process group on exit; this does not affect the agent
  CLIs.
