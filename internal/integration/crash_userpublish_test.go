// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"testing"

	"github.com/goabonga/maestro/internal/worktree"
)

// crashUserPublishSetup returns a project whose integration branch and
// view hold the result of a committed, tested sync not yet published to
// the user repository.
func crashUserPublishSetup(t *testing.T) crashFixture {
	t.Helper()
	f := crashSyncSetup(t)
	if err := crashSyncRun(f.store, f.project, f.id, 0); err != nil {
		t.Fatal(err)
	}
	if err := RefreshIntegrationView(f.project); err != nil {
		t.Fatal(err)
	}
	return crashFixture{store: f.store, project: f.project}
}

// crashUserPublishFlows are the flows of a PUBLISH operation: a
// publication to the user repository, and one whose branch the user
// moves just before its update.
func crashUserPublishFlows() []crashFlow {
	return []crashFlow{
		{
			name:  "user-publish",
			setup: crashUserPublishSetup,
			run: func(store Store, project worktree.Project, _ crashSpec) error {
				_, err := store.PublishToUser(project)
				return err
			},
			cases: []crashCase{
				{point: crashUserPrepared, decisions: []string{"abandon:PREPARED>ROLLED_BACK*"}},
				{point: crashUserStarted, decisions: []string{"abandon:STARTED>ROLLED_BACK*"}},
				{point: crashUserTransferred, decisions: []string{"abandon:STARTED>ROLLED_BACK*"}},
				{point: crashUserBranch, decisions: []string{"finalize:STARTED>COMMITTED*"}},
				{point: crashUserApplied, decisions: []string{"finalize:APPLIED>COMMITTED*"}},
			},
		},
		{
			name:  "user-publish-concurrent",
			setup: crashUserPublishSetup,
			run: func(store Store, project worktree.Project, _ crashSpec) error {
				// The user points the branch at the commit before the
				// synced one, neither its old value nor the target.
				userPublishBeforeUpdate = func() {
					_, _ = userPublishGit(project.UserRepository, "update-ref", UserPublishRef, "HEAD~1")
				}
				_, err := store.PublishToUser(project)
				return err
			},
			wantErr: ErrPublishConcurrent,
			cases: []crashCase{
				// The user branch is neither the old value nor the target:
				// recovery blocks and rewrites nothing.
				{point: crashUserFailed, decisions: []string{"block:FAILED>FAILED"}},
			},
		},
	}
}
