# Project initialization

`maestro init` registers a Git repository with Maestro and imports it into
a private canonical repository. Maestro then works on that private copy;
the user repository is never modified.

```console
$ cd my-project
$ maestro init
project registered: 6b1f6d3a-…
```

An explicit path may be given instead of the working directory:

```console
$ maestro init ~/src/my-project
```

The repository must contain at least one commit. Running `maestro init`
again on an already registered repository — from any of its worktrees or
through any path alias — is harmless and reports the existing project:

```console
$ maestro init
project already registered: 6b1f6d3a-…
```

## What initialization does

- Resolves the repository identity from its common Git directory, so
  every linked worktree and symbolic-link alias maps to the same project.
- Creates a project directory under Maestro's data directory, named by a
  generated project identifier.
- Clones the repository as a bare canonical copy with fully copied
  objects: no hardlinks and no alternates point back at the user
  repository.
- Marks the imported `HEAD` as `refs/heads/maestro/integration`, the
  branch Maestro will integrate work on.
- Records the project metadata in `project.json`.

## Data directory

Project data lives outside the repository, in the first of:

1. `$MAESTRO_DATA_HOME`
2. `$XDG_DATA_HOME/maestro`
3. `~/.local/share/maestro`

Each project occupies `projects/<id>/` with its `repository.git` canonical
clone and its `project.json` metadata.
