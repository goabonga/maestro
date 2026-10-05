# Publication to the user repository

`maestro publish` exports the head of a project's private integration
branch to its user repository. The work is done by
`integration.Store.PublishToUser(project)` in the daemon, behind the
`POST /v1/projects/{id}/publish` route.

## What is written

The only reference ever written in the user repository is
`refs/heads/maestro/integration` (`integration.UserPublishRef`). The
user's current branch, index and worktree are never touched. The branch
is created when absent; otherwise it may only be fast-forwarded.

## Steps

All publications of one project are serialized inside the daemon.

1. Unfinished `PUBLISH` operations of the project are reconciled first
   (below).
2. The head of `refs/heads/maestro/integration` in the canonical
   repository is read and frozen. It must be the `result_sha` of a
   `COMMITTED` `INTEGRATE` or `SYNC` operation of the project; otherwise
   the publication is refused with `ErrPublishUntested`.
3. The current value of the user branch is read. Already at the head:
   nothing is journaled and the publication reports `up_to_date`.
4. Checks, before any journal entry:
   - the previous value must be known to the canonical repository and be
     an ancestor of the head, or the publication is refused with
     `ErrPublishDiverged`, naming both commits;
   - the branch must not be checked out in any worktree of the user
     repository, the main one included (`git worktree list`), or the
     publication is refused with `ErrPublishCheckedOut`, naming the
     worktree.
5. A `PUBLISH` operation is prepared: `integration_base_sha` holds the
   previous value of the user branch (the null object id when it is
   absent), `source_head_sha` and `source_commits` the frozen head, and
   `config_id` the configuration snapshot of the operation that produced
   it. Its `worker_id` is `maestro-svc`. The start event records the
   destination.
6. The objects are transferred with `git fetch` run in the user
   repository from the canonical one, by object id, with
   `--no-write-fetch-head`, no refspec, no tags, no submodules and no
   automatic maintenance: no reference and no `FETCH_HEAD` is created.
7. The worktree check is repeated, then
   `git update-ref refs/heads/maestro/integration <head> <previous>`
   updates the branch only if it still holds the value read in step 3.
   A concurrent change is refused with `ErrPublishConcurrent`, naming
   the value found.
8. The operation is recorded `APPLIED` with the head as `result_sha`,
   then `COMMITTED` with the evidence of the operations it publishes: the
   test reports, reviews and approvals of every committed `INTEGRATE` or
   `SYNC` operation whose result is reachable from the head and not from
   the previous value.

A failure in steps 6 or 7 leaves the user branch untouched: the
operation moves to `FAILED` with its cause, then `ROLLED_BACK`.

## Reconciliation

An unfinished `PUBLISH` operation left by a crash is settled before a new
publication, from the current value of the user branch:

| Operation | User branch | Decision |
| --- | --- | --- |
| `PREPARED` | any | nothing was transferred: `FAILED`, then `ROLLED_BACK` |
| `STARTED` | at its head | the update happened: `APPLIED`, then `COMMITTED` |
| `STARTED` | at its previous value | the update did not happen: `FAILED`, then `ROLLED_BACK` |
| `STARTED` | anything else | refused with `ErrPublishConcurrent`; nothing is rewritten and the operation stays |
| `APPLIED` | any | `COMMITTED` with its evidence |
| `FAILED` | any | `ROLLED_BACK` |

## Controlled Git environment

Every Git command against the user repository runs with `GIT_DIR` set to
its common Git directory, without system or global configuration, with
`core.hooksPath=/dev/null` so no hook of the user repository runs (the
`reference-transaction` hook included), `core.fsmonitor=false`, no
signing, and only the local file transport allowed. Arguments are fixed
verbs, validated object ids and paths owned by Maestro.

## Route

`POST /v1/projects/{id}/publish` takes no body and requires an
`Idempotency-Key`. It replies with `project_id`, `reference`,
`previous_sha` (absent when the branch was created), `published_sha`,
`up_to_date`, and, when a publication was journaled, `operation_id` and
`state`. A refused publication is a `409 conflict` whose message names the
cause; an unknown project is a `404`, a missing user repository a `409`.
