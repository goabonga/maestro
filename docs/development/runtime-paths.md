# Runtime path control

Agent runtime files (instructions, settings, MCP configuration, Maestro's
own state) must never enter a task's history. Git exclusions are a
convenience only: `git add -f` bypasses them. The `internal/provision`
package therefore verifies the index and incoming commits itself and
refuses any addition or modification of a runtime path.

## Runtime paths

`provision.RuntimePaths` is a list of repository-relative paths, for
example:

```go
provision.RuntimePaths{"CLAUDE.md", ".claude/", "AGENTS.md", ".mcp.json", ".maestro/"}
```

An entry names a file or a directory (a trailing `/` is accepted); either
form covers the path itself and everything below it. Matching is exact
and case-sensitive: entries are never interpreted as patterns, so
`docs/CLAUDE.md` or `CLAUDE.md.bak` are not covered by `CLAUDE.md`.

`Validate` refuses an empty, absolute or unclean entry, or one that
leaves the repository (`..`), with `ErrInvalidRuntimePath`. Both checks
validate their list before running Git.

## Index check

`CheckIndex(dir, base, paths)` compares the index of the worktree at
`dir` with the task base commit (`git diff-index --cached`). Every
runtime path added or modified in the index relative to `base` is
refused, including a path staged with `git add -f` or committed on top of
the base. A runtime path already tracked at the base and left unchanged
is accepted, and so is its removal. Unstaged worktree changes are not
inspected.

## Incoming-commit check

`CheckCommits(dir, base, head, paths)` walks every commit of
`base..head` (`git rev-list`) and compares each one with its first
parent, or with the empty tree for a root commit (`git diff-tree`). The
check is per commit, not on the final diff: a runtime path added by one
commit and removed by a later one is still refused. Commits of a merged
side branch are part of the range and are checked individually; the merge
commit itself is compared with its first parent. Removing a runtime path
is accepted.

## Errors

Every violation wraps `ErrRuntimePath` and names where it happened and
the path, for example:

```text
runtime path change refused: index adds .claude/settings.json
runtime path change refused: commit 3f2a… modifies CLAUDE.md
```

All violations are reported together (`errors.Join`). A base or head that
does not resolve to a commit, or that could be read as a Git option, is
refused with `ErrInvalidRevision` before any other Git command runs; the
checks only use read-only plumbing with fixed verbs, on resolved commit
hashes.
