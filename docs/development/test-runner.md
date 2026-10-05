# Test runner

The `internal/testrun` package runs a project's configured test commands
against one exact revision and turns each result into a `TEST_REPORT`
handoff document bound to the tested SHA. Nothing is built from task
text: commands come from configuration as explicit argument vectors.

## Configuration

Test commands live in the `[tests]` table of `.maestro.toml` (or
`.maestro.local.toml`), one named table per command:

```toml
[tests.unit]
argv = ["go", "test", "./..."]
timeout = "10m"

[tests.lint]
argv = ["make", "lint"]
```

- `argv` is required and is run as is, never through a shell; a string
  instead of an array, an empty array or an empty program is refused.
- `timeout` is optional and must be positive; without it the runner's
  default applies (30 minutes).
- Names use lowercase letters, digits and dashes. Layers merge key by
  key, like the rest of the configuration, and the usual secret refusal
  applies to every value.

The table is part of the configuration snapshot: changing a test command
changes the `config_id`. `testrun.Commands(config)` lists the commands
sorted by name.

## Running

`Runner.Run(repository, sha, clone, commands)`:

1. Clones `repository` into `clone` (which must not exist) with
   `git clone --no-local --no-checkout`, so the clone shares no file with
   the repository. A commit reachable only from another reference, such
   as an operation reference, is fetched explicitly. The SHA is then
   checked out detached and `HEAD` is verified to equal it. Only full
   object ids are accepted; anything else fails with `ErrCheckout`.
2. Runs each command in order, each in its own confined group from
   `internal/launcher`, with the clone as working directory:
   - no network (only a loopback);
   - the clone's `.git` bound read-only: a command may build in the
     sources but cannot move `HEAD`, write the index or change the
     clone's configuration;
   - nothing else of the host or the repository is mounted;
   - the fixed environment `testrun.Environment()` (`PATH`, `HOME=/tmp`,
     `TMPDIR=/tmp`, `LANG`, `TERM`), nothing inherited from the daemon, so
     no credential or API key reaches a test;
   - a timeout: on expiry the group is stopped (SIGTERM, then SIGKILL
     after the grace period) and the result is marked timed out;
   - a bounded capture of the combined stdout and stderr that keeps the
     last bytes (256 KiB by default) and reports the truncation.
3. After each command, compares the clone with the tested revision:
   a moved `HEAD`, a changed index entry or a modified tracked file is
   listed in `Changed`; untracked and ignored files (build outputs) are
   listed separately in `Untracked`. Each list is capped at 1000 paths.

The daemon's own Git commands on the clone ignore system and global
configuration, hooks and file-system monitors.

A non-zero exit is a result, not an error. A `Result` records the name,
argv, tested SHA, exit code, duration, bounded output, timeout and the
two path lists. A result passes when it exits 0, in time, with no
`Changed` entry; a `Run` passes when every result passes. The clone is
left in place for diagnosis; the caller removes it.

## Reports

`Run.Documents(identity)` renders one `TEST_REPORT` document per command,
with the artifact id `<prefix>-<command name>` and the task, turn,
attempt, worker and configuration ids of the `Identity`. Each document is
checked against the handoff contract before it is returned.
`testrun.Accept(db, run, identity)` validates every document, then
persists each one with `handoff.Accept`; an artifact id already accepted
is refused.

The `TEST_REPORT` payload carries, besides `tested_sha`, `argv`,
`environment`, `exit_code` and `log`, the optional fields `name`,
`duration_ms`, `timed_out`, `log_truncated`, `changed_paths` and
`untracked_paths`.

## A result is valid for its SHA only

`testrun.ForRevision(document, revision)` decodes a `TEST_REPORT` and
returns its payload only when `tested_sha` equals the revision under
evaluation; otherwise it fails with `ErrRevision`. A document of another
kind fails with `handoff.ErrInvalid`. `testrun.Passed(payload)` applies
the pass rule above to a stored report.
