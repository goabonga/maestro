// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/agent"
	"github.com/goabonga/maestro/internal/budget"
	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/handoff"
	"github.com/goabonga/maestro/internal/provision"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/testrun"
	"github.com/goabonga/maestro/internal/turn"
	"github.com/goabonga/maestro/internal/worktree"
)

const validPlan = "# Plan\n## Objective\nAdd a flag.\n## Files\nflag.txt\n## Steps\n1. write it\n## Tests\ncat flag.txt\n"

// gitIdentity commits as a test author, whatever the host configures.
var gitIdentity = []string{"-c", "user.name=Test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false"}

// isolateEngineGit drops the GIT_* variables a git-spawned caller may
// have exported, once, so no test git command reaches its repository.
var isolateEngineGit sync.Once

// engineGit runs one git command of a test.
func engineGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := runGit(dir, append(append([]string{}, gitIdentity...), args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

// manualClock is the clock of an engine test; the engine's waits move
// it forward.
type manualClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// fakeAgent is one scripted turn of a fake agent: it acts on the
// session's worktree when it receives the prompt and returns what the
// session then detects.
type fakeAgent func(s *fakeSession, prompt string) agent.Detection

// fakeSession is the live session of a worker whose worktree is its
// private repository.
type fakeSession struct {
	t                   *testing.T
	dir                 string
	canonical           string
	runtime             provision.RuntimePaths
	turns               []fakeAgent
	prompts             []string
	detection           agent.Detection
	interrupts, settles int
	// settleErr fails every Settle; closes counts the Close calls.
	settleErr error
	closes    int
}

func (s *fakeSession) Checkout(t task.Task, revision string) (string, error) {
	if _, err := runGit(s.dir, "fetch", "--quiet", s.canonical,
		"+refs/heads/*:refs/canonical/heads/*", "+refs/maestro/*:refs/canonical/maestro/*"); err != nil {
		return "", err
	}
	if _, err := runGit(s.dir, "checkout", "--quiet", "--force", "-B", t.Branch, revision); err != nil {
		return "", err
	}
	if _, err := runGit(s.dir, "clean", "--quiet", "-ffd"); err != nil {
		return "", err
	}
	return s.dir, nil
}

func (s *fakeSession) RuntimePaths() provision.RuntimePaths { return s.runtime }

func (s *fakeSession) Send(prompt string) error {
	s.prompts = append(s.prompts, prompt)
	if len(s.turns) == 0 {
		s.t.Errorf("unexpected prompt:\n%s", prompt)
		s.detection = agent.DetectFailed
		return nil
	}
	next := s.turns[0]
	s.turns = s.turns[1:]
	s.detection = next(s, prompt)
	return nil
}

func (s *fakeSession) Poll() agent.Detection { return s.detection }

func (s *fakeSession) Interrupt() error {
	s.interrupts++
	return nil
}

func (s *fakeSession) Settle() error {
	s.settles++
	return s.settleErr
}

func (s *fakeSession) Close() error {
	s.closes++
	return nil
}

// document reads the handoff document a prompt asks for.
func (s *fakeSession) document(prompt string) map[string]any {
	s.t.Helper()
	const marker = "keep every other value as given:\n\n"
	index := strings.LastIndex(prompt, marker)
	if index < 0 {
		s.t.Fatalf("the prompt asks for no document:\n%s", prompt)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(prompt[index+len(marker):]), &document); err != nil {
		s.t.Fatalf("the prompt's document: %v", err)
	}
	return document
}

// write fills the document a prompt asks for and publishes it.
func (s *fakeSession) write(prompt string, fill func(document, payload map[string]any)) {
	s.t.Helper()
	document := s.document(prompt)
	payload, _ := document["payload"].(map[string]any)
	fill(document, payload)
	data, err := json.Marshal(document)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := handoff.Write(s.dir, document["turn_id"].(string), document["attempt_id"].(string), data); err != nil {
		s.t.Fatal(err)
	}
}

// planner writes a valid plan.
func planner(s *fakeSession, prompt string) agent.Detection {
	s.write(prompt, func(_, payload map[string]any) { payload["body"] = validPlan })
	return agent.DetectCompleted
}

// coder commits a file and describes every commit since the base.
func coder(file, content string) fakeAgent {
	return func(s *fakeSession, prompt string) agent.Detection {
		if err := os.WriteFile(filepath.Join(s.dir, file), []byte(content), 0o600); err != nil {
			s.t.Fatal(err)
		}
		engineGit(s.t, s.dir, "add", "--", file)
		engineGit(s.t, s.dir, "commit", "--quiet", "-m", "write "+file)
		head := engineGit(s.t, s.dir, "rev-parse", "HEAD")
		s.write(prompt, func(document, payload map[string]any) {
			commits := strings.Fields(engineGit(s.t, s.dir, "rev-list", "--reverse", document["task_base_sha"].(string)+"..HEAD"))
			document["source_head_sha"] = head
			payload["summary"], payload["source_sha"], payload["commits"], payload["commands"] = "wrote "+file, head, commits, []string{}
		})
		return agent.DetectCompleted
	}
}

// reviewer gives a verdict on the revision it is asked about.
func reviewer(verdict string) fakeAgent {
	return func(s *fakeSession, prompt string) agent.Detection {
		s.write(prompt, func(_, payload map[string]any) {
			payload["verdict"] = verdict
			payload["issues"] = []any{}
			if verdict == "changes_requested" {
				payload["issues"] = []any{map[string]any{"id": "r1", "description": "say it louder", "file": "flag.txt", "severity": "low"}}
			}
		})
		return agent.DetectCompleted
	}
}

// detect returns a fixed detection and writes nothing.
func detect(detection agent.Detection) fakeAgent {
	return func(*fakeSession, string) agent.Detection { return detection }
}

// fakeTester returns scripted exit codes, one run per call.
type fakeTester struct {
	exits []int
	runs  []string
}

func (f *fakeTester) Run(_, sha, clone string, commands []testrun.Command) (testrun.Run, error) {
	f.runs = append(f.runs, sha)
	exit := 0
	if len(f.exits) > 0 {
		exit, f.exits = f.exits[0], f.exits[1:]
	}
	run := testrun.Run{TestedSHA: sha, Clone: clone}
	for _, command := range commands {
		run.Results = append(run.Results, testrun.Result{Name: command.Name, Argv: command.Argv, TestedSHA: sha,
			ExitCode: exit, Output: "unit: exit " + string(rune('0'+exit)) + "\n"})
	}
	return run, nil
}

// engineHarness is an engine over a migrated store, a project whose
// canonical repository holds the base commit, and one IDLE worker.
type engineHarness struct {
	engine   *Engine
	clock    *manualClock
	project  worktree.Project
	worker   Worker
	session  *fakeSession
	tester   *fakeTester
	task     task.Task
	sessions map[string]Session
}

func (h *engineHarness) Session(projectID, name string) (Session, bool) {
	session, ok := h.sessions[projectID+"/"+name]
	return session, ok
}

func newEngineHarness(t *testing.T, cfg config.Config, turns ...fakeAgent) *engineHarness {
	t.Helper()
	isolateEngineGit.Do(func() {
		for _, entry := range os.Environ() {
			if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, "GIT_") {
				_ = os.Unsetenv(name)
			}
		}
	})
	db, err := state.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	configID, err := config.Persist(db, config.Snapshot{Config: cfg, Instructions: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	c := &manualClock{at: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}
	project := worktree.Project{ID: "project-1", Dir: filepath.Join(t.TempDir(), "projects", "project-1")}

	source := t.TempDir()
	engineGit(t, source, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(source, "README"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	engineGit(t, source, "add", "README")
	engineGit(t, source, "commit", "--quiet", "-m", "base")
	base := engineGit(t, source, "rev-parse", "HEAD")
	engineGit(t, "", "clone", "--quiet", "--bare", source, project.Repository())
	engineGit(t, project.Repository(), "update-ref", "refs/heads/maestro/integration", base)

	store := Store{DB: db, Now: c.now}
	w, err := store.Register(project, Spec{Name: "claude-01", Agent: "claude", AgentKind: "claude", Driver: "claude-code-2.1"})
	if err != nil {
		t.Fatal(err)
	}
	engineGit(t, "", "init", "--quiet", "-b", "start", w.Repository)
	for _, in := range []Input{
		{Event: Start, Guard: Guard{CapacityReserved: true}},
		{Event: Ready, Guard: Guard{SessionReady: true, ProfileConfirmed: true}},
	} {
		if w, err = store.Transition(project.ID, w.Name, in); err != nil {
			t.Fatal(err)
		}
	}
	created, err := task.Store{DB: db, Now: c.now}.Create(project.ID, "add a flag file", configID, base)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{t: t, dir: w.Repository, canonical: project.Repository(),
		runtime: provision.RuntimePaths{"CLAUDE.md"}, turns: turns}
	h := &engineHarness{clock: c, project: project, worker: w, session: session, tester: &fakeTester{}, task: created,
		sessions: map[string]Session{project.ID + "/" + w.Name: session}}
	h.engine = &Engine{DB: db, Sessions: h, Tester: h.tester, Now: c.now, PollInterval: time.Minute,
		Wait: func(_ context.Context, d time.Duration) error {
			c.advance(d)
			return nil
		}}
	return h
}

// testConfig is the default configuration with one test command.
func testConfig() config.Config {
	cfg := config.Defaults()
	cfg.Tests = map[string]config.TestCommand{"unit": {Argv: []string{"true"}}}
	return cfg
}

func (h *engineHarness) drive(t *testing.T) {
	t.Helper()
	if err := h.engine.Drive(context.Background(), h.project); err != nil {
		t.Fatal(err)
	}
}

func (h *engineHarness) current(t *testing.T) (task.Task, Worker) {
	t.Helper()
	got, err := h.engine.tasks().Get(h.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	w, err := h.engine.workers().Get(h.project.ID, h.worker.Name)
	if err != nil {
		t.Fatal(err)
	}
	return got, w
}

// events lists the task's recorded events.
func (h *engineHarness) events(t *testing.T) []task.Event {
	t.Helper()
	records, err := h.engine.tasks().Events(h.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var events []task.Event
	for _, record := range records {
		events = append(events, record.Event)
	}
	return events
}

// turnStates lists the states of the task's turns, oldest first.
func (h *engineHarness) turnStates(t *testing.T) []turn.Turn {
	t.Helper()
	rows, err := h.engine.DB.Query(`SELECT turn_id FROM turns WHERE task_id = ? ORDER BY created_at, rowid`, h.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var turns []turn.Turn
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		u, err := h.engine.turns().Get(id)
		if err != nil {
			t.Fatal(err)
		}
		turns = append(turns, u)
	}
	return turns
}

func sameEvents(got []task.Event, want ...task.Event) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestEngineDrivesATaskToReadyToIntegrate(t *testing.T) {
	h := newEngineHarness(t, testConfig(), planner, coder("flag.txt", "on\n"), reviewer("approve"))
	h.drive(t)

	got, w := h.current(t)
	if got.State != task.ReadyToIntegrate || got.ApprovedSHA == "" || got.ApprovedSHA != got.HeadSHA {
		t.Fatalf("task %s approved %q head %q: %s", got.State, got.ApprovedSHA, got.HeadSHA, got.Reason)
	}
	if !sameEvents(h.events(t), task.Created, task.Assign, task.AcceptPlan, task.Implement, task.TestsPass, task.Approve) {
		t.Fatalf("events %v", h.events(t))
	}
	if w.State != Idle || w.Assignment != nil {
		t.Fatalf("worker %s holds %+v", w.State, w.Assignment)
	}
	turns := h.turnStates(t)
	if len(turns) != 3 {
		t.Fatalf("%d turns", len(turns))
	}
	for _, u := range turns {
		if u.State != turn.Succeeded || u.Agent != "claude" {
			t.Fatalf("turn %+v", u)
		}
	}
	used, err := h.engine.budgets().Turns(h.task.ID)
	if err != nil || used.Task != 3 || used.Agents["claude"] != 3 {
		t.Fatalf("turns used %+v %v", used, err)
	}
	if h.session.settles != 3 || h.session.interrupts != 0 || h.session.closes != 0 {
		t.Fatalf("settles %d interrupts %d closes %d", h.session.settles, h.session.interrupts, h.session.closes)
	}
	if len(h.tester.runs) != 1 || h.tester.runs[0] != got.HeadSHA {
		t.Fatalf("tests ran on %v, head %s", h.tester.runs, got.HeadSHA)
	}
	imported := engineGit(t, h.project.Repository(), "rev-parse", "refs/maestro/workers/claude-01/"+got.Branch)
	if imported != got.HeadSHA {
		t.Fatalf("imported %s, head %s", imported, got.HeadSHA)
	}
	for _, kind := range []handoff.Kind{handoff.Plan, handoff.Implementation, handoff.TestReport, handoff.Review} {
		if _, err := handoff.Latest(h.engine.DB, h.task.ID, kind); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	if !strings.Contains(h.session.prompts[1], validPlan) {
		t.Fatalf("the implementation prompt misses the plan:\n%s", h.session.prompts[1])
	}
	usage, err := h.engine.budgets().Check(h.task.ID)
	if err != nil || usage.Open != "" {
		t.Fatalf("time usage %+v %v", usage, err)
	}
}

func TestEngineCorrectsFailedTestsAndRequestedChanges(t *testing.T) {
	h := newEngineHarness(t, testConfig(), planner, coder("flag.txt", "on\n"), coder("flag.txt", "ON\n"),
		reviewer("changes_requested"), coder("flag.txt", "ON!\n"), reviewer("approve"))
	h.tester.exits = []int{1, 0, 0}
	h.drive(t)

	got, w := h.current(t)
	if got.State != task.ReadyToIntegrate || got.FixCycles != 2 {
		t.Fatalf("task %s after %d fix cycles: %s", got.State, got.FixCycles, got.Reason)
	}
	if !sameEvents(h.events(t), task.Created, task.Assign, task.AcceptPlan, task.Implement, task.TestsFail, task.Fix,
		task.TestsPass, task.RequestChanges, task.Fix, task.TestsPass, task.Approve) {
		t.Fatalf("events %v", h.events(t))
	}
	if w.State != Idle {
		t.Fatalf("worker %s", w.State)
	}
	afterTests, afterReview := h.session.prompts[2], h.session.prompts[4]
	if !strings.Contains(afterTests, "test unit (true) exited 1") || !strings.Contains(afterTests, "unit: exit 1") {
		t.Fatalf("the correction after the tests misses them:\n%s", afterTests)
	}
	if !strings.Contains(afterReview, "r1 (low, flag.txt): say it louder") {
		t.Fatalf("the correction after the review misses its issue:\n%s", afterReview)
	}
	content := engineGit(t, h.project.Repository(), "show", got.HeadSHA+":flag.txt")
	if content != "ON!" {
		t.Fatalf("approved content %q", content)
	}
	commits := strings.Fields(engineGit(t, h.project.Repository(), "rev-list", got.BaseSHA+".."+got.HeadSHA))
	if len(commits) != 3 {
		t.Fatalf("%d commits on the task branch", len(commits))
	}
}

func TestEngineBlocksWhenFixCyclesAreExhausted(t *testing.T) {
	h := newEngineHarness(t, testConfig(), planner, coder("flag.txt", "on\n"))
	if _, err := h.engine.DB.Exec(`UPDATE tasks SET max_fix_cycles = 0 WHERE task_id = ?`, h.task.ID); err != nil {
		t.Fatal(err)
	}
	h.tester.exits = []int{1}
	h.drive(t)

	got, w := h.current(t)
	if got.State != task.Blocked || got.ResumeState != task.Testing || !strings.Contains(got.BlockedReason, task.ReasonFixCyclesExhausted) {
		t.Fatalf("task %s resume %s: %s", got.State, got.ResumeState, got.BlockedReason)
	}
	if _, err := handoff.Latest(h.engine.DB, h.task.ID, handoff.FixRequest); !errors.Is(err, handoff.ErrNone) {
		t.Fatalf("a fix request without a fix cycle: %v", err)
	}
	if w.State != Idle {
		t.Fatalf("worker %s", w.State)
	}
}

func TestEngineRepairsAnInvalidHandoffOnce(t *testing.T) {
	garbled := func(s *fakeSession, prompt string) agent.Detection {
		s.write(prompt, func(_, payload map[string]any) { payload["body"] = "no sections" })
		return agent.DetectCompleted
	}
	h := newEngineHarness(t, testConfig(), garbled, planner, coder("flag.txt", "on\n"), reviewer("approve"))
	h.drive(t)

	got, w := h.current(t)
	if got.State != task.ReadyToIntegrate {
		t.Fatalf("task %s: %s", got.State, got.Reason)
	}
	turns := h.turnStates(t)
	if len(turns) != 4 || turns[0].State != turn.Failed || turns[1].State != turn.Succeeded || turns[1].PreviousID != turns[0].ID {
		t.Fatalf("turns %+v", turns)
	}
	if !strings.Contains(h.session.prompts[1], "was refused") || !strings.Contains(h.session.prompts[1], turns[1].AttemptID) {
		t.Fatalf("repair prompt:\n%s", h.session.prompts[1])
	}
	used, err := h.engine.budgets().Turns(h.task.ID)
	if err != nil || used.Task != 4 {
		t.Fatalf("turns used %+v %v", used, err)
	}
	if w.State != Idle {
		t.Fatalf("worker %s", w.State)
	}
}

func TestEngineBlocksARepairThatChangesTheCode(t *testing.T) {
	missing := detect(agent.DetectCompleted)
	tampering := func(s *fakeSession, prompt string) agent.Detection {
		if err := os.WriteFile(filepath.Join(s.dir, "README"), []byte("changed\n"), 0o600); err != nil {
			s.t.Fatal(err)
		}
		return planner(s, prompt)
	}
	h := newEngineHarness(t, config.Defaults(), missing, tampering)
	h.drive(t)

	got, w := h.current(t)
	if got.State != task.Blocked || got.ResumeState != task.Planning || !strings.Contains(got.BlockedReason, "the repair changed the code") {
		t.Fatalf("task %s resume %s: %s", got.State, got.ResumeState, got.BlockedReason)
	}
	if w.State != Idle || w.Assignment != nil {
		t.Fatalf("worker %s holds %+v", w.State, w.Assignment)
	}
	if _, err := handoff.Latest(h.engine.DB, h.task.ID, handoff.Plan); !errors.Is(err, handoff.ErrNone) {
		t.Fatalf("a plan was accepted: %v", err)
	}
}

func TestEngineBlocksAnInputWaitThenFailsTheWorkerWhenItExpires(t *testing.T) {
	h := newEngineHarness(t, config.Defaults(), detect(agent.DetectWaitingInput))
	h.drive(t)

	got, w := h.current(t)
	if got.State != task.Blocked || got.ResumeState != task.Planning || !strings.Contains(got.BlockedReason, "waits for input") {
		t.Fatalf("task %s resume %s: %s", got.State, got.ResumeState, got.BlockedReason)
	}
	if w.State != WaitingInput || w.Assignment == nil || w.Assignment.TaskID != h.task.ID {
		t.Fatalf("worker %s holds %+v", w.State, w.Assignment)
	}
	waiting, err := h.engine.turns().Get(w.Assignment.TurnID)
	if err != nil || waiting.State != turn.WaitingInput {
		t.Fatalf("turn %+v %v", waiting, err)
	}

	h.clock.advance(4 * time.Minute)
	h.drive(t)
	if _, w = h.current(t); w.State != WaitingInput {
		t.Fatalf("worker %s before the input wait bound", w.State)
	}
	h.clock.advance(time.Minute)
	h.drive(t)
	if _, w = h.current(t); w.State != Failed || !strings.Contains(w.Reason, string(turn.InputWaitTimeout)) {
		t.Fatalf("worker %s: %s", w.State, w.Reason)
	}
	if expired, err := h.engine.turns().Get(waiting.ID); err != nil || expired.State != turn.Failed {
		t.Fatalf("turn %+v %v", expired, err)
	}
	if h.session.interrupts != 1 || h.session.closes != 1 {
		t.Fatalf("%d interrupts, %d closes", h.session.interrupts, h.session.closes)
	}
}

func TestEngineBlocksATurnThatTimesOut(t *testing.T) {
	h := newEngineHarness(t, config.Defaults(), detect(agent.DetectRunning))
	h.drive(t)

	got, w := h.current(t)
	if got.State != task.Blocked || got.ResumeState != task.Planning || !strings.Contains(got.BlockedReason, string(turn.TurnTimeout)) {
		t.Fatalf("task %s resume %s: %s", got.State, got.ResumeState, got.BlockedReason)
	}
	if w.State != Failed || h.session.interrupts != 1 || h.session.closes != 1 {
		t.Fatalf("worker %s, %d interrupts, %d closes", w.State, h.session.interrupts, h.session.closes)
	}
	if turns := h.turnStates(t); len(turns) != 1 || turns[0].State != turn.Failed || turns[0].Reason != string(turn.TurnTimeout) {
		t.Fatalf("turns %+v", turns)
	}
	if h.clock.now().Before(h.task.CreatedAt.Add(20 * time.Minute)) {
		t.Fatalf("blocked at %s, before the turn timeout", h.clock.now())
	}
}

func TestEngineBlocksAFailedTurn(t *testing.T) {
	h := newEngineHarness(t, config.Defaults(), detect(agent.DetectFailed))
	h.drive(t)

	got, w := h.current(t)
	if got.State != task.Blocked || got.ResumeState != task.Planning || w.State != Failed {
		t.Fatalf("task %s resume %s, worker %s", got.State, got.ResumeState, w.State)
	}
	if h.session.closes != 1 {
		t.Fatalf("the session of the failed worker was closed %d times", h.session.closes)
	}
}

func TestEngineRevokesTheTurnsRightsBeforeReadingTheHandoff(t *testing.T) {
	h := newEngineHarness(t, config.Defaults(), planner)
	h.session.settleErr = errors.New("the agent did not resume")
	h.drive(t)

	got, w := h.current(t)
	if got.State != task.Blocked || got.ResumeState != task.Planning || !strings.Contains(got.BlockedReason, "revoke the turn's rights") {
		t.Fatalf("task %s resume %s: %s", got.State, got.ResumeState, got.BlockedReason)
	}
	if w.State != Failed || h.session.settles != 1 || h.session.closes != 1 {
		t.Fatalf("worker %s, %d settles, %d closes", w.State, h.session.settles, h.session.closes)
	}
	if _, err := handoff.Latest(h.engine.DB, h.task.ID, handoff.Plan); err == nil {
		t.Fatal("the handoff of a turn whose rights were not revoked was accepted")
	}
	if turns := h.turnStates(t); len(turns) != 1 || turns[0].State != turn.Failed {
		t.Fatalf("turns %+v", turns)
	}
}

func TestEngineBlocksATaskOverItsTurnBudget(t *testing.T) {
	cfg := testConfig()
	cfg.Budgets.MaxTurnsPerTask = 1
	h := newEngineHarness(t, cfg, planner)
	h.drive(t)

	got, w := h.current(t)
	if got.State != task.Blocked || got.ResumeState != task.Implementing || !strings.Contains(got.BlockedReason, budget.BoundTaskTurns) {
		t.Fatalf("task %s resume %s: %s", got.State, got.ResumeState, got.BlockedReason)
	}
	if w.State != Idle || w.Assignment != nil {
		t.Fatalf("worker %s holds %+v", w.State, w.Assignment)
	}
	turns := h.turnStates(t)
	if len(turns) != 2 || turns[1].State != turn.Failed || len(h.session.prompts) != 1 {
		t.Fatalf("turns %+v, %d prompts", turns, len(h.session.prompts))
	}
}

func TestEngineBlocksACommitOfARuntimePath(t *testing.T) {
	h := newEngineHarness(t, testConfig(), planner, coder("CLAUDE.md", "mine\n"))
	h.drive(t)

	got, w := h.current(t)
	if got.State != task.Blocked || got.ResumeState != task.Implementing || !strings.Contains(got.BlockedReason, "CLAUDE.md") {
		t.Fatalf("task %s resume %s: %s", got.State, got.ResumeState, got.BlockedReason)
	}
	if w.State != Idle {
		t.Fatalf("worker %s", w.State)
	}
}

func TestEngineWaitsForAWorkerWithALiveSession(t *testing.T) {
	h := newEngineHarness(t, config.Defaults())
	h.sessions = map[string]Session{}
	h.drive(t)
	if got, w := h.current(t); got.State != task.New || w.State != Idle {
		t.Fatalf("task %s, worker %s", got.State, w.State)
	}
	if turns := h.turnStates(t); len(turns) != 0 {
		t.Fatalf("turns %+v", turns)
	}
}

func TestEngineBlocksTestingWithoutATestCommand(t *testing.T) {
	h := newEngineHarness(t, config.Defaults(), planner, coder("flag.txt", "on\n"))
	h.drive(t)
	got, _ := h.current(t)
	if got.State != task.Blocked || got.ResumeState != task.Testing || got.BlockedReason != testrun.ErrNoCommands.Error() {
		t.Fatalf("task %s resume %s: %s", got.State, got.ResumeState, got.BlockedReason)
	}
	if len(h.tester.runs) != 0 {
		t.Fatalf("tests ran: %v", h.tester.runs)
	}
}

func TestEngineResumesABlockedTask(t *testing.T) {
	h := newEngineHarness(t, testConfig(), planner, coder("flag.txt", "on\n"), reviewer("approve"))
	h.engine.Tester = nil
	h.drive(t)
	if got, _ := h.current(t); got.State != task.Blocked || got.ResumeState != task.Testing {
		t.Fatalf("task %s resume %s: %s", got.State, got.ResumeState, got.BlockedReason)
	}
	h.engine.Tester = h.tester
	if _, err := h.engine.tasks().Transition(h.task.ID, task.Input{Event: task.Resume, Reason: "runner installed",
		Guard: task.Guard{CauseLifted: true, Reconciled: true}}); err != nil {
		t.Fatal(err)
	}
	h.drive(t)
	if got, _ := h.current(t); got.State != task.ReadyToIntegrate {
		t.Fatalf("task %s: %s", got.State, got.Reason)
	}
}

func TestEngineRepairsAReviewOfAnotherDiff(t *testing.T) {
	stale := func(s *fakeSession, prompt string) agent.Detection {
		s.write(prompt, func(_, payload map[string]any) {
			payload["verdict"], payload["issues"], payload["diff_digest"] = "approve", []any{}, strings.Repeat("0", 64)
		})
		return agent.DetectCompleted
	}
	h := newEngineHarness(t, testConfig(), planner, coder("flag.txt", "on\n"), stale, reviewer("approve"))
	h.drive(t)

	got, _ := h.current(t)
	if got.State != task.ReadyToIntegrate {
		t.Fatalf("task %s: %s", got.State, got.Reason)
	}
	repair := h.session.prompts[3]
	if !strings.Contains(repair, "is not the digest") {
		t.Fatalf("repair prompt:\n%s", repair)
	}
	turns := h.turnStates(t)
	if len(turns) != 4 || turns[2].State != turn.Failed || turns[3].PreviousID != turns[2].ID {
		t.Fatalf("turns %+v", turns)
	}
}

// openStep reads the step of the task's open active interval, empty
// when none is open.
func (h *engineHarness) openStep(t *testing.T) string {
	t.Helper()
	var step string
	err := h.engine.DB.QueryRow(`SELECT open_step FROM budget_time WHERE task_id = ?`, h.task.ID).Scan(&step)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return step
}

func TestEngineEndsTheTurnItStopsDrivingOnCancellation(t *testing.T) {
	h := newEngineHarness(t, config.Defaults(), detect(agent.DetectRunning))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.engine.Wait = func(ctx context.Context, d time.Duration) error {
		h.clock.advance(d)
		cancel()
		return ctx.Err()
	}
	if err := h.engine.Drive(ctx, h.project); !errors.Is(err, context.Canceled) {
		t.Fatalf("drive: %v", err)
	}

	got, w := h.current(t)
	if got.State != task.Blocked || got.ResumeState != task.Planning || !strings.Contains(got.BlockedReason, "stopped driving") {
		t.Fatalf("task %s resume %s: %s", got.State, got.ResumeState, got.BlockedReason)
	}
	if w.State != Failed || h.session.interrupts != 1 || h.session.closes != 1 {
		t.Fatalf("worker %s, %d interrupts, %d closes", w.State, h.session.interrupts, h.session.closes)
	}
	if turns := h.turnStates(t); len(turns) != 1 || turns[0].State != turn.Interrupted {
		t.Fatalf("turns %+v", turns)
	}
	if step := h.openStep(t); step != "" {
		t.Fatalf("interval %s left open", step)
	}
	h.drive(t)
	if got, _ := h.current(t); got.State != task.Blocked {
		t.Fatalf("task %s after another drive", got.State)
	}
}

func TestEngineRecoversATurnNoDriveFollows(t *testing.T) {
	h := newEngineHarness(t, config.Defaults())
	u, err := h.engine.turns().Create(h.task.ID, h.worker.Agent, h.task.ConfigID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.workers().Transition(h.project.ID, h.worker.Name, Input{Event: Assign,
		Assignment: Assignment{TaskID: h.task.ID, Role: Planning, TurnID: u.ID},
		Reason:     "planning turn", Guard: Guard{AssignmentPersisted: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.tasks().Transition(h.task.ID, task.Input{Event: task.Assign, Reason: "assigned",
		Guard: task.Guard{AssignmentAvailable: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.budgets().Start(h.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.turns().Transition(u.ID, turn.Running, "prompt sent"); err != nil {
		t.Fatal(err)
	}

	if !h.engine.begin(h.project.ID) {
		t.Fatal("another drive advances the project")
	}
	h.drive(t)
	if _, w := h.current(t); w.State != Busy || h.session.interrupts != 0 {
		t.Fatalf("worker %s, %d interrupts, while a drive follows its turn", w.State, h.session.interrupts)
	}
	h.engine.release(h.project.ID)

	h.clock.advance(time.Minute)
	h.drive(t)
	got, w := h.current(t)
	if got.State != task.Blocked || got.ResumeState != task.Planning || !strings.Contains(got.BlockedReason, "no engine follows") {
		t.Fatalf("task %s resume %s: %s", got.State, got.ResumeState, got.BlockedReason)
	}
	if w.State != Failed || h.session.interrupts != 1 || h.session.closes != 1 {
		t.Fatalf("worker %s, %d interrupts, %d closes", w.State, h.session.interrupts, h.session.closes)
	}
	if recovered, err := h.engine.turns().Get(u.ID); err != nil || recovered.State != turn.Interrupted {
		t.Fatalf("turn %+v %v", recovered, err)
	}
	if step := h.openStep(t); step != "" {
		t.Fatalf("interval %s left open", step)
	}
	usage, err := h.engine.budgets().Check(h.task.ID)
	if err != nil || usage.Active != time.Minute {
		t.Fatalf("usage %+v %v", usage, err)
	}
}

// assignTurn assigns a new planning turn of the harness's task to its
// worker and returns the worker as stored.
func (h *engineHarness) assignTurn(t *testing.T) Worker {
	t.Helper()
	u, err := h.engine.turns().Create(h.task.ID, h.worker.Agent, h.task.ConfigID)
	if err != nil {
		t.Fatal(err)
	}
	w, err := h.engine.workers().Transition(h.project.ID, h.worker.Name, Input{Event: Assign,
		Assignment: Assignment{TaskID: h.task.ID, Role: Planning, TurnID: u.ID},
		Reason:     "planning turn", Guard: Guard{AssignmentPersisted: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.turns().Transition(u.ID, turn.Running, "prompt sent"); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestEngineAbortLeavesAWorkerThatMovedOn(t *testing.T) {
	h := newEngineHarness(t, config.Defaults())
	stale := h.assignTurn(t)
	if _, err := h.engine.tasks().Transition(h.task.ID, task.Input{Event: task.Assign, Reason: "assigned",
		Guard: task.Guard{AssignmentAvailable: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.budgets().Start(h.task.ID); err != nil {
		t.Fatal(err)
	}
	for _, to := range []turn.State{turn.Validating, turn.Succeeded} {
		if _, err := h.engine.turns().Transition(stale.Assignment.TurnID, to, "turn completed"); err != nil {
			t.Fatal(err)
		}
	}
	idle, err := h.engine.workers().Transition(h.project.ID, h.worker.Name, Input{Event: AcceptTurn,
		Reason: "turn accepted", Guard: Guard{RightsRevoked: true, Reconciled: true}})
	if err != nil || idle.State != Idle {
		t.Fatalf("worker %s: %v", idle.State, err)
	}

	check := func(state State, turnID string) {
		t.Helper()
		got, w := h.current(t)
		if w.State != state || h.session.interrupts != 0 || h.session.closes != 0 {
			t.Fatalf("worker %s, %d interrupts after a stale abort", w.State, h.session.interrupts)
		}
		if turnID != "" && (w.Assignment == nil || w.Assignment.TurnID != turnID) {
			t.Fatalf("worker holds %+v, not turn %s", w.Assignment, turnID)
		}
		if got.State != task.Planning {
			t.Fatalf("task %s: %s", got.State, got.BlockedReason)
		}
		if step := h.openStep(t); step == "" {
			t.Fatal("a stale abort closed the active interval")
		}
	}
	if err := h.engine.abort(h.project, stale, "stale"); err != nil {
		t.Fatal(err)
	}
	check(Idle, "")

	busy := h.assignTurn(t)
	if err := h.engine.abort(h.project, stale, "stale"); err != nil {
		t.Fatal(err)
	}
	check(Busy, busy.Assignment.TurnID)
	if u, err := h.engine.turns().Get(busy.Assignment.TurnID); err != nil || u.State != turn.Running {
		t.Fatalf("turn %+v %v", u, err)
	}
}

func TestEngineRunsOneDriveOfAProjectAtATime(t *testing.T) {
	var pending string
	slowPlanner := func(_ *fakeSession, prompt string) agent.Detection {
		pending = prompt
		return agent.DetectRunning
	}
	h := newEngineHarness(t, testConfig(), slowPlanner, coder("flag.txt", "on\n"), reviewer("approve"))
	other := &Engine{DB: h.engine.DB, Sessions: h, Tester: h.tester, Now: h.clock.now, PollInterval: time.Minute}
	others := 0
	h.engine.Wait = func(ctx context.Context, d time.Duration) error {
		h.clock.advance(d)
		if pending == "" {
			return nil
		}
		others++
		if err := other.Drive(ctx, h.project); err != nil {
			t.Fatalf("concurrent drive: %v", err)
		}
		if _, w := h.current(t); w.State != Busy || h.session.interrupts != 0 {
			t.Fatalf("worker %s, %d interrupts after a concurrent drive", w.State, h.session.interrupts)
		}
		h.session.detection = planner(h.session, pending)
		pending = ""
		return nil
	}
	h.drive(t)

	got, w := h.current(t)
	if others != 1 || got.State != task.ReadyToIntegrate || w.State != Idle || h.session.interrupts != 0 {
		t.Fatalf("%d concurrent drives, task %s, worker %s, %d interrupts: %s",
			others, got.State, w.State, h.session.interrupts, got.BlockedReason)
	}
	if !other.begin(h.project.ID) {
		t.Fatal("the project is still advanced after its drive returned")
	}
	other.release(h.project.ID)
}
