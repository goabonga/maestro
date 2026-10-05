# Conflict resolution

When a source commit does not apply on the integration base,
`Store.BuildCandidate` fails the INTEGRATE operation with a
`*CandidateConflict` and the integration branch stays where it was (see
[Integration candidate](integration-candidate.md)). The task then goes
to `MERGE_CONFLICT` on that base through its `conflict` event (see
[Task workflow](tasks.md)). `resolution.go` in package
`internal/integration` implements the Git and journal side of the
resolution: the author resolves the conflict in its own private
repository, Maestro imports and verifies the resolved commit, then builds
its candidate as a new operation.

```go
r, err := store.PrepareResolution(project, conflictedID)
// the author resolves and commits in r.Path
op, err := store.ImportResolution(project, conflictedID, attemptID, nil, runtimePaths)
```

## Preparing the resolution worktree

`Store.PrepareResolution(project, id)` takes the operation the attempt
follows: the integration that conflicted, or the last rejected
resolution of that conflict. Before any Git mutation it checks that:

- the chain of operations superseding each other from `id` back to the
  conflicted integration holds only integrations of the project, of the
  same task and on the same integration base;
- `id` is the conflicted integration, `FAILED` or `ROLLED_BACK`, or a
  resolution whose rejection is journaled, and that no other operation
  already supersedes it;
- fewer than two resolutions were rejected on the base, by the task's
  counter and by the journal (`ErrResolutionsExhausted`), and the task
  is `MERGE_CONFLICT` on that base (`ErrResolutionTask`);
- no resolution worktree exists yet for `id`.

A conflicted integration still `FAILED` is rolled back first: the
integration branch never moved. The author is the worker of the
conflicted integration. In its private repository Maestro copies, from
the canonical repository, the integration base into the branch
`maestro/resolution-<id>` and the source head under
`refs/maestro/resolutions/<id>/source`, then checks the branch out in
`<project>/worktrees/<worker>/resolutions/<id>/`. The source commits are
applied in order with `git cherry-pick --no-commit` until the conflict,
which is left in the worktree. The task branch is never touched.

The returned `Resolution` names the worktree, its branch and base, the
commits applied cleanly, the conflicting commit with its paths, and the
remaining commits the author still applies: the resolution holds the
whole delta of the task. A conflict that no longer reproduces fails with
`ErrResolutionNotReproduced`.

## Importing the resolved commit

The author resolves the paths, applies the remaining commits and commits
once on the branch. `Store.ImportResolution(project, id, attemptID,
metadata, runtime)` then checks, before preparing anything:

| Check | Error |
| --- | --- |
| the integration branch is still at the conflict base | `ErrStaleIntegrationBase` |
| the worktree is clean, on its branch, and the branch left the base | `ErrResolutionIncomplete` |
| the commit has the integration base as its only parent | `ErrResolutionBase` |
| no runtime path is added or modified | `ErrSourceChain` and `provision.ErrRuntimePath` |
| every touched path is touched by a source commit of the conflicted integration | `ErrResolutionPath` |

The commit is copied into the canonical repository under
`refs/maestro/resolutions/<id>/proposal` before these checks. A refused
proposal prepares no operation.

An accepted proposal becomes a new INTEGRATE operation with its own id
and `attemptID`, superseding `id`. Its task base and integration base are
both the conflict base, its source chain is the single resolved commit,
its configuration snapshot is the task's, and its commit metadata is
`metadata` or, when nil, the conflicted integration's. `BuildCandidate`
builds it like any integration and leaves it `APPLIED` with its durable
result reference. The task stays `MERGE_CONFLICT`: the proposal is
provisional until its tests and the review of the resolved diff pass.

## Rejecting and accepting a resolution

`Store.RejectResolution(project, id, reason)` rejects a resolution
operation whose build, tests or review failed, or that a human refused.
The operation is failed and rolled back unless it already is; then, in
one SQLite transaction, a `reject-resolution` event is journaled on the
operation and the task goes through its `reject-resolution` event. The
first rejection on a base keeps the task in `MERGE_CONFLICT` for another
proposal; the second blocks it with continuation `MERGE_CONFLICT`. A
resolution is rejected once. `Store.RejectedResolutions(taskID, base)`
counts the journaled rejections of an integration base.

`Store.AcceptResolution(project, id, guard)` returns the task of a
`TESTED` resolution to integration. In one SQLite transaction the task
goes through `resolve-conflict`, whose guard carries the validity of the
proposal, the verification of the resolved diff and whether a human
approval is required, then through `build-candidate` with the
operation's result. In `human` mode that approval is the task's
`approve-resolution` event on the candidate's `result_sha`, recorded
while the task is still `MERGE_CONFLICT`. The task is then `VALIDATING`
the result, which `Store.Publish` publishes (see
[Publication](publication.md)).
