# Syncing from your repository

Maestro works from the private integration branch of a project, never
from your branches. When you have moved your own repository forward,
`maestro sync` brings that work into the integration branch, after
testing it.

```console
$ maestro sync --from main
syncing branch main at 9d41e0a7c2b8… from integration 2c87cbbc71c5… (operation 5a0c7e12-…)
note: untracked files in the user repository are not synced: only commits are
synced 9d41e0a7c2b8…: integration advanced from 2c87cbbc71c5…
tests: sync-5a0c7e12-…-unit
```

The command talks to the running [daemon](daemon.md). It works on the
project of the current repository, or on the one named by
`--project <id>`, and accepts `--socket` like the other commands.

## What is synced

- **A frozen commit.** The daemon reads the commit your branch points to
  when you run the command and syncs exactly that commit. Committing on
  the branch while the sync runs changes nothing; the output names the
  commit that was synced.
- **Only commits.** Staged changes, unstaged changes and untracked files
  stay out of the sync. Each kind found in your working tree is reported
  with a `note:` line. When your repository configures content filters,
  Maestro does not run them, so unstaged changes are not inspected and a
  note says so.
- **Your repository is only read.** Its branches, index and working tree
  are left as they are, and none of its hooks run.

## When a sync is refused

A sync is refused, and nothing is recorded, when:

- the branch does not exist or its name is not a valid branch name;
- the branch is already at the integration head;
- the branch does not descend from the integration head. The error names
  both commits. Maestro never resets or merges on your behalf: prepare a
  branch that builds on the integration head and sync it instead;
- no test command is configured, or the daemon cannot run tests on this
  host. A sync always runs the project's tests;
- every test slot of the daemon is in use.

## Tests and the integration branch

The tests are the commands under `[tests]` in the project's configuration,
as the repository holds it when you run the command:

```toml
[tests.unit]
argv = ["make", "test"]
```

They run confined, on a private copy of the synced commit. When they all
pass, the integration branch advances to the synced commit, and only if
it still is where the sync found it: a concurrent change is refused. When
a test fails, the sync is rolled back, the integration branch does not
move, and the command fails with the failed tests and their reports.

Tasks already started keep the commit they started from; tasks created
after the sync start from the new integration head.
