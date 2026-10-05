# Sync operation

A sync brings the integration branch of a project to a commit of its user
repository. It is a `SYNC` operation of the
[operations journal](operations.md), run by `integration.Syncer` in two
phases, and served by the daemon under `/v1/syncs`.

## Prepare

`Syncer.Prepare(SyncRequest)` reads, checks and journals; it changes no
Git reference. In order:

1. A sync needs a test runner (`Syncer.Tester`) and at least one test
   command, or it fails with `ErrSyncUntested`: the journal commits a
   `SYNC` only once it is tested.
2. The branch name is screened (no leading `-`, no whitespace) and
   checked with `git check-ref-format refs/heads/<branch>`; the commit it
   points to is read with
   `git rev-parse --verify --quiet --end-of-options refs/heads/<branch>^{commit}`.
   This SHA is frozen for the whole operation. A bad or missing branch
   fails with `ErrSyncSource`.
3. A source equal to the integration head fails with `ErrSyncUpToDate`.
4. `git merge-base --is-ancestor <integration head> <source>` runs in the
   user repository: a source that descends from the integration head
   reaches it, so the user repository holds it and no import is needed
   for the check. A divergence fails with `ErrSyncDiverged`, naming both
   SHAs.
5. The operation is journaled in `PREPARED` with
   `integration_base_sha` (the head it advances from) and
   `source_head_sha` (the frozen SHA), under the worker `daemon` and a
   fresh attempt id.

A refusal at any of these steps journals nothing. The returned `SyncPlan`
also carries diagnostics on the uncommitted work of the user repository:
`git diff-index --cached --quiet HEAD` for staged changes,
`git diff-files --quiet` for unstaged changes and
`git ls-files --others --exclude-standard` for untracked files. When the
user repository configures a `filter.*` driver, `diff-files` is skipped
(it could run a clean filter) and a diagnostic says so.

Commands on the user repository run with `GIT_DIR` set to it, without
system or global configuration, with `core.hooksPath=/dev/null`,
`core.fsmonitor=false` and `--no-optional-locks`: they only read.

## Run

`Syncer.Run(SyncPlan)` carries the operation to its end:

| Step | Git | Journal |
| --- | --- | --- |
| start | | `Start`, the source branch in the reason |
| import | `git fetch --no-tags --no-write-fetch-head --no-recurse-submodules --no-auto-maintenance -- <user repository> <sha>:refs/maestro/sync/<id>` in the canonical repository | |
| re-check | the imported ref holds the SHA; `merge-base --is-ancestor` again | |
| candidate | tree of the SHA | `RecordCandidate(id, refs/maestro/sync/<id>, tree)` |
| pin | `CreateResultRef`: `refs/maestro/operations/<id>/result` created at the SHA with the null old value | `RecordResult` → `APPLIED` |
| tests | `SyncTester.Run` in `<project>/operations/<id>/tests` | reports accepted as `TEST_REPORT` artifacts, then `MarkTested` → `TESTED` |
| publish | `git update-ref refs/heads/maestro/integration <sha> <previous>` | `Commit` → `COMMITTED` |

Canonical repository commands go through the same controlled runner as the
result references: no system or global configuration, no hooks, no
signing, `core.fsync=committed`. The fetch never writes to the user
repository.

The test reports carry the artifact ids `sync-<operation id>-<test name>`;
their task, turn, attempt, worker and configuration fields are the
operation id, its attempt, the worker `daemon` and the operation's
`config_id`. Every report is kept, passed or not; the clone is removed
after a pass and kept after a failure.

Any failure before `update-ref` fails the operation with its cause and
rolls it back: a failed test (`ErrSyncTestsFailed`, naming the tests, the
report ids and the kept clone), a test run that could not happen, or an
integration head that moved since `Prepare` (`ErrSyncConcurrent`, naming
the expected and found heads). The integration branch is never reset; the
sync ref and the result reference stay for diagnosis. Once `update-ref`
has succeeded the operation is only ever committed.

Tasks keep the `task_base_sha` they were created with; a sync changes no
task.

## Daemon routes

| Route | Body | Answer |
| --- | --- | --- |
| `POST /v1/syncs` (Idempotency-Key) | `{"project_id", "branch"}` | `202` with the prepared operation, its branch and diagnostics |
| `GET /v1/syncs/{id}?project_id=` | | `200` with the operation |

`POST` takes the configuration snapshot of the user repository the way
task creation does, reserves one `tests` slot of the daemon capacity
(released when the sync ends), runs `Prepare` and answers; `Run` then
continues in the background, so a long test run outlives the request.
`ErrSyncSource` maps to `400 invalid_request`; `ErrSyncUpToDate`,
`ErrSyncDiverged`, `ErrSyncUntested` and a full capacity to
`409 conflict`.

The view carries `operation_id`, `project_id`, `state`, `previous_sha`,
`synced_sha`, `config_id`, `test_report_ids`, `error` and the
timestamps. `maestro sync` posts the request, then polls the operation
until it is `COMMITTED` or `ROLLED_BACK`.

`maestro-svc` gives its syncer a `testrun.Runner` on a probed launcher;
on a host that cannot confine there is no runner and every sync is
refused. When the daemon stops, `Server.Wait` lets the running syncs
finish before the store closes.
