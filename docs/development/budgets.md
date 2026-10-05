# Task budgets

Package `internal/budget` enforces the durable budgets of a task: the
turns it may consume and the time it may spend. Every bound is read from
the task's configuration snapshot (`config_id`), never from the current
configuration files, and every counter is stored in the state database
(tables `budget_turns` and `budget_time`, schema version 6). Counters
belong to the task and to the named agent only: they do not depend on a
worker, a session or an assignment, and a resume or a change of worker
never resets them.

## Configuration

```toml
[budgets]
max_turns_per_task = 30   # every role together, per task
task_timeout = "2h"       # cumulated active time
wall_timeout = "8h"       # optional: calendar time, waits included

[agents.reviewer]
max_turns_per_task = 2    # on top of the task total, never instead of it
```

`wall_timeout` is optional and must be positive when set. Leaving it
unset keeps the snapshot's encoding, so existing `config_id`s do not
change.

## Turn budgets

`Store.Reserve(task, agent, key)` consumes one turn before a prompt is
sent. The reservation is checked against both caps at once and stored in
the same SQL statement, so two concurrent reservations never both take
the last turn:

- the task total, `budgets.max_turns_per_task`;
- the agent's own cap, `agents.<name>.max_turns_per_task`, when set.

Every role, repair, conflict resolution and retry reserves its own turn.
A reservation is never refunded: a crash after it leaves the turn
consumed, even if the prompt was never sent. The key identifies the
send: reserving the same key again (an idempotent retry of the same
send) returns the first reservation with `Replayed` set and consumes
nothing, and the same key for another agent fails with
`ErrKeyConflict`. `Store.Turns` returns the consumed turns, in total
and per agent.

The fix-cycle limit (`max_fix_cycles`) is separate: it stays in
`internal/task` and counts entries into `FIXING`.

## Time budgets

`task_timeout` counts the cumulated time of the active steps only.
`StepOf` maps a task state to its step:

| Step | Task states |
| --- | --- |
| `planning` | `PLANNING` |
| `coding` | `IMPLEMENTING`, `FIXING` |
| `tests` | `TESTING` |
| `review` | `REVIEWING` |
| `integration` | `INTEGRATING`, `MERGE_CONFLICT`, `VALIDATING` |

`NEW`, `READY_TO_INTEGRATE`, `BLOCKED` and the terminal states are not
active: queues, pauses and human waits are not counted.

`Store.Start` opens an active interval for a task in an active step
(`ErrNotActive` otherwise; `ErrIntervalOpen` when one is already open).
The interval is measured in memory from the store's clock: with the
real clock, readings carry Go's monotonic reading. It is persisted at
its boundaries: `Interval.Checkpoint` records the elapsed time and keeps
the interval open, `Interval.Stop` adds it to the task's active time.
`Store.Check` returns the usage persisted so far.

`wall_timeout`, when set, bounds the calendar time since the task's
creation, waits included.

### Crash recovery

An interval left open by a crash is never confirmed. `Store.Recover`
closes it and charges it conservatively from the recorded timestamps:
from its recorded start up to the recovery instant, or the duration
last persisted at a checkpoint if that is longer. A new interval cannot
start before the open one is recovered.

### Clock regression

A clock that reads before a recorded timestamp (the task's creation,
the start or last boundary of an interval, the last update of the
account) fails with `ErrClockRegression` and a diagnostic naming both
instants; nothing is charged while the clock reads backwards.

## Exceeding a bound

A reached bound fails with an `*ExceededError` (wrapping `ErrExceeded`)
that names it by its configuration key, with its limit and the amount
used:

- `budgets.max_turns_per_task`;
- `agents.<name>.max_turns_per_task`;
- `budgets.task_timeout`;
- `budgets.wall_timeout`.

Time bounds are reached when the usage equals the limit: a task with no
time left cannot start an interval. `Checkpoint` and `Stop` record the
elapsed time before reporting a reached bound, so time is never lost.

`Store.Block(task, cause)` applies a budget failure or a clock
regression to its task through the workflow's block transition: the
task goes `BLOCKED` with the failure as its reason and its current
state as its continuation. Nothing is killed: the caller stops the
task's work as for any other block. Any other cause is refused with
`ErrNotBudget`. Consumed budgets are never reset.
