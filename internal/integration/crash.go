// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

// The crash points of the journaled operations. Each one names a window
// right after a durable mutation, Git or SQLite, and before the next:
// a crash there leaves a state that startup recovery must reconcile.
const (
	// crashBuildStarted: the integration is journaled STARTED, its
	// candidate worktree does not exist yet.
	crashBuildStarted = "build/started"
	// crashBuildWorktree: the candidate worktree is added, detached at
	// the integration base, nothing applied yet.
	crashBuildWorktree = "build/worktree-added"
	// crashBuildChain: the source chain is applied to the candidate and
	// its tree written, not journaled yet.
	crashBuildChain = "build/chain-applied"
	// crashBuildRecorded: the candidate tree is journaled.
	crashBuildRecorded = "build/candidate-recorded"
	// crashBuildResult: the result commit is stored, not referenced.
	crashBuildResult = "build/result-built"
	// crashApplyRef: the durable result reference is created, APPLIED
	// is not journaled yet.
	crashApplyRef = "apply/result-ref"
	// crashBuildApplied: APPLIED is journaled, the candidate worktree is
	// not removed yet.
	crashBuildApplied = "build/applied"
	// crashRollBackFailed: an integration is journaled FAILED, not
	// ROLLED_BACK yet.
	crashRollBackFailed = "rollback/failed"
	// crashPublishBranch: the integration branch is moved to the result,
	// the operation is not COMMITTED yet.
	crashPublishBranch = "publish/branch-moved"
	// crashPublishCommitted: the operation is COMMITTED and its task
	// DONE, the integration view is not refreshed yet.
	crashPublishCommitted = "publish/committed"
	// crashSyncStarted: the sync is journaled STARTED, nothing imported.
	crashSyncStarted = "sync/started"
	// crashSyncImported: the frozen commit is imported under
	// refs/maestro/sync/<id>.
	crashSyncImported = "sync/imported"
	// crashSyncRecorded: the candidate tree of the sync is journaled.
	crashSyncRecorded = "sync/candidate-recorded"
	// crashSyncRef: the durable result reference of the sync is created,
	// APPLIED is not journaled yet.
	crashSyncRef = "sync/result-ref"
	// crashSyncApplied: APPLIED is journaled, no test has run.
	crashSyncApplied = "sync/applied"
	// crashSyncReports: the test reports are kept, TESTED is not
	// journaled yet.
	crashSyncReports = "sync/reports-kept"
	// crashSyncTested: TESTED is journaled, the integration branch has
	// not moved.
	crashSyncTested = "sync/tested"
	// crashSyncBranch: the integration branch is moved to the imported
	// commit, the sync is not COMMITTED yet.
	crashSyncBranch = "sync/branch-moved"
	// crashSyncFailed: a sync is journaled FAILED, not ROLLED_BACK yet.
	crashSyncFailed = "sync/failed"
	// crashUserPrepared: the publication is journaled PREPARED.
	crashUserPrepared = "user-publish/prepared"
	// crashUserStarted: the publication is journaled STARTED, nothing
	// transferred.
	crashUserStarted = "user-publish/started"
	// crashUserTransferred: the objects are in the user repository, its
	// branch has not moved.
	crashUserTransferred = "user-publish/transferred"
	// crashUserBranch: the branch of the user repository is moved to the
	// target, APPLIED is not journaled yet.
	crashUserBranch = "user-publish/branch-moved"
	// crashUserApplied: APPLIED is journaled, not COMMITTED yet.
	crashUserApplied = "user-publish/applied"
	// crashUserFailed: a publication is journaled FAILED, not
	// ROLLED_BACK yet.
	crashUserFailed = "user-publish/failed"
)

// crashHook is called at every crash point with its name. It does
// nothing in the daemon; the crash injection tests replace it, in a
// child process, to kill that process at a chosen point.
var crashHook = func(string) {}
