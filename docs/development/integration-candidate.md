# Integration candidate

An integration turns the whole work of a task into one commit on top of
the integration base. `Store.BuildCandidate` in package
`internal/integration` builds that commit for a prepared or started
INTEGRATE operation and leaves the operation `APPLIED` with its durable
result reference (see [Operations journal](operations.md)).

```go
op, err := store.BuildCandidate(project, operationID, runtimePaths)
```

## Steps

1. **Check the inputs**, before any Git mutation:
    - the operation is an INTEGRATE of this project, in `PREPARED` or
      `STARTED`, and has no candidate worktree left on disk;
    - its integration base is a commit of the canonical repository and
      `refs/heads/maestro/integration` still points at it;
    - `ValidateSourceChain` accepts `task_base_sha..source_head_sha` in
      the canonical repository (see [Source chain](source-chain.md)), so
      every source commit must already have been imported there, and the
      validated chain is exactly the chain frozen in the operation.
2. **Start** the operation if it is still `PREPARED`.
3. **Create the candidate**: a detached worktree of the canonical
   repository on the integration base, at
   `<project>/operations/<operation-id>/candidate/`
   (`integration.CandidateWorktree` returns that path).
4. **Apply the ordered chain**, oldest first, with
   `git cherry-pick --no-commit`, one commit at a time. Corrections made
   by later commits are part of the delta.
5. **Write the tree** with `git write-tree` and record it with
   `RecordCandidate` before any commit exists.
6. **Build one commit** with `BuildResult`: the recorded tree, the frozen
   message, author and committer, and the integration base as its only
   parent.
7. **Apply** with `ApplyIntegration`: the durable reference
   `refs/maestro/operations/<id>/result` is created before `APPLIED` and
   `result_sha` are stored.
8. **Remove the candidate**: it is checked out at the result, whose tree
   its index already holds, then removed with `git worktree remove`
   (never forced), its metadata pruned and the empty operation directory
   deleted.

The integration branch is never moved by the build.

## Failures

| Situation | Error | Operation | Candidate worktree |
|-----------|-------|-----------|--------------------|
| A commit conflicts with the base | `*CandidateConflict` wrapping `ErrCandidateConflict` | `FAILED`, error naming the commit and paths | kept, reset to the base |
| The candidate tree equals the base tree | `ErrEmptyCandidate` | `FAILED`, never committed | removed |
| The integration branch left the frozen base | `ErrStaleIntegrationBase` | `FAILED` | never created |
| Invalid chain, runtime path change, or a chain other than the frozen one | `ErrSourceChain` | `FAILED` | never created |
| A source commit missing from the canonical repository | `ErrSourceRevision` | `FAILED` | never created |
| Any later Git or journal error | the error itself | `FAILED` | kept |
| A candidate worktree already exists | `ErrCandidateWorktree` | unchanged | left as is |
| Not an INTEGRATE of this project, or not `PREPARED`/`STARTED` | `ErrInvalid` / `ErrTransition` | unchanged | never created |

`CandidateConflict` carries the conflicting `Commit` and its `Paths`, in
index order. A cherry-pick with `--no-commit` leaves no sequencer state,
so the conflicted application is aborted by resetting the candidate's
detached `HEAD`, which is still the integration base; no branch is
involved.

An empty candidate is reported for a human decision and is never turned
into a commit. A new integration base always requires a new operation:
the build refuses to run once the integration branch has moved.

## Git usage

Commands use fixed verbs and validated full object ids, never task text.
They run without system or global configuration, with hooks disabled
(`core.hooksPath=/dev/null`), signing off (`commit.gpgSign=false`) and no
file-system monitor, inheriting only `PATH` from the daemon. Commands on
the canonical repository also sync objects and references to disk.
