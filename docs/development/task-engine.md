# Task engine

`worker.Engine` assigns the tasks of a project to its `IDLE` workers and
drives them turn by turn through the sequential workflow, until each task
is `READY_TO_INTEGRATE` or `BLOCKED`. It applies the transitions of the
task workflow ([Task workflow](tasks.md)), the worker lifecycle
([Worker registry](workers.md)) and the turn lifecycle
([Turn lifecycle](turns.md)); it adds no state of its own.

## Interfaces

The engine never writes to a PTY. It drives a worker through its live
session, the `worker.Session` interface, found by `worker.Sessions`:

| Method | Contract |
| --- | --- |
| `Checkout(task, revision)` | Prepare the worktree for a turn: the task branch at the revision, without uncommitted changes. Returns the worktree, where the handoff is read and the commits are verified. |
| `RuntimePaths()` | The provisioned paths a commit of the task must not add or modify. |
| `Send(prompt)` | Admit the turn: grant its rights, write the prompt, start the detection of its end. |
| `Poll()` | The detection of the current turn (`agent.Detection`). |
| `Interrupt()` | Stop the current turn. |
| `Settle()` | Revoke the turn's rights; once it returns nil the agent can no longer write the task's sources. |
| `Close()` | End the session of a worker the engine failed: its agent terminated, its session slot released. |

Tests run through `worker.Tester`, whose signature `*testrun.Runner`
implements; commits are imported into the canonical repository with
`worktree.Store.Import`. The stores share the engine's `Now` clock, and
`Wait` sleeps between two polls of a running turn (`PollInterval`,
500 ms by default).

## Driving

`Engine.Drive(ctx, project)` first fails the workers whose input wait
expired and recovers the turns no `Drive` follows any more (below), then
takes the tasks of the project oldest first, one at a time, and runs
their steps until the task cannot progress: it is `READY_TO_INTEGRATE`,
`BLOCKED` or terminal, or its next step finds no `IDLE` worker with a
live session. A task a live worker holds an assignment on is skipped.
`Drive` returns once no task can progress, or with the context's error
or the first failure of a store.

One `Drive` at a time advances a project, across every `Engine` of the
process sharing the same database: a `Drive` called meanwhile returns at
once, and the running one makes one more pass before it returns, so no
task created in between waits for a later call.

A `Drive` that returns on an error in the middle of a step does not leave
it stranded: it closes the task's active interval, and the turn its
worker is still `BUSY` with is interrupted (`INTERRUPTED`), the worker
`FAILED` and its session closed, and the task `BLOCKED` with its
continuation; a task whose worker waits for input is blocked. What a
failing store keeps it from applying, or what a stopped process left, is
recovered by the next `Drive`: every `BUSY` worker holding an assignment
has its turn ended the same way, since no other `Drive` follows a turn of
the project, the open interval of its task being charged up to now
(`budget.Store.Recover`) and a task out of time blocked on its budget.
A worker is failed only if it is still at the state and version it was
read at, `BUSY` with the same turn, before anything else is touched: a
worker whose turn ended meanwhile, now `IDLE` or `BUSY` with another
turn, keeps its session, its task and its active interval.

| Task state | Step | Role | Handoff | Revision checked out |
| --- | --- | --- | --- | --- |
| `NEW`, `PLANNING` | worker turn | `planning` | `PLAN` | task base |
| `IMPLEMENTING` | worker turn | `implementation` | `IMPLEMENTATION` | task base |
| `TESTING` | test run | none | `TEST_REPORT` | revision under test |
| `REVIEWING` | worker turn | `review` | `REVIEW` | revision under review |
| `FIXING` | worker turn | `correction` | `IMPLEMENTATION` | revision to fix |

The worker is the first `IDLE` worker of the project, by name, with a
live session.

## A worker turn

1. The turn is created (`PREPARED`) with the bounds of the task's
   configuration snapshot, and one turn of the task's budget is reserved
   under the turn id (`budget.Store.Reserve`). A reservation over a cap
   fails the turn and blocks the task with the bound as its reason; the
   worker is not assigned.
2. The worker takes the assignment (`IDLE` to `BUSY`). A `NEW` task then
   takes the `assign` event to `PLANNING`.
3. The task's active interval opens for the step (`budget.Store.Start`);
   an interval a crash left open is charged first. A task out of time
   abandons the turn (`INTERRUPTED`), releases the worker and blocks.
4. The session checks the revision out and receives the role's prompt;
   the turn is `RUNNING`.
5. The engine polls the detection until the turn ends:

| Detection | Effect |
| --- | --- |
| `COMPLETED` | the turn's rights revoked (`Settle`), turn `VALIDATING`; the handoff is decided on (below) |
| `WAITING_INPUT` | turn and worker `WAITING_INPUT`, the worker keeping the assignment; task `BLOCKED` |
| `FAILED` | turn `FAILED`, worker `FAILED`, task `BLOCKED` |
| `INTERRUPTED` | turn `INTERRUPTED`, worker `FAILED`, task `BLOCKED` |
| `RUNNING`, `UNKNOWN` | wait, until the turn's timeout fails the turn, the worker and the task |

Every worker the engine fails has its session closed: the agent is
interrupted, then its confined group terminated and its session slot
released, so a failed worker keeps no agent running and holds no slot.
A blocked task keeps its
current state as its continuation, so `maestro task resume` returns it
to the step that failed, with a new turn.

An input wait stays bounded: once its `input_wait_timeout` passes,
`Drive` fails the turn with `input wait timeout`, fails the worker and
closes its session.

## Prompts

Each prompt names the worker, the role and the task with its
description, then gives the role's input:

- `planning`: write a plan, without modifying any file;
- `implementation`: the last accepted plan, to implement on the task
  branch from the task base;
- `correction`: the fix request of the revision to fix, with the review
  issues it references or the failed tests and the end of their log
  (4 KiB at most);
- `review`: the revision, the task base and the SHA-256 of their binary
  diff (`git diff --binary base head`), without modifying any file.

It ends with the path of the handoff document, written to `.tmp` then
renamed, and the document itself: every identifier the engine checks
(`artifact_id`, `task_id`, `turn_id`, `attempt_id`, `worker_id`,
`config_id`, `input_artifact_ids`, `task_base_sha` and, for a review,
`source_head_sha`), with placeholders for what the agent fills in.

## Handoff decision

`handoff.EndTurn` reads the document of the turn's attempt. A valid
document is then checked against Git and the task:

- an implementation: its commits are the first-parent chain from the
  task base (`handoff.VerifyCommits`), none adds or modifies a runtime
  path (`.maestro/` or the session's `RuntimePaths`), and the task
  branch, imported under `refs/maestro/workers/<worker>/<branch>` of
  the canonical repository, is at its source head;
- a review: it is about the revision under review, and its diff digest
  is the digest of that revision's diff from the task base.

| Outcome | Effect |
| --- | --- |
| valid | the artifact is accepted, the turn `SUCCEEDED`, the worker released; then the task transition |
| missing or invalid document, or a check that does not hold | the turn `FAILED`; one format repair |
| a commit of a runtime path, or an ambiguous read | the turn `FAILED`, the worker released, the task `BLOCKED` |

The format repair runs in a new turn and attempt that retries the
failed one (`turn.Store.Retry`), on the same worker, which is released
in between: it reserves a turn of the budget like any other. Its prompt
gives the refusal and asks for the document only. `handoff.EndRepair`
then blocks the task if the repair changed the HEAD, the index or the
files, or if its document is missing or invalid again; a document that
fails the checks above blocks it as well.

The rights of a completed turn are revoked before its handoff is read:
the session settles as soon as the end of the turn is detected, so the
agent cannot change the worktree while the engine reads and verifies
it. A session that cannot confirm the revocation fails the turn and the
worker, closes the session and blocks the task, without reading the
handoff. Once the worker is released, the step's interval is closed; a
task that exceeded its time budget meanwhile blocks instead of moving
on.

| Accepted handoff | Task event |
| --- | --- |
| `PLAN` | `accept-plan` |
| `IMPLEMENTATION` in `IMPLEMENTING` | `implement` at its source head |
| `IMPLEMENTATION` in `FIXING` | `fix` at its source head |
| `REVIEW` approving | `approve` of the revision under review |
| `REVIEW` requesting changes | `request-changes`, with a fix request referencing its issues |

## Test run

A task in `TESTING` runs the test commands of its configuration
snapshot on its revision under test, in a clone under
`<project>/tests/<task-id>/<run-id>/`, during an active interval of the
`tests` step. One `TEST_REPORT` per command is accepted, as worker
`maestro`, under the turn id `tests-<run-id>`. Passing tests remove the
clone and take `tests-pass`; failing ones keep it and take `tests-fail`,
with a fix request referencing the reports of the failed commands. A
snapshot without a test command, an engine without a tester or a run
that cannot start blocks the task.

A fix request (`FIX_REQUEST`, worker `maestro`) is accepted before the
task enters `FIXING`, about the revision under test or review; none is
produced when the fix cycles are exhausted, the task then being
blocked.

## Progress

Every step is recorded by the stores it goes through: the task's
transitions with their reasons and revisions (`maestro task show`), the
worker's events with the task and turn they are about
(`maestro worker show`), the turns and their events, the accepted
artifacts and the consumed budgets.
