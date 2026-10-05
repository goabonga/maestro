# Operations journal and result references

Every change to the integration branch is journaled as an operation
before Git is touched. The `internal/integration` package keeps that
journal in SQLite and holds the durable Git proof of an integration's
result in the canonical repository.

## Operations

An operation has a type:

| Type | Meaning |
| --- | --- |
| `INTEGRATE` | one aggregated commit built from a task's work |
| `SYNC` | the integration branch brought to an imported commit |
| `PUBLISH` | the publication of operations already tested and committed |

The `operations` table (schema version 9) stores, per operation: its
`id` (a UUID), `type`, `state` and a `version` used for compare-and-set;
`project_id`, `config_id`, `task_id` (required for `INTEGRATE`, optional
otherwise), `worker_id`, `attempt_id` and `supersedes_operation_id`; the
frozen Git inputs `task_base_sha`, `integration_base_sha`,
`source_head_sha` and `source_commits` (the ordered chain, oldest first);
`candidate_ref`, `candidate_tree_sha` and `result_sha`; the frozen
`commit_metadata`; the evidence `test_report_ids`, `review_artifact_ids`
and `approval_artifact_ids`; `error`; `started_at` (when the operation was
prepared) and `updated_at`. Every change is appended to
`operation_events`.

The schema itself refuses an integration without its task or commit
metadata, a `PUBLISH` operation in `TESTED`, an applied, tested or
committed operation without its result, a failed or rolled back operation
without its error, and an operation superseded twice.

## Transition table

`integration.Allowed(type, from, to)` is the single authority on
transitions:

| Type | Success path | Failure path |
| --- | --- | --- |
| `INTEGRATE`, `SYNC` | `PREPARED → STARTED → APPLIED → TESTED → COMMITTED` | any non-terminal state `→ FAILED → ROLLED_BACK` |
| `PUBLISH` | `PREPARED → STARTED → APPLIED → COMMITTED` | any non-terminal state `→ FAILED → ROLLED_BACK` |

`COMMITTED` and `ROLLED_BACK` are terminal. A failed operation can only
roll back: no transition turns an unfinished or failed operation into a
success, and nothing commits an operation implicitly.

Each `Store` method maps to one step and its guard:

| Method | Step | Guard |
| --- | --- | --- |
| `Prepare` | `→ PREPARED` | complete and well-formed inputs |
| `Start` | `PREPARED → STARTED` | |
| `RecordCandidate` | `STARTED → STARTED` | the candidate tree is frozen once recorded |
| `ApplyIntegration` | `STARTED → APPLIED` | `INTEGRATE` only: a valid result reference (below) |
| `RecordResult` | `STARTED → APPLIED` | `SYNC` and `PUBLISH` only; `SYNC` needs its candidate tree |
| `MarkTested` | `APPLIED → TESTED` | at least one test report |
| `Commit` | `TESTED → COMMITTED`, or `APPLIED → COMMITTED` for `PUBLISH` | at least one test report |
| `Fail` | `→ FAILED` | a non-empty error |
| `RollBack` | `FAILED → ROLLED_BACK` | |

A transition is stored with its event in one transaction, and only if the
operation is still at the state and version that were read: a concurrent
transition makes the later one fail with `ErrTransition` instead of
overwriting it. A refused transition leaves no trace.

## Prepared inputs

`Prepare` validates the inputs before any Git mutation. Object ids are
full SHA-1 or SHA-256 hexadecimal ids. An integration names its task, its
task base, its source head and its source commits, which must end at that
head, and freezes its commit metadata: message, author and committer, each
with a name, an email and a date to the second with its time-zone offset.
Names and emails Git would trim or reject are refused, and the message is
stored ending with exactly one newline, so the result commit can be
rebuilt from the same inputs. An operation may supersede another one of
the same project, task and type that is not committed.

## Durable result reference

The proof of an integration is the reference
`refs/maestro/operations/<id>/result` in the project's canonical
repository (`worktree.Project.Repository()`), which agents cannot reach.

- `BuildResult(repository, expected)` creates the result commit with
  `git commit-tree`: the candidate tree, the integration base as its only
  parent and the frozen metadata. The same inputs always build the same
  commit.
- `CreateResultRef(repository, id, sha)` runs
  `git update-ref <ref> <sha> <null id>`: the reference is created
  atomically and only if it does not exist. Repeating the creation at the
  same commit is accepted; a reference at another commit is never
  overwritten and fails with `ErrResultRefConflict`.
- `ValidateResult(repository, sha, expected)` reads the raw commit and
  accepts it only with exactly one tree, the expected one, exactly one
  parent, the expected base, the frozen author, committer and message, and
  no other header (no signature, no encoding). Anything else fails with
  `ErrInvalidResult`.
- `ProveResult(project, op)` reads the reference of an integration and
  validates its target against the operation's base, recorded tree and
  metadata. It reports a missing reference without error.

`Store.ApplyIntegration(project, id, sha)` validates the result, creates
its reference, validates the reference again and only then stores
`APPLIED` with `result_sha`. A crash between the reference and the journal
leaves a valid reference and an operation still in `STARTED`:
`ProveResult` finds it, and `ApplyIntegration` with the same result
completes the step. A reference at an unexpected commit is reported and
left untouched, and the operation stays where it was.

The presence of the source commits in the history of a commit is never
taken as proof of an integration: only the result reference and its
validation are.

Git commands run with fixed verbs and validated ids against the bare
repository (`GIT_DIR`), without system or global configuration, hooks or
commit signing, and with `core.fsync=committed` so objects and references
reach the disk before the journal is updated.
