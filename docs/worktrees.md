# Inspecting worktrees

Maestro keeps its working copies in task worktrees under its
[data directory](initialization.md#data-directory), one per task and per
worker. Two read-only commands expose them without entering them; both
run from inside the registered repository, or take `--path` to point at
it.

## List

`maestro worktree list` prints every task worktree with its worker, its
task, its branch, its `HEAD` and whether it holds pending changes:

```console
$ maestro worktree list
WORKER     TASK    BRANCH               HEAD          STATE  PATH
claude-01  task-1  maestro/task-task-1  2c87cbbc71c5  clean  …/worktrees/claude-01/tasks/task-1
```

A repository that was never registered is refused: run
[`maestro init`](initialization.md) first.

## Diff

`maestro diff <worker> [task]` prints the pending changes of a worker's
task worktree — staged and unstaged, against its `HEAD`. The task may be
omitted when the worker holds exactly one task worktree; otherwise the
command lists the candidates. A clean worktree prints nothing.

```console
$ maestro diff claude-01
diff --git a/README.md b/README.md
…
```

Both commands only read: they never create, modify or remove a worktree,
and they verify through Git that each path really is a working tree
before reading it.
