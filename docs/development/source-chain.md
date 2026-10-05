# Source chain validation

Before a task is integrated, the commits it brings must form a clean
chain on top of its task base. `integration.ValidateSourceChain` in
package `internal/integration` checks that chain and returns its commits,
oldest first, for the candidate builder.

```go
chain, err := integration.ValidateSourceChain(repository, taskBaseSHA, sourceHeadSHA, runtimePaths)
```

## Rules

- The task base and the source head must be full, lowercase object ids
  (SHA-1 or SHA-256) of commits of the repository. Anything else (a
  branch name, `HEAD`, an abbreviated id, a tree id, an option-like
  value) is refused with `ErrSourceRevision` before any other Git command
  runs.
- The source head must differ from the task base; otherwise the task has
  nothing to integrate and `ErrEmptySourceChain` is returned.
- The source head must descend from the task base.
- The commits of `base..head` must form a linear first-parent chain whose
  oldest commit has the task base as its only parent. A merge commit
  anywhere in the chain is refused. Because every commit has a single
  parent and the chain ends on the base, the range holds no commit
  foreign to the chain.
- No commit of the chain may add or modify a runtime path. This reuses
  `provision.CheckCommits` (see [Runtime path control](runtime-paths.md)):
  each commit is checked individually, so a runtime file added then
  removed later is still refused, while removing a runtime path tracked
  at the base is accepted.

Corrections stay in the chain: a later commit fixing an earlier one is
accepted, and the returned list contains every commit, not only the last
one, so the candidate applies the complete `base..head` delta.

## Errors

Chain violations wrap `ErrSourceChain` and name the offending commit, for
example:

```text
invalid source chain: commit 3f2a… is a merge commit
invalid source chain: source head 9b1c… does not descend from the task base 41d0…
invalid source chain: runtime path change refused: commit 7e5f… adds .claude/settings.json
```

A runtime path violation wraps both `ErrSourceChain` and
`provision.ErrRuntimePath`. An invalid runtime path list is refused with
`provision.ErrInvalidRuntimePath` before Git runs.

## Git usage

Only read-only plumbing runs (`rev-parse`, `merge-base --is-ancestor`,
`rev-list`), with fixed verbs and validated object ids. The validator's
own commands run without system or global configuration, without hooks
and without optional locks, inheriting only `PATH` from the daemon.
