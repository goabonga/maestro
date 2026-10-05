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
version. The allowed ranges are the ones registered in
`internal/agent` and checked by `maestro agent doctor`. An allowed driver is allowed for its reference version only,
and only under the conditions its observations impose.

## Reference versions

| Agent | Version | Verdict |
| --- | --- | --- |
| Claude Code | 2.1.289 | **allowed**, under the conditions below |
| Codex | 0.160.0 | **allowed**, under the conditions below |

## Cases

| Case | Claude Code 2.1.289 | Codex 0.160.0 |
| --- | --- | --- |
| prompt | pass | pass |
| multiline prompt | pass | pass |
| tool approval | pass: asks before creating a file in the default permission mode | pass: asks before running a write in `read-only` with `on-request` approvals |
| free question | pass: asks, then waits for the answer | pass: asks, then waits for the answer |
| error (unknown model) | pass: explicit error | pass: explicit error |
| network silence | pass: explicit, bounded retries | pass, degraded: no error, ends only on Maestro's turn timeout |
| interruption | pass | pass |
| resize | pass | pass |
| crash | pass: SIGKILL ends the agent | pass: the TUI and its profile's daemon are killed |
| resume by id | pass: the conversation survives the crash | pass: a fresh daemon resumes the conversation by id |

## Observations for the drivers

- **Claude Code starts in an automatic mode.** Version 2.1.289 starts
  with edits accepted automatically. A driver must pass the permission
  mode explicitly and never rely on the default. In the default mode the
  approval reads "Do you want to create hello.txt?" with numbered
  choices.
- **Both CLIs ask to trust an unknown folder** on their first start in
  it ("Quick safety check" for Claude Code, "Trust this folder?" for
  Codex). Maestro must provision that trust for the worktree, or the
  driver must recognise the dialog as waiting for input.
- **Claude Code queries the terminal at startup** (`XTVERSION`,
  `DECRQM`, a kitty graphics probe); during the recordings a real terminal answered. Inside a
  daemon-owned PTY nobody answers unless a client is attached: the
  driver must either answer these queries or prove that the CLI starts
  without replies.
- **Claude Code reports network loss explicitly**: "Can't reach the API
  server … Retrying in 1s · attempt 1/10", with a bounded number of
  attempts — a marker the turn detector can recognize.
- **Codex gives no signal on network loss**: the turn stays "Working"
  with no error and no timeout. Its approval reads "Would you like to run
  the following command?" with `y`, `p` and `esc` choices. Only Maestro's own turn timeout can end
  such a turn; the driver must report it as unknown, never as completed.
- **Codex runs turns in an app-server daemon, not in its TUI.** Quitting
  the TUI prints "Any running work continues", and the daemon outlives
  it. Killing only the TUI leaves the turn running; the recorded crash
  kills the TUI and the daemon of a dedicated profile, and the resume
  then starts a fresh daemon that answers from the persisted
  conversation. Under Maestro the daemon must start inside the worker's
  sandbox, from the worker's private home, so that stopping the group
  stops the work.
- **Codex resume needs its own session id**: it is chosen by Codex and
  read back from the conversation file it writes, never from
  `resume --last`.
- An interactive `dash` inside the sandbox cannot hand the terminal back
  to its original process group on exit; this does not affect the agent
  CLIs.

## Driver implementation checks

The agent tests replay the committed prompt, multiline, resize and resume
captures into the versioned turn detector, including output split inside
escape sequences and UTF-8 characters. The Claude resume capture answers
the remembered code word and then asks a follow-up question: its expected
observation is `WAITING_INPUT`. Network-loss captures never become
`COMPLETED`. Additional tests cover stale summaries above a new streamed
answer, process death, interruptions, input waits and their preserved turn
deadline, and terminal-control rejection in multiline prompts.

Native identity tests use temporary worker directories and fixture
processes. They check durable reopen, exclusive supervisor ownership,
worktree/version/configuration binding, missing and ambiguous IDs, corrupt
records and symlink escapes. A fixture CLI also creates its metadata from
inside a confined PTY before confirmation. Automated tests do not launch
the real Claude or Codex binaries or contact model APIs.

Manual checks on 2026-10-05 exercised both reference binaries in private
Bubblewrap namespaces with separate HOME directories: creation, forced
runtime stop and exact-ID interactive resume. Native conversation files
contained the expected UUIDs and actual assistant responses. The Go driver,
launcher, PTY, metadata confirmation and detector were then exercised
together for each agent and reached `COMPLETED`. These checks used explicit
limits of 120 CPU seconds, 64 GiB of address space and 512 processes; they
do not establish native compatibility with a different limits profile.
Raw transcripts remain outside the repository; temporary credential copies
were removed after these checks.
