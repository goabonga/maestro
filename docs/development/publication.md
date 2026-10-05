# Publication and finalization

Once an integration is `APPLIED`, its result commit is tested, published
on the private integration branch and finalized. The
`internal/integration` package implements these last steps in
`publication.go`, on top of the [operations journal](operations.md).

## Recording the test reports

`Store.RecordTestReports(project, id, reportIDs)` attaches accepted
`TEST_REPORT` artifacts to an `APPLIED` integration. Each report is read
from the `artifacts` table and checked with `testrun.ForTask`: it must
belong to the operation's task, have been produced under the
configuration snapshot the task runs on, and have tested the operation's
`result_sha` exactly. The task's snapshot must also be the one the
operation was prepared under. A report that is unknown, bound to another
revision or to another snapshot is refused and the operation is left
unchanged.

| Reports | Outcome |
| --- | --- |
| every report passed | `APPLIED → TESTED`, the report ids stored as evidence |
| at least one report failed | `APPLIED → FAILED → ROLLED_BACK`, `ErrTestsFailed` |

A report passes when its command exited 0 in time without changing the
tracked files or the index (`testrun.Passed`). On failure the operation's
error names the result and the failing reports. Nothing is deleted: the
reports stay in the `artifacts` table, and the result reference and the
candidate stay in place for diagnosis.

## Rolling back

`Store.RollBackIntegration(project, id, cause)` moves an unfinished
integration to `FAILED` with its cause (unless it already failed), then to
`ROLLED_BACK` once the integration branch is confirmed not to hold the
operation's result. The branch is never reset: a failed integration has
not moved it. A branch already at the result is refused with
`ErrPublished` and the operation stays `FAILED`.

## Publishing

`Store.Publish(project, id)` publishes a `TESTED` integration. Before any
Git mutation it checks that:

- the durable reference `refs/maestro/operations/<id>/result` holds the
  recorded `result_sha` and that commit is the valid result of the
  operation's frozen inputs (`ProveResult`);
- the task runs on the operation's configuration snapshot and accepts
  `validation-pass` on that result, that is it is `VALIDATING` it;
- the integration view holds no unexplained change (below).

The publication itself is one compare-and-swap in the canonical
repository:

```
git update-ref refs/heads/maestro/integration <result_sha> <integration_base_sha>
```

A branch that is no longer at the operation's integration base is refused
with `ErrBranchMoved`; the operation stays `TESTED` and the task
`VALIDATING`. Once the branch holds the result, one SQLite transaction
moves the operation to `COMMITTED` and the task to `DONE`: both happen, or
neither does. The task joins the transaction through
`task.Store.TransitionTx`, which applies an event inside a caller's
transaction.

### Crash between the update-ref and the SQLite transaction

A crash after the update-ref and before the SQLite commit leaves the
branch at the result, the result reference valid and the operation
`TESTED`. The state is recognized from the durable reference: calling
`Publish` again proves the result, sees the branch already at it, skips
the update-ref and only finalizes `COMMITTED` and `DONE`.

## Integration view

The integration view is a detached worktree of the canonical repository
at `worktrees/integration/` under the project directory, written only by
the daemon. `RefreshIntegrationView(project)` creates it at the commit of
`refs/heads/maestro/integration` when it is missing, and otherwise moves
it there with a detached checkout. `Publish` refreshes it after the
finalization; a refresh error is returned together with the committed
operation.

Before a publication and before a refresh, the view must be:

- a directory at the top of a worktree of the canonical repository;
- detached, at the published commit or one of its ancestors;
- clean: no change to tracked files or the index, no untracked or
  ignored file.

Any other state is an unexplained change: it fails with
`ErrIntegrationView`, forbids the publication and the refresh, and leaves
the view untouched.

Every Git command runs with fixed verbs and validated object ids, without
system or global configuration, hooks or signing.
