// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/worktree"
)

// The crash injection harness runs a journaled operation in a child
// process, the test binary itself, against a temporary database and
// temporary repositories. The child replaces crashHook to SIGKILL
// itself at one crash point; the parent then reopens the database, runs
// the startup recovery and checks that the project converged or is
// explicitly blocked, and that a second recovery changes nothing.

// crashSpecEnv names the variable holding the path of the crashSpec
// that turns TestCrashChild into the child process.
const crashSpecEnv = "MAESTRO_CRASH_SPEC"

// crashSpec is what the child process runs: one flow on a fixture,
// killed at Point, or with no Point, recording the crash points it
// reaches in Record.
type crashSpec struct {
	Flow           string   `json:"flow"`
	Point          string   `json:"point"`
	Record         string   `json:"record"`
	Database       string   `json:"database"`
	ProjectID      string   `json:"project_id"`
	ProjectDir     string   `json:"project_dir"`
	UserRepository string   `json:"user_repository"`
	OperationID    string   `json:"operation_id"`
	Reports        []string `json:"reports"`
}

// crashFixture is the durable state a flow starts from.
type crashFixture struct {
	store   Store
	project worktree.Project
	// id is the operation the flow carries on, when it exists already.
	id string
	// reports are test report ids the flow uses.
	reports []string
}

// crashFlow is one journaled operation run in the child process.
type crashFlow struct {
	name  string
	setup func(t *testing.T) crashFixture
	// run carries the operation on in the child process; its error
	// must match wantErr.
	run     func(store Store, project worktree.Project, spec crashSpec) error
	wantErr error
	cases   []crashCase
}

// crashCase is a crash at one point of a flow and what recovery must
// decide after it.
type crashCase struct {
	point string
	// decisions summarize the first recovery, as recoverySummary does.
	decisions []string
	// retest completes an operation recovery left for its tests to run
	// again: a passing report is recorded and the next recovery must
	// publish it.
	retest bool
}

// crashAllFlows returns every flow of the harness.
func crashAllFlows() []crashFlow {
	return crashIntegrationFlows()
}

// TestCrashChild is the child process of the crash injection tests; it
// is skipped otherwise.
func TestCrashChild(t *testing.T) {
	path := os.Getenv(crashSpecEnv)
	if path == "" {
		t.Skip("run by the crash injection tests in a child process")
	}
	data, err := os.ReadFile(path) // #nosec G304 -- written by the parent test
	if err != nil {
		t.Fatal(err)
	}
	var spec crashSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	var flow *crashFlow
	for _, f := range crashAllFlows() {
		if f.name == spec.Flow {
			flow = &f
		}
	}
	if flow == nil {
		t.Fatalf("unknown flow %q", spec.Flow)
	}
	db, err := state.Open(spec.Database)
	if err != nil {
		t.Fatal(err)
	}
	project := worktree.Project{ID: spec.ProjectID, UserRepository: spec.UserRepository, Dir: spec.ProjectDir}
	var reached []string
	crashHook = func(point string) {
		reached = append(reached, point)
		if point != spec.Point {
			return
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	err = flow.run(Store{DB: db}, project, spec)
	switch {
	case flow.wantErr == nil && err != nil, flow.wantErr != nil && !errors.Is(err, flow.wantErr):
		t.Fatalf("flow %s: err %v, want %v", spec.Flow, err, flow.wantErr)
	case spec.Point != "":
		t.Fatalf("crash point %s not reached; reached %v", spec.Point, reached)
	}
	if data, err = json.Marshal(reached); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.Record, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// crashChild runs flow on the fixture in a child process, killed at
// point or, with no point, to its end. The fixture's database is closed
// first, like the daemon's at a crash, and reopened afterwards. It
// returns the fixture on the reopened database and the crash points the
// child reached when it ran to its end.
func crashChild(t *testing.T, flow crashFlow, f crashFixture, point string) (crashFixture, []string) {
	t.Helper()
	dir := t.TempDir()
	spec := crashSpec{
		Flow: flow.name, Point: point, Record: filepath.Join(dir, "reached.json"),
		Database: f.store.DB.Path(), ProjectID: f.project.ID, ProjectDir: f.project.Dir,
		UserRepository: f.project.UserRepository, OperationID: f.id, Reports: f.reports,
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.count=1") // #nosec G204 -- the test binary itself
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") && !strings.HasPrefix(variable, crashSpecEnv+"=") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, crashSpecEnv+"="+specPath)
	out, err := cmd.CombinedOutput()
	var reached []string
	if point == "" {
		if err != nil {
			t.Fatalf("flow %s: %v\n%s", flow.name, err, out)
		}
		data, err := os.ReadFile(spec.Record) // #nosec G304 -- written by the child
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &reached); err != nil {
			t.Fatal(err)
		}
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("flow %s at %s: the child was not killed: %v\n%s", flow.name, point, err, out)
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
			t.Fatalf("flow %s at %s: the child ended with %v, not SIGKILL\n%s", flow.name, point, err, out)
		}
	}

	db, err := state.Open(spec.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f.store = Store{DB: db}
	return f, reached
}

// crashConsistent checks the project after a recovery that took
// decisions: no operation is committed without its target holding its
// result, none holds a result without its durable reference, none left
// its result in a target without being committed, and every operation
// still unfinished was left on purpose, for its tests to run again or
// blocked for a human.
func crashConsistent(t *testing.T, f crashFixture, decisions []Decision) {
	t.Helper()
	ops, err := f.store.List(f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	repository := f.project.Repository()
	branch := branchOf(t, f.project)
	for _, op := range ops {
		if !op.State.Terminal() {
			left := false
			for _, d := range decisions {
				if d.OperationID == op.ID && (d.Action == ActionRetest || d.Action == ActionBlock) {
					left = true
				}
			}
			if !left {
				t.Fatalf("operation %s left in %s without a decision to retest or block: %v", op.ID, op.State, recoverySummary(decisions))
			}
		}
		if op.Type == Publish {
			user, err := userPublishRead(f.project.UserRepository)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case op.State == Committed && user != op.SourceHeadSHA:
				t.Fatalf("publication %s committed, user branch at %q, target %s", op.ID, user, op.SourceHeadSHA)
			case op.State == RolledBack && user == op.SourceHeadSHA:
				t.Fatalf("publication %s rolled back, user branch at its target %s", op.ID, user)
			}
			continue
		}
		ref, hasRef, err := ReadResultRef(repository, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if op.ResultSHA != "" && (!hasRef || ref != op.ResultSHA) {
			t.Fatalf("operation %s records the result %s, its reference holds %q", op.ID, op.ResultSHA, ref)
		}
		published := false
		if hasRef {
			if published, err = recoveryContains(repository, branch, ref); err != nil {
				t.Fatal(err)
			}
		}
		switch {
		case op.State == Committed && !published:
			t.Fatalf("operation %s committed, integration branch at %s without its result %q", op.ID, branch, ref)
		case op.State != Committed && published:
			t.Fatalf("operation %s in %s, integration branch at %s holds its result %s", op.ID, op.State, branch, ref)
		case op.Type == Integrate && op.State == Committed && taskState(t, f.store) != task.Done:
			t.Fatalf("operation %s committed, task %s", op.ID, taskState(t, f.store))
		}
	}
}

// crashRetest records a passing test report on the applied operation
// recovery left for its tests to run again, then recovers once more:
// the operation must be published.
func crashRetest(t *testing.T, f crashFixture) {
	t.Helper()
	ops, err := f.store.List(f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var op Operation
	for _, candidate := range ops {
		if candidate.State == Applied {
			op = candidate
		}
	}
	report := acceptReport(t, f.store, "unit", "sha256-x", op.ResultSHA, 0)
	switch op.Type {
	case Integrate:
		if _, err := f.store.DB.Exec("UPDATE tasks SET state = 'VALIDATING', result_sha = ? WHERE task_id = 't1'", op.ResultSHA); err != nil {
			t.Fatal(err)
		}
		_, err = f.store.RecordTestReports(f.project, op.ID, []string{report})
	case Sync:
		_, err = f.store.MarkTested(op.ID, Evidence{TestReportIDs: []string{report}})
	default:
		t.Fatalf("no applied operation to test again: %+v", ops)
	}
	if err != nil {
		t.Fatal(err)
	}
	decisions := recoveryRun(t, f.store, f.project)
	recoveryExpect(t, decisions, "retry-publication:TESTED>COMMITTED*")
	crashConsistent(t, f, decisions)
}

// TestCrashInjection kills each flow at each of its crash points and
// recovers. A first run of each flow to its end checks that the flow
// reaches every point listed for it, and only points some flow lists.
func TestCrashInjection(t *testing.T) {
	flows := crashAllFlows()
	covered := map[string]bool{}
	for _, flow := range flows {
		for _, c := range flow.cases {
			covered[c.point] = true
		}
	}
	for _, flow := range flows {
		t.Run(flow.name, func(t *testing.T) {
			t.Run("points", func(t *testing.T) {
				t.Parallel()
				_, reached := crashChild(t, flow, flow.setup(t), "")
				seen := map[string]bool{}
				for _, point := range reached {
					seen[point] = true
					if !covered[point] {
						t.Errorf("crash point %s is reached but never injected", point)
					}
				}
				for _, c := range flow.cases {
					if !seen[c.point] {
						t.Errorf("crash point %s is not reached by the flow: %v", c.point, reached)
					}
				}
			})
			for _, c := range flow.cases {
				t.Run(c.point, func(t *testing.T) {
					t.Parallel()
					f, _ := crashChild(t, flow, flow.setup(t), c.point)
					decisions := recoveryRun(t, f.store, f.project)
					recoveryExpect(t, decisions, c.decisions...)
					crashConsistent(t, f, decisions)

					again := recoveryRun(t, f.store, f.project)
					for _, d := range again {
						if d.Applied {
							t.Fatalf("a second recovery changed the project: %v", recoverySummary(again))
						}
					}
					if c.retest {
						crashRetest(t, f)
					}
				})
			}
		})
	}
}

// crashIntegrationFlows are the flows of an INTEGRATE operation: the
// candidate build, a failed test run and the publication.
func crashIntegrationFlows() []crashFlow {
	return []crashFlow{
		{
			name: "build",
			setup: func(t *testing.T) crashFixture {
				f := candidateRepository(t, "base\n", [2]string{"feature.txt", "one\n"}, [2]string{"feature.txt", "two\n"})
				store := recoveryJournal(t)
				return crashFixture{store: store, project: f.project, id: candidateOperation(t, store, f).ID}
			},
			run: func(store Store, project worktree.Project, spec crashSpec) error {
				_, err := store.BuildCandidate(project, spec.OperationID, nil)
				return err
			},
			cases: []crashCase{
				{point: crashBuildStarted, decisions: []string{"rebuild:STARTED>APPLIED*", "retest:APPLIED>APPLIED"}, retest: true},
				{point: crashBuildWorktree, decisions: []string{"rebuild:STARTED>APPLIED*", "retest:APPLIED>APPLIED"}, retest: true},
				{point: crashBuildChain, decisions: []string{"abandon:STARTED>ROLLED_BACK*"}},
				{point: crashBuildRecorded, decisions: []string{"abandon:STARTED>ROLLED_BACK*"}},
				{point: crashBuildResult, decisions: []string{"abandon:STARTED>ROLLED_BACK*"}},
				{point: crashApplyRef, decisions: []string{"record-result:STARTED>APPLIED*", "retest:APPLIED>APPLIED"}, retest: true},
				{point: crashBuildApplied, decisions: []string{"retest:APPLIED>APPLIED"}, retest: true},
			},
		},
		{
			name: "failed-tests",
			setup: func(t *testing.T) crashFixture {
				store, project, op := recoveryApplied(t)
				report := acceptReport(t, store, "unit", "sha256-x", op.ResultSHA, 1)
				return crashFixture{store: store, project: project, id: op.ID, reports: []string{report}}
			},
			run: func(store Store, project worktree.Project, spec crashSpec) error {
				_, err := store.RecordTestReports(project, spec.OperationID, spec.Reports)
				return err
			},
			wantErr: ErrTestsFailed,
			cases: []crashCase{
				{point: crashRollBackFailed, decisions: []string{"roll-back:FAILED>ROLLED_BACK*"}},
			},
		},
		{
			name: "publish",
			setup: func(t *testing.T) crashFixture {
				store, project, op := recoveryTested(t)
				return crashFixture{store: store, project: project, id: op.ID}
			},
			run: func(store Store, project worktree.Project, spec crashSpec) error {
				_, err := store.Publish(project, spec.OperationID)
				return err
			},
			cases: []crashCase{
				{point: crashPublishBranch, decisions: []string{"finalize:TESTED>COMMITTED*"}},
				{point: crashPublishCommitted, decisions: []string{"refresh-view:COMMITTED>COMMITTED*"}},
			},
		},
	}
}
