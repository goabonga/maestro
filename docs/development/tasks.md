# Task workflow

A task is one unit of work driven from `NEW` to `DONE`. Package
`internal/task` holds its state machine, persists it in the `tasks` and
`task_events` tables of the state database, and refuses every
transition its table does not list.

## States

| State | Meaning |
| --- | --- |
| `NEW` | Created, waiting for an assignment. |
| `PLANNING` | The plan is being produced. |
| `IMPLEMENTING` | The code is being written. |
| `TESTING` | The tests run on the current revision. |
| `REVIEWING` | The current revision is under review. |
| `FIXING` | A correction is being written. |
| `READY_TO_INTEGRATE` | Every required review approved the current revision. |
| `INTEGRATING` | The integration candidate is being built. |
| `MERGE_CONFLICT` | The integration found a conflict to resolve. |
| `VALIDATING` | The integration candidate is under test. |
| `BLOCKED` | Stopped, with its reason and the state to resume in. |
| `DONE` | Terminal: integrated and published. |
| `CANCELLED` | Terminal: cancelled. |

## Transition table

The table is the single authority. Each row is an event accepted in a
set of states, with a guard and an effect. An event a state does not
list fails with `task.ErrTransition`; an unmet guard fails with
`task.ErrGuard`. Neither leaves a trace.

| From | Event | Guard | Effect and next state |
| --- | --- | --- | --- |
| `NEW` | `assign` | an assignment is available | `PLANNING` |
| `PLANNING` | `accept-plan` | the plan is valid | `IMPLEMENTING` |
| `IMPLEMENTING` | `implement` | the artifact is valid and the revision is a commit id | freeze the revision to test; `TESTING` |
| `FIXING` | `fix` | the artifact is valid and the revision is a commit id | freeze the new revision, drop the approvals; `TESTING` |
| `TESTING` | `tests-pass` | about the current revision | `REVIEWING` |
| `TESTING` | `tests-fail` | about the current revision | a correction (below) |
| `REVIEWING` | `request-changes` | about the current revision | a correction (below) |
| `REVIEWING` | `approve` | about the current revision; every required review approves | freeze the approval; `READY_TO_INTEGRATE` |
| `READY_TO_INTEGRATE` | `integrate` | a `human` or `auto` trigger; the approval is still on the current revision | `INTEGRATING` |
| `INTEGRATING` | `build-candidate` | the candidate is a commit id | freeze `result_sha`; `VALIDATING` |
| `INTEGRATING` | `conflict` | the integration base is a commit id | `MERGE_CONFLICT` |
| `MERGE_CONFLICT` | `resolve-conflict` | the proposal is valid, its resolved diff tested and reviewed, approved by a human when the gate requires it, and fewer than two resolutions failed on this base | `INTEGRATING` |
| `MERGE_CONFLICT` | `reject-resolution` | none | count the failure; `MERGE_CONFLICT` after the first, `BLOCKED` after the second |
| `MERGE_CONFLICT` | `approve-resolution` | a `human` trigger on a tested and reviewed resolution | record the approval of that resolution; `MERGE_CONFLICT` |
| `VALIDATING` | `validation-pass` | about the current candidate; the publication succeeded | `DONE` |
| `VALIDATING` | `validation-fail` | about the current candidate; the rollback is confirmed | a correction (below) |
| every non-terminal state but `BLOCKED` | `block` | a reason is given | keep the current state as continuation; `BLOCKED` |
| `BLOCKED` | `resume` | the cause is lifted and the step reconciled | the stored `resume_state` |
| every non-terminal state | `cancel` | processes stopped, no pending operation, no publication done | `CANCELLED` |

No event leaves `DONE` or `CANCELLED`.

A correction enters `FIXING` while fix cycles remain: it increments the
fix counter and drops the approval and the candidate. When the counter
has reached its bound (`DefaultMaxFixCycles`, 3), the task blocks with
the reason `fix cycles exhausted` and resumes in the state where the
correction was needed. Blocking in `PLANNING` resumes in `PLANNING` and
consumes no fix cycle.

The guards read their facts from `task.Guard`: assignments, artifacts,
reviews, tests, integration and process supervision report them with
the event. A fact left unset does not hold.

## Stale results

Test results, reviews and validation outcomes name the revision they
are about. When it is not the task's current revision (or candidate),
the event fails with `task.ErrStale`: it is recorded in `task_events`
with `stale = 1` and the same state on both sides, and the task does
not change.

## Persistence

`Store.Create` stores a `NEW` task with:

- its `config_id`, which must name a stored configuration snapshot;
- its `task_base_sha`, a full commit id;
- its branch `maestro/task-<id>` (`worktree.TaskBranch`);
- its fix cycle bound.

Creation is recorded as a `create` event from the empty state to `NEW`.

`Store.Transition` evaluates the event, then writes the task and its
`task_events` row in one transaction. The update applies only if the
task is still at the state and version it read, so a concurrent
transition makes the later one fail with `ErrTransition` instead of
overwriting it. The database refuses a `BLOCKED` task without its
`resume_state` and `blocked_reason`, so the continuation of a blocked
task is always durable. Conflict failures are counted per integration
base: a conflict on another base starts a new count.
