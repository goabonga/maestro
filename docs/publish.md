# Publishing the integration

Maestro works on a private copy of your repository. `maestro publish`
makes the result of that work visible in your repository, as the branch
`maestro/integration`, so you can merge or rebase it as you like.

```console
$ maestro publish
published 4e1c0a7… to refs/heads/maestro/integration (was created)
operation: 0f6a2c9e-…
```

The command talks to the running daemon. It works on the project of the
repository holding the current directory, or on the one named by
`--project <id>`; `--socket <path>` selects the daemon socket.

## What it changes

Only the branch `maestro/integration` of your repository. Your current
branch, your index and your working tree are never touched, and none of
your Git hooks run.

- When the branch does not exist, it is created.
- Otherwise it is only fast-forwarded:

  ```console
  $ maestro publish
  published 9b2d51e… to refs/heads/maestro/integration (was 4e1c0a7…)
  operation: 3d7e81b4-…
  ```

- When it already holds the integration, nothing happens:

  ```console
  $ maestro publish
  refs/heads/maestro/integration already at 9b2d51e…
  ```

## When it refuses

The publication is refused, and your branch is left as it is, when:

- the integration is not the result of a tested and committed Maestro
  operation;
- `maestro/integration` has commits that the integration does not
  contain (a divergence): the message names both commits. Move your work
  to another branch first;
- `maestro/integration` is checked out in any worktree of your
  repository: the message names the worktree. Switch that worktree to
  another branch first;
- `maestro/integration` changed while the publication was running.

Every publication is journaled as a `PUBLISH` operation. A publication
interrupted by a crash is finished or rolled back the next time you
publish, depending on whether your branch was already updated.
