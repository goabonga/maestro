# Turn lifecycle

A turn is one attempt of an agent at a task step. Package `internal/turn`
holds its state machine, persists it in the `turns` and `turn_events`
tables of the state database, and evaluates its timeouts.

## States

| State | Meaning |
| --- | --- |
| `PREPARED` | Created, not yet admitted to run. |
| `RUNNING` | The agent is working. |
| `WAITING_INPUT` | The agent waits for an intervention. |
| `VALIDATING` | The handoff artifact and the Git state are being validated (`handoff.EndTurn`, `handoff.EndRepair`); this is not task integration. |
| `SUCCEEDED` | Terminal: the turn was validated. |
| `INTERRUPTED` | Terminal: the turn was interrupted. |
| `FAILED` | Terminal: the turn failed or exceeded a bound. |

## Transitions

The table is explicit; any transition it does not list is refused with
`turn.ErrTransition` and leaves no trace.

| From | To |
| --- | --- |
| `PREPARED` | `RUNNING`, `INTERRUPTED`, `FAILED` |
| `RUNNING` | `WAITING_INPUT`, `VALIDATING`, `INTERRUPTED`, `FAILED` |
| `WAITING_INPUT` | `RUNNING` (after a validated intervention), `INTERRUPTED`, `FAILED` |
| `VALIDATING` | `SUCCEEDED`, `INTERRUPTED`, `FAILED` |

No transition leaves a terminal state.

`Store.Transition` writes the new state and its `turn_events` row in one
transaction. The update is conditional on the state it read, so two
concurrent transitions of the same turn cannot both apply: the loser
gets `ErrTransition`. Creation is recorded as an event from the empty
state to `PREPARED`, so the events of a turn replay its whole history.

## Configuration and timeouts

Every turn carries the `config_id` of a stored configuration snapshot;
the database refuses a turn whose snapshot is not stored. When the turn
is created, its bounds are resolved from that snapshot and stored in
the turn:

- the turn timeout: `budgets.turn_timeout` (default `20m`), replaced by
  `agents.<name>.turn_timeout` when the agent sets one;
- the input-wait timeout: `budgets.input_wait_timeout` (default `5m`).

They are never read again from configuration files afterwards.

The turn timeout counts from admission, the first entry into `RUNNING`,
and covers every later non-terminal state, input waits included. Each
input wait is also bounded on its own, from the moment the turn entered
`WAITING_INPUT`. `Turn.Expired(now)` reports which bound has passed
(the earlier one when both have), and `Turn.Deadline()` the next instant
at which the turn expires. A turn not yet admitted or already ended
never expires. `Store.Expire` fails an expired turn with the bound as
its reason. The store reads time from an injectable clock (`Store.Now`),
so expiry is decided without waiting.

## Retries

A turn is never reused. `Store.Retry` accepts only a terminal turn
(`ErrNotTerminal` otherwise) and creates a new `PREPARED` turn with a new
`turn_id` and `attempt_id`, the same task, agent and `config_id`, linked
to the previous turn by `previous_turn_id`. A turn is retried at most
once (`ErrRetried`): further retries chain from the latest turn.
