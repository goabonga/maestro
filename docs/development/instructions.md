# Instruction provisioning

`internal/provision` materializes a project's instruction files into a
task worktree before an agent starts in it. The files come from the
configuration snapshot (`Snapshot.Instructions`, frozen by
`internal/config` under a `config_id`), never from the current disk: a
task always sees the instructions it was created with.

## Layout

Instruction files are versioned under the project's `maestro/`
directory, one subdirectory per agent kind, at the path the agent reads
them relative to the worktree root:

```text
maestro/
  agents/
    claude-code/
      CLAUDE.md
      .claude/skills/<skill>/SKILL.md
      roles/
        review/
          CLAUDE.md
    codex/
      AGENTS.md
      roles/
        coding/
          AGENTS.md
```

- `maestro/agents/<kind>/<path>` applies to every worker of that kind.
- `maestro/agents/<kind>/roles/<role>/<path>` is the role overlay: for a
  worker with that role, it replaces the kind's file at the same path.
  Other roles' files are ignored.
- Files elsewhere under `maestro/` are not instruction files for an
  agent and are left out.

Each kind accepts only its native locations, and any other path is
refused with `ErrInvalidInstructions`:

| Kind | Native paths |
| --- | --- |
| `claude-code` | `CLAUDE.md` and the `.claude/` tree (skills under `.claude/skills/`) |
| `codex` | `AGENTS.md` |

Roles are kebab-case names; an empty role applies no overlay. Paths
must be clean relative paths without control characters and never
enter a `.git` directory.

## Provisioning

```go
paths, err := provision.Instructions(worktree, provision.KindClaudeCode, "review", snapshot.Instructions)
```

`Instructions` takes the root of a task worktree and returns the
provisioned paths, relative to the worktree and sorted, for read-only
mounts and runtime path control.

Every target is checked before anything is written:

- A file the repository tracks is never replaced. For `claude-code`, a
  tracked `CLAUDE.md` sends Maestro's file to `CLAUDE.local.md`, which
  Claude Code also reads, when that path is free. Any other collision,
  and every collision for `codex`, fails with `ErrPathTaken`.
- An untracked file Maestro did not provision is not replaced either.
- No path is written through a link or over a non-regular file.

Provisioning again is idempotent: unchanged files are left as they are,
and files from a previous provisioning that the new set no longer
carries are removed, unless the repository has started tracking them.
The previous set is recorded in `<git-dir>/maestro/instructions`, where
`<git-dir>` is the worktree's private Git directory.

## Exclusion from Git

Provisioned files must not show up as changes. Maestro excludes them
for the worktree only, never through `.gitignore`, the repository's
`info/exclude` or the per-worktree `info/exclude` (which Git does not
read):

1. It enables `extensions.worktreeConfig` in the repository. Git then
   reads `core.bare` and `core.worktree` from the common configuration
   for every worktree, so those keys move first to the main worktree's
   `config.worktree`; a bare worker repository stays bare and its task
   worktrees keep working.
2. It writes `<git-dir>/maestro/excludes`, outside the worktree sources
   (`provision.ExcludesFile` returns the path). The file first copies the
   excludes Git would use otherwise — the effective `core.excludesFile`,
   or Git's default `$XDG_CONFIG_HOME/git/ignore` — then lists the
   provisioned paths, anchored and quoted so they match literally.
3. It sets `git config --worktree core.excludesFile` to that file.

A tracked file is never added to the excludes: it stays visible to Git,
and changes to it show as usual.
