# Worker registry

A worker is one work slot of a project: an agent, its driver and a
private repository. Package `internal/worker` registers workers, holds
their lifecycle state machine, and persists both in the `workers` and
`worker_events` tables of the state database (schema version 10). Its
`Supervisor` starts and stops the workers' confined agent sessions
([below](#starting-and-stopping)) and exposes them to the task engine
([live sessions](#live-sessions)).

## Identity

`Store.Register` stores a worker with:

- its project ID and its name, unique in the project; the name is a
  safe identifier (lowercase letters and digits, single hyphens between
  them, at most 64 bytes), so it is also a path component;
- its agent, the agent's kind and the driver name;
- the path of its private repository, `worktree.Project.WorkerRepository`.

A name already registered in the project fails with `worker.ErrExists`;
an invalid name or a missing field fails with `worker.ErrInvalid`. The
same name may exist in another project. Registration is recorded as a
`register` event from the empty state to `STOPPED`.

`Store.Get` reads one worker, `Store.List` the workers of a project by
name, and `Store.Events` the recorded events of a worker, oldest first.

## States

| State | Meaning |
| --- | --- |
| `STOPPED` | Registered, no session. |
| `STARTING` | Its session is being started. |
| `IDLE` | Ready, without an assignment. |
| `BUSY` | Running its assigned turn. |
| `WAITING_INPUT` | Its turn waits for an input; the assignment is kept. |
| `ATTACHED` | A human drives the session exclusively. |
| `PAUSED` | Paused; it takes no new assignment. |
| `DRAINING` | Leaving the pool; it takes no new assignment. |
| `FAILED` | Stopped by an error or an inconsistency. |

Every state but `STOPPED` and `FAILED` is active.

## Transition table

The table is the single authority. Each row is an event accepted in a
set of states, with a guard and an effect. An event a state does not
list fails with `worker.ErrTransition`; an unmet guard fails with
`worker.ErrGuard`. Neither leaves a trace.

| From | Event | Guard | Effect and next state |
| --- | --- | --- | --- |
| `STOPPED` | `start` | a session slot is reserved | `STARTING` |
| `STARTING` | `ready` | the session is ready and its profile confirmed | `IDLE` |
| `IDLE` | `assign` | the assignment and its turn are persisted | take the assignment; `BUSY` |
| `BUSY` | `accept-turn` | the turn's rights are revoked and its effects reconciled | release the assignment; `IDLE` |
| `BUSY` | `request-input` | the input request is recognized | keep the assignment; `WAITING_INPUT` |
| `IDLE`, `BUSY`, `WAITING_INPUT` | `attach` | the turn is quiescent or its interruption confirmed | `ATTACHED` |
| `ATTACHED` | `detach`, `connection-lost` | none | by the continuation (below) |
| `IDLE`, `BUSY`, `WAITING_INPUT` | `pause` | the interruption and the stop of the descendants are confirmed | release the assignment; `PAUSED` |
| `PAUSED` | `resume` | the profile and the continuation are validated | `STARTING` |
| `IDLE`, `BUSY`, `WAITING_INPUT`, `ATTACHED`, `PAUSED` | `scale-down` | none | keep the assignment; `DRAINING` |
| `DRAINING` | `drained` | the turn is finished or cancelled, no client is attached and the descendants are stopped | release the assignment; `STOPPED` |
| every state but `STOPPED` | `stop` | the effects are reconciled | release the assignment; `STOPPED` |
| every active state | `fail` | a reason is given | keep the assignment; `FAILED` |
| `FAILED` | `recover` | the cause is lifted and no descendant remains | release the assignment; `STOPPED` |

A detached worker is reconciled by its continuation: without an
assignment it returns to `IDLE`; with one and a valid continuation it
returns to `WAITING_INPUT`; when the effects are not reconciled, or the
continuation of its assignment is not valid, it goes to `FAILED` with
the cause in its reason.

The guards read their facts from `worker.Guard`: capacity, sessions,
turns, process supervision and attach report them with the event. A
fact left unset does not hold.

## Assignments

An assignment links a worker to one turn of a task for a role
(`planning`, `implementation`, `review` or `correction`). A worker
holds at most one: it takes one only from `IDLE`, and the database
refuses an assignment on a `STOPPED`, `STARTING`, `IDLE` or `PAUSED`
worker and requires one on a `BUSY` or `WAITING_INPUT` worker.

An assignment always names a stored turn: `Store.Transition` checks
that the turn exists, belongs to the assigned task and runs the
worker's agent, and fails with `worker.ErrInvalid` otherwise. A turn is
assigned to one worker at most; a second worker fails with
`worker.ErrAssigned`. Since an assignment needs a turn, a task that
waits without a live turn reserves no worker, and an accepted turn
frees its worker at once.

A failed worker keeps its assignment, so the concerned task is known;
`recover` or `stop` releases it.

## Persistence

`Store.Transition` evaluates the event, then writes the worker and its
`worker_events` row in one transaction. The update applies only if the
worker is still at the state and version it read, so a concurrent
transition makes the later one fail with `ErrTransition` instead of
overwriting it. Each event row records the task and turn of the
assignment the worker held or took.

## Starting and stopping

`Supervisor.Start` starts `Count` workers of one agent of a project:

1. **Agent.** The agent is a name of the configuration snapshot: when
   `[agents.<name>]` sets a `driver`, that driver names the agent kind;
   otherwise the name is the kind itself (`claude-code` or `codex`).
   `agent.Registry.Installed` finds the kind's binary on `PATH`, reads
   its `--version` and selects the validated driver, as `maestro agent
   doctor` does. An unknown kind or a version without a validated driver
   fails with `agent.ErrNoDriver`; every resolution failure is wrapped in
   `worker.ErrAgent`. A daemon that cannot confine refuses every start
   with `launcher.ErrUnsupported`.
2. **Capacity.** One session slot of the global capacity
   (`max_sessions`) is reserved per worker, all of them or none: a
   count above the free slots fails with `scheduler.ErrFull` and
   registers nothing.
3. **Workers.** The `STOPPED` workers of the same agent, kind and driver
   are started again, by name; the others are registered under the first
   free names `<agent>-01`, `<agent>-02`, and so on. A worker that is
   still being stopped, or whose group is not confirmed gone, is never
   reused. Each one takes the `start` event with its reserved slot and is
   returned `STARTING`. When a step fails, the start registers nothing:
   the workers it registered are removed, the reused ones take the `stop`
   event back to `STOPPED`, and every slot is released.

Each `STARTING` worker is then launched in the background:

- its private repository (`worktree.Store.AddWorker`) receives the
  current integration head, checked out detached in the worker's own
  worktree, `workers/<worker>/worktree/` of the project
  (`worktree.Store.CheckoutWorker`). A starting worker holds no
  assignment, so an existing worktree is reset to that head: the
  uncommitted changes and untracked files of a previous run, ignored
  files included, are discarded;
- the snapshot's instruction files are provisioned in that worktree for
  the agent kind, without a role (`provision.Instructions`), and the MCP
  servers that apply to the agent are written in its native user
  configuration inside its private HOME: `.claude/.claude.json` for
  Claude Code, `.codex/config.toml` for Codex;
- a fresh private HOME and supervisor state directory
  (`workers/<worker>/home/` and `workers/<worker>/supervisor/`) hold
  the native session; the agent starts through its driver
  (`ClaudeStart` or `CodexStart`) in a confined PTY, under a review
  permissions profile of epoch 1 on the integration head: the worktree
  and the worker repository are read-only, the agent binary is mounted
  read-only, and the environment is limited to `PATH`, `TERM`, `LANG`,
  the private HOME variables and, when the agent configures
  `api_key_env`, that variable taken from the daemon's environment. The
  session shares the host network, so the agent reaches its provider;
- the native session identity is confirmed from the agent's own
  metadata (`ConfirmClaude` or `ConfirmCodex`), polled until it holds,
  then bound to the session epoch. The worker takes the `ready` event
  and is `IDLE`, with the confirmed ID in its reason.

Any failure on the way — an agent that exits before confirming its
session, a confirmation that does not hold within the start timeout (one
minute by default), a provisioning error — terminates the session group
and moves the worker to `FAILED` with the cause in its reason. An `IDLE`
worker whose session ends on its own is moved to `FAILED` the same way;
until that failure is recorded, the worker is being torn down: a stop is
refused and its name is not reused.

A slot is released only once its session group is confirmed gone: every
session is watched until its group ends, so a group whose termination
fails keeps its slot against `max_sessions`, and its worker's name is
not reused, until the group is seen gone.

`Supervisor.Stop` stops a worker. A live worker takes the `scale-down`
event, so it is `DRAINING` and takes no new assignment; its confined
group is terminated (SIGTERM, then SIGKILL after the grace period), its
slot released, and it takes the `drained` event to `STOPPED`. A `FAILED`
worker takes the `stop` event to `STOPPED` once its group is gone,
releasing the assignment it kept: when an earlier termination of its
group failed, the stop retries it first. A worker that is `STARTING`,
already `STOPPED`, live and holding an assignment, already being
stopped by another call or being torn down after its session ended on
its own is refused with `worker.ErrTransition`: stops of one worker
never run concurrently. A
group that cannot be terminated leaves the worker `FAILED`, its slot
held until the group ends, and a later stop that cannot terminate it
either leaves it `FAILED` too.

`Supervisor.Close` refuses new starts, abandons the starts in progress
and stops every live worker; a live session a stop leaves behind is
terminated anyway and its worker moved to `FAILED`, and the termination
of every group an earlier teardown failed on is retried. `Close` then
returns once every group is gone, or after `CloseTimeout` (ten seconds
by default) when a group could not be terminated. `maestro-svc` calls it
when it shuts down.

`Supervisor.Ready`, when set, is called with the project of every
started worker once it reaches `IDLE`; a worker whose start fails is not
reported. `maestro-svc` drives the project's tasks from it.

## Live sessions

`Supervisor.Session(project, name)` returns the live session of a
started worker, the `worker.Session` the task engine drives
([Task engine](task-engine.md)); a worker that is starting, being
stopped or torn down, failed or stopped has none. The session drives its
agent through the worker's driver:

| Method | Effect |
| --- | --- |
| `Checkout(task, revision)` | fetches the canonical repository's branches and `refs/maestro/` references into the worker repository (under `refs/canonical/`), checks the task branch out at the revision in the worker's worktree, discarding uncommitted changes and untracked files; ignored files, such as the provisioned instruction files, are kept |
| `RuntimePaths()` | the instruction files provisioned in the worktree at start |
| `Send(prompt)` | grants the turn's rights, then writes the prompt to the PTY as a bracketed paste followed by Enter (`agent.PromptInput`) and begins the detection of the turn |
| `Poll()` | the driver's `agent.TurnDetector` on the PTY's output and the session's state |
| `Interrupt()` | records the interruption in the detector and sends Esc |
| `Settle()` | revokes the turn's rights |
| `Close()` | ends the session of a worker the engine failed: the session is taken over for termination, its confined group terminated, whatever profile it runs under, and its slot released once the group is gone |

Rights change by stop-and-resume, never in place. A started agent runs
under a `review` profile until its first turn. A turn runs under a
`coding` permissions profile: the worktree and the worker repository are
writable. `Settle` ends the session's permissions epoch — input fenced,
the whole confined group stopped and confirmed gone — and resumes the
exact confirmed native conversation (`ClaudeResume` or `CodexResume`) in
a new confined PTY under a `review` profile of the next epoch, where the
worktree and the worker repository are read-only; once it returns nil,
the agent cannot write the task's sources. The next `Send` resumes it
the same way under a `coding` profile before writing its prompt. A
resumed agent is ready for a prompt once its terminal has drawn
something and stayed quiet for 300 ms, at most the start timeout. The
detector's own turn and input-wait bounds are set to 24 hours: the
engine enforces the task's bounds.

A change of rights keeps the worker's slot and state: the session's
watcher follows the resumed group instead of the stopped one. A resume
that fails returns its error, and the engine then fails the worker and
closes its session: a resumed agent that never settles is terminated
with it. A resumed agent that exits fails the worker like a session that
ended on its own. A stop or `Close` waits for a change of rights in progress, and a session
taken over for termination is never resumed.

## Startup reconciliation

A daemon that starts holds no runtime: every session of the previous
daemon is lost. Before it binds its socket, still under the user lock,
`maestro-svc` calls `Store.Reconcile` on the workers of every
registered project. The reconciliation moves workers only through the
transition table, signals no process and resumes no conversation.

It first searches the worker's workspaces for surviving processes with
`launcher.Survivors`: its private repository and its own directory of
the project, `workers/<worker>/`, which holds the worktree its session
starts in, its private HOME and its supervisor state. A survivor is a
process visible to the daemon whose working directory is one of them or
lies below it. A confined group binds its workspace at the same path
and works in it, so its processes show that path from the host. No
persisted PID is trusted and the search proves no membership in a
supervised group: a process it finds is an unidentified survivor, never
a process to stop.

A repository that was removed, or removed and created again, still has
the processes that kept working in it: the kernel names their working
directory `<path> (deleted)`, and the search matches it. A process that
ends during the search is skipped, and so is a process the daemon is
not permitted to inspect, such as one of another user or one that is
not dumpable: its working directory is hidden from the search. Any
other failure to read a working directory is a failed search.

| Recorded worker | Found | Decision |
| --- | --- | --- |
| any active state | survivors, or a failed search | `fail`; the reason names the processes or the error |
| `BUSY`, `WAITING_INPUT`, `ATTACHED` or `DRAINING` holding a turn | no survivor | `fail`, keeping the assignment |
| `PAUSED` | no survivor | unchanged: a paused worker has no runtime |
| `STARTING`, `IDLE`, `ATTACHED` or `DRAINING` without a turn | no survivor | `stop`: no turn and no process are left to reconcile |
| `STOPPED` or `FAILED` | anything | unchanged; survivors are only reported |

A failed worker keeps the assignment it held: the turn lost its
runtime and its effects are not reconciled, and no other worker can
take that turn until `recover` or `stop` releases it. The task of the
assignment is blocked in the same transaction as the worker's failure,
with the worker's reason, unless it is already blocked or finished. A
worker with an unidentified survivor fails as well, so that two
runtimes never work in the same repository.

Each decision is recorded as a `fail` or `stop` event with its reason.
`maestro-svc` prints one line per worker it moved or found survivors
for, such as `worker <project>/<name>: IDLE -> STOPPED: runtime lost
with the previous daemon`. A second reconciliation finds the workers
stopped, paused or failed and leaves them unchanged.

## Daemon API

The daemon serves the registry on its versioned JSON API
(`internal/ipc`, `workers.go`). `maestro-svc` wires `ipc.Server.Workers`
to a `worker.Store` on its state database, and `ipc.Server.Supervisor`
to a supervisor bounded by its session ceiling and confined by its
launcher; a server without a store answers every worker route with
`not_found`, and one without a supervisor answers the start and stop
routes with `not_found`. Like the task routes, the reads name their
project with `project_id` in the query and the mutations in their body;
a worker of another project is not disclosed. Both mutations require an
`Idempotency-Key`: a retry replays the first response.

| Route | Body | Success |
| --- | --- | --- |
| `GET /v1/workers?project_id=` | | `200`, the project's workers, by name |
| `GET /v1/workers/{name}?project_id=` | | `200`, the worker with its `events` |
| `POST /v1/workers` | `{"project_id", "agent", "count"}` | `202`, the `STARTING` workers |
| `POST /v1/workers/{name}/stop` | `{"project_id"}` | `200`, the `STOPPED` worker |

A start takes and persists the configuration snapshot of the user's
repository as it is now, like a new task, and the workers are
provisioned from it.

A worker carries its `name`, `agent`, `agent_kind`, `driver`,
`repository`, `state`, `version`, `reason`, `created_at`, `updated_at`
and, when it holds one, its `assignment` (`task_id`, `role`,
`turn_id`). The events of `show` are the most recent ones, at most
`ipc.RecentWorkerEvents` (20), oldest first; each names the task and
turn it is about, when there is one.

| Code | Status | Cause |
| --- | --- | --- |
| `invalid_request` | 400 | missing `project_id` or `agent`, a count below 1, an invalid configuration, or an agent without a validated driver |
| `not_found` | 404 | unknown project, or a worker unknown in that project |
| `conflict` | 409 | the session ceiling is reached, the agent's binary is missing or unreadable, the host cannot confine, the project's repository is missing, or the worker cannot stop in its state or is already being stopped |

`maestro worker list` and `maestro worker show <name>` print these
documents, and `maestro worker start <agent> [--count <n>]` and
`maestro worker stop <name>` call the mutations, on the project of the
current repository or the one named by `--project <id>`. `start`
follows each worker until it is `IDLE` or `FAILED` and fails when one of
them failed.
