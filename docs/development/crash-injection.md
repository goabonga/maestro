# Crash injection

The crash injection tests kill a process in the middle of a journaled
operation and check that the [startup recovery](recovery.md) brings the
project to a consistent state, or blocks it explicitly. They run against
temporary repositories and a temporary SQLite database only, through
`make go-test` like every other test.

## Crash points

`internal/integration/crash.go` names the crash points of the
operations. Each one sits right after a durable mutation, Git or SQLite,
and before the next one; the tests inject a crash only at these named
points, not after every durable mutation. The code of the operation calls
`crashHook` with the point's name; in the daemon the hook does nothing.

| Operation | Crash points |
| --- | --- |
| Candidate build (`BuildCandidate`, `ApplyIntegration`) | `build/started`, `build/worktree-added`, `build/chain-applied`, `build/candidate-recorded`, `build/result-built`, `apply/result-ref`, `build/applied` |
| Rollback of an integration (`RollBackIntegration`) | `rollback/failed` |
| Publication of an integration (`Publish`) | `publish/branch-moved`, `publish/committed` |
| Sync (`Syncer.Run`) | `sync/started`, `sync/imported`, `sync/candidate-recorded`, `sync/result-ref`, `sync/applied`, `sync/reports-kept`, `sync/tested`, `sync/branch-moved`, `sync/failed` |
| Publication to the user repository (`PublishToUser`) | `user-publish/prepared`, `user-publish/started`, `user-publish/transferred`, `user-publish/branch-moved`, `user-publish/applied`, `user-publish/failed` |

## Harness

`TestCrashInjection` in `internal/integration/crash_test.go` runs each
flow of the table below once per crash point. A flow prepares its fixture
in the test, closes the database, then runs the operation in a child
process: the test binary itself, restricted to `TestCrashChild` and given
the fixture through the `MAESTRO_CRASH_SPEC` variable. The child replaces
`crashHook` and sends itself `SIGKILL` at the chosen point; the parent
checks that the child died of that signal. Git variables of the
environment are not passed to the child.

The parent then reopens the database, as the daemon does at startup, runs
`Store.Recover` and checks:

- the decisions, against the ones expected for that point;
- that no operation is committed unless its target holds its result, and
  no target holds the result of an uncommitted operation;
- that every result in the journal has its durable reference;
- that a committed integration has its task `DONE`;
- that every unfinished operation was left on purpose, by a `retest` or
  `block` decision;
- that a second recovery applies nothing.

When recovery leaves an operation for its tests to run again, the harness
records a passing test report and recovers once more: the operation must
then be published.

Each flow also runs once to its end in a child process that records the
crash points it reaches. Every point the flow lists must be reached, and
every crash point reached by a flow must be injected by some flow.
`TestCrashPointsDeclared` then reads the package source: every
`crashHook` call must name a constant declared in `crash.go`, and every
declared constant must be called and injected by some flow. A crash
point declared on a path no flow runs to its end, such as an error
branch, still fails without its test; a `crashHook` call that reuses an
existing constant on such a path is not detected.

| Flow | Operation run in the child |
| --- | --- |
| `build` | `BuildCandidate` of a prepared integration |
| `failed-tests` | `RecordTestReports` with a failing report |
| `publish` | `Publish` of a tested integration |
| `sync` | `Syncer.Run` with passing tests |
| `sync-failed-tests` | `Syncer.Run` with failing tests |
| `user-publish` | `PublishToUser` of a committed sync |
| `user-publish-concurrent` | `PublishToUser` while the user moves the branch |

## Expected outcomes

| Crash point | Recovery |
| --- | --- |
| `build/started`, `build/worktree-added` | `rebuild`, then `retest` |
| `build/chain-applied`, `build/candidate-recorded`, `build/result-built` | `abandon`: the candidate holds changes and is kept for diagnosis |
| `apply/result-ref` | `record-result`, then `retest` |
| `build/applied` | `retest` |
| `rollback/failed`, `sync/failed` | `roll-back` |
| `publish/branch-moved`, `sync/branch-moved` | `finalize` |
| `publish/committed` | `refresh-view` |
| `sync/started`, `sync/imported`, `sync/candidate-recorded` | `abandon` |
| `sync/result-ref` | `record-result`, then `retest` |
| `sync/applied`, `sync/reports-kept` | `retest` |
| `sync/tested` | `retry-publication` |
| `user-publish/prepared`, `user-publish/started`, `user-publish/transferred` | `abandon` |
| `user-publish/branch-moved`, `user-publish/applied` | `finalize` |
| `user-publish/failed` with the user branch moved elsewhere | `block`: nothing is rewritten |
