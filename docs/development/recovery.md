# Operation recovery

After a restart, every operation of the [operations journal](operations.md)
that is not terminal is reconciled from what can be observed, never from
guesses. The `internal/integration` package implements the decision table
in `recovery.go`. A crash never turns an operation into a success: an
operation is finalized only when its target already holds its proven and
tested result, and an abandoned operation is never published.

## Entry points

`Store.Recover(project, runtime)` reconciles one project and returns the
decisions it took, in order. The caller holds the project lock and starts
no worker before it returns. `runtime` is the project's runtime path list,
used when a candidate is rebuilt. The error only reports a failure to read
the journal; every other problem becomes a blocking decision.

`Store.DecideRecovery(project, id)` returns the decision for one
unfinished operation without applying it: it only reads SQLite and Git.

Each `Decision` names the operation, the state it was observed in
(`From`), the `Action`, the commit held by the durable result reference
(`ResultRef`), the commit held by the target reference (`Target`), whether
the action was applied, the state after it (`To`) and a diagnostic. A
blocking decision carries an `Err` wrapping `ErrRecoveryBlocked` and the
cause, and `Blocked()` reports it.

## Order

1. The integration view: when the integration branch holds the result of
   a committed INTEGRATE or SYNC operation and the view is missing or
   behind it, the view is refreshed. Nothing is replayed. A view holding an
   unexplained change blocks.
2. `FAILED` operations.
3. Unfinished INTEGRATE and SYNC operations.
4. Unfinished PUBLISH operations.

An action that leaves the operation further along but not terminal
(recording a result, rebuilding a candidate) is followed by a new
decision for the same operation, a bounded number of times.

## Observations

For INTEGRATE and SYNC, recovery reads the journaled state and result, the
durable reference `refs/maestro/operations/<id>/result`, and the position
of `refs/heads/maestro/integration` in the canonical repository.

- An integration's reference must pass `ProveResult`: the commit has the
  integration base as its only parent, the recorded candidate tree and the
  frozen metadata.
- A sync's reference must hold its frozen imported commit
  (`source_head_sha`), descending from its base. No aggregated commit is
  required.
- A result in the journal must match the reference; a result without its
  reference, or a reference holding something else, blocks.

The branch is classified as at the base, at the result, holding the result
in its history, or at the result of another committed operation that
descends from the base (a stale base). Anything else is unexpected.

## INTEGRATE decisions

| Observed state | Decision |
| --- | --- |
| No result reference, branch at the base | `rebuild`: a clean leftover candidate is removed and the candidate is built again from the persisted inputs with `BuildCandidate`; a candidate holding changes is kept and the operation is abandoned |
| Valid reference, journal without `result_sha` (`STARTED`) | `record-result`: `ApplyIntegration` records `APPLIED` from the proof |
| Valid reference, branch at the base, `APPLIED` | `retest`: left to the caller |
| Valid reference, branch at the base, `TESTED` with valid evidence | `retry-publication`: `Publish` is called once |
| Branch at the result, `TESTED` with valid evidence | `finalize`: `Publish` skips the update and commits the operation and the task |
| `FAILED`, branch at the base or a stale base, result not on the branch | `roll-back`: `RollBackIntegration`, candidate and diagnostics kept |
| Branch unexpected, inconsistent proof, or result on the branch without `TESTED` | `block`: nothing is rewritten or deleted |
| Stale base | `abandon`: a new base requires a new operation |
| Operation superseded, or its task cancelled | `abandon`, unless its result is already on the branch, which blocks |

Evidence of a `TESTED` integration is valid when it has at least one test
report, every report is accepted, belongs to the task and to its current
configuration snapshot (which must be the operation's), tested the result
exactly and passed, and the task is `VALIDATING` that result.

A failed retry or finalization that leaves the operation unchanged becomes
a blocking decision with its cause, for example `ErrIntegrationView` or
`ErrBranchMoved`; it is not repeated within the same recovery. A rebuild
ending in a conflict or an empty candidate leaves the operation `FAILED`,
which the next decision rolls back.

## SYNC decisions

| Observed state | Decision |
| --- | --- |
| No reference, branch at the base | `abandon`: the sync is run again by the user |
| Valid reference, `STARTED` | `record-result`: `RecordResult` from the pinned commit |
| Valid reference, branch at the base, `APPLIED` | `retest` |
| Valid reference, branch at the base, `TESTED` with valid evidence | `retry-publication`: one compare-and-swap of the branch from the base, after checking the view, then `Commit` and a view refresh |
| Branch at the imported commit, `TESTED` with valid evidence | `finalize`: `Commit` and a view refresh |
| `FAILED`, branch without the imported commit | `roll-back` |
| Anything else | `block` |

Evidence of a `TESTED` sync is valid when every test report is accepted,
tested the imported commit exactly and passed.

## PUBLISH decisions

The target is `refs/heads/maestro/integration` of the user repository,
read without hooks, file-system monitor or optional locks. The operation
froze the old value as its integration base (the null id when the branch
was absent) and the published commit as its source head.

| Observed state | Decision |
| --- | --- |
| Branch at the old value, `PREPARED` or `STARTED` | `abandon`: the user publishes again |
| Branch at the target, `STARTED` or `APPLIED` | `finalize`: `RecordResult`, then `Commit` with the evidence of the committed operations it publishes |
| `FAILED`, branch at the old value | `roll-back` |
| Anything else | `block` |

A publication is finalized only when its target is the result of a
committed, tested INTEGRATE or SYNC operation of the project.

## Left to the caller

`retest` is the only decision that changes nothing and does not block:
the tests run again in a new clean clone of the same result, then the
reports are recorded with `RecordTestReports` for an integration, or
`MarkTested` for a sync. Recovery does not move tasks other than through
`Publish`: a rolled back or abandoned integration leaves its task for the
workflow to settle. Blocking decisions do not write anything; they are
returned for the daemon to report.
