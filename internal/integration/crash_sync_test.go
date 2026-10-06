// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goabonga/maestro/internal/worktree"
)

// crashSyncSetup returns a project whose user repository has a commit
// past the integration head, and a SYNC operation of it prepared.
func crashSyncSetup(t *testing.T) crashFixture {
	t.Helper()
	project, _ := canonicalProject(t)
	store := recoveryJournal(t)
	user := filepath.Dir(project.UserRepository)
	if err := os.WriteFile(filepath.Join(user, "synced.txt"), []byte("synced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, user, false, "", "add", "synced.txt")
	gitIn(t, user, false, "", "commit", "-q", "-m", "feat: synced")
	plan, err := Syncer{Store: store, Tester: &syncTestTester{}}.Prepare(SyncRequest{
		Project: project, Branch: "main", ConfigID: "sha256-x", Tests: syncTestCommands,
	})
	if err != nil {
		t.Fatal(err)
	}
	return crashFixture{store: store, project: project, id: plan.Operation.ID}
}

// crashSyncRun runs the prepared sync of the fixture with tests exiting
// with exit.
func crashSyncRun(store Store, project worktree.Project, id string, exit int) error {
	op, err := store.Get(id)
	if err != nil {
		return err
	}
	plan := SyncPlan{
		Operation: op, Project: project, Branch: "main",
		PreviousSHA: op.IntegrationBaseSHA, SyncedSHA: op.SourceHeadSHA, Tests: syncTestCommands,
	}
	_, err = Syncer{Store: store, Tester: &syncTestTester{exit: exit}}.Run(plan)
	return err
}

// crashSyncFlows are the flows of a SYNC operation: a sync whose tests
// pass and one whose tests fail.
func crashSyncFlows() []crashFlow {
	return []crashFlow{
		{
			name:  "sync",
			setup: crashSyncSetup,
			run: func(store Store, project worktree.Project, spec crashSpec) error {
				return crashSyncRun(store, project, spec.OperationID, 0)
			},
			cases: []crashCase{
				{point: crashSyncStarted, decisions: []string{"abandon:STARTED>ROLLED_BACK*"}},
				{point: crashSyncImported, decisions: []string{"abandon:STARTED>ROLLED_BACK*"}},
				{point: crashSyncRecorded, decisions: []string{"abandon:STARTED>ROLLED_BACK*"}},
				{point: crashSyncRef, decisions: []string{"record-result:STARTED>APPLIED*", "retest:APPLIED>APPLIED"}, retest: true},
				{point: crashSyncApplied, decisions: []string{"retest:APPLIED>APPLIED"}, retest: true},
				{point: crashSyncReports, decisions: []string{"retest:APPLIED>APPLIED"}, retest: true},
				{point: crashSyncTested, decisions: []string{"retry-publication:TESTED>COMMITTED*"}},
				{point: crashSyncBranch, decisions: []string{"finalize:TESTED>COMMITTED*"}},
			},
		},
		{
			name:  "sync-failed-tests",
			setup: crashSyncSetup,
			run: func(store Store, project worktree.Project, spec crashSpec) error {
				return crashSyncRun(store, project, spec.OperationID, 1)
			},
			wantErr: ErrSyncTestsFailed,
			cases: []crashCase{
				{point: crashSyncFailed, decisions: []string{"roll-back:FAILED>ROLLED_BACK*"}},
			},
		},
	}
}
