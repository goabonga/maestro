// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/agent"
	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/scheduler"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/worktree"
)

// Fixture agents: each answers --version on the host like the real CLI,
// and runs inside the confined PTY otherwise.
const (
	// claudeFixture records its chosen session in its private
	// configuration, as Claude Code does, then waits on its terminal.
	claudeFixture = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$2" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$2.jsonl"
echo READY
exec cat
`
	// codexFixture records its own session in its private home, as
	// Codex does, then waits on its terminal.
	codexFixture = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli 0.160.0"; exit 0; fi
mkdir -p "$CODEX_HOME/sessions/2026/10/06"
printf '{"type":"session_meta","payload":{"id":"0199a0a0-0000-4000-8000-000000000001","cwd":"%s","cli_version":"0.160.0"}}\n' "$PWD" > "$CODEX_HOME/sessions/2026/10/06/rollout.jsonl"
echo READY
exec cat
`
	// crashingFixture exits before recording any session.
	crashingFixture = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
echo boom
exit 3
`
	// endingFixture records its session, then ends on its own.
	endingFixture = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$2" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$2.jsonl"
sleep 1
exit 0
`
	// signalledFixture records its session, then ends on its own once
	// the file end-session appears in its worktree.
	signalledFixture = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.289 (Claude Code)"; exit 0; fi
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$2" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$2.jsonl"
echo READY
while [ ! -e end-session ]; do sleep 0.05; done
exit 0
`
)

// harness is a supervisor over a real project, with fixture agents
// installed in a private bin directory.
type harness struct {
	supervisor *Supervisor
	project    worktree.Project
	capacity   *scheduler.Capacity
}

// confined returns the host's launcher, skipping hosts that cannot
// confine.
func confined(t *testing.T) *launcher.Launcher {
	t.Helper()
	l, err := launcher.New()
	if errors.Is(err, launcher.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestMain runs the tests without any GIT_* variable of the caller and
// without the system or global Git configuration: a suite started from a
// git-spawned command (a hook, rebase --exec) inherits GIT_DIR and its
// siblings, which would point the git commands of the tests and of the
// project store at the caller's repository instead of a temporary one.
func TestMain(m *testing.M) {
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, "GIT_") {
			_ = os.Unsetenv(name)
		}
	}
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	_ = os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Exit(m.Run())
}

// testGit runs one git command of a test in dir, without any GIT_*
// variable set since TestMain and without the system or global
// configuration.
func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); !strings.HasPrefix(name, "GIT_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// userRepository creates a repository with one commit.
func userRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"},
		{"config", "commit.gpgsign", "false"}, {"commit", "-q", "--allow-empty", "-m", "feat: initial"},
	} {
		testGit(t, dir, args...)
	}
	return dir
}

func TestUserRepositoryIgnoresAnInheritedGitDir(t *testing.T) {
	victim := t.TempDir()
	testGit(t, victim, "init", "-q", "-b", "trunk")
	fingerprint := func() string {
		t.Helper()
		var parts []string
		for _, name := range []string{"config", "HEAD"} {
			data, err := os.ReadFile(filepath.Join(victim, ".git", name)) // #nosec G304 -- the test's own temporary repository
			if err != nil {
				t.Fatal(err)
			}
			parts = append(parts, string(data))
		}
		return strings.Join(append(parts, testGit(t, victim, "for-each-ref")), "\n--\n")
	}
	before := fingerprint()
	t.Setenv("GIT_DIR", filepath.Join(victim, ".git"))
	t.Setenv("GIT_WORK_TREE", victim)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(victim, ".git", "index"))
	user := userRepository(t)
	if log := testGit(t, user, "log", "--format=%s"); log != "feat: initial" {
		t.Fatalf("the test repository has %q", log)
	}
	if after := fingerprint(); after != before {
		t.Fatalf("the victim repository changed:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(victim, ".git", "index")); !os.IsNotExist(err) {
		t.Fatalf("the victim repository got an index: %v", err)
	}
}

// newHarness builds a supervisor with the given session ceiling and the
// fixture binaries, by binary name.
func newHarness(t *testing.T, l *launcher.Launcher, sessions int, binaries map[string]string) harness {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	projects := worktree.Store{Base: t.TempDir()}
	project, _, err := projects.Init(userRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	capacity, err := scheduler.NewCapacity(sessions, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for name, script := range binaries {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil { // #nosec G306 -- an executable fixture
			t.Fatal(err)
		}
	}
	supervisor := &Supervisor{
		Store: Store{DB: db}, Projects: projects, Capacity: capacity, Launcher: l,
		LookPath: func(name string) (string, error) {
			path := filepath.Join(bin, name)
			if _, err := os.Stat(path); err != nil {
				return "", err
			}
			return path, nil
		},
		StartTimeout: 15 * time.Second, StopGrace: 200 * time.Millisecond,
	}
	t.Cleanup(supervisor.Close)
	return harness{supervisor: supervisor, project: project, capacity: capacity}
}

// request asks for count workers of an agent.
func (h harness) request(agentName string, count int, snapshot config.Snapshot) StartRequest {
	return StartRequest{Project: h.project, Agent: agentName, Count: count, ConfigID: "sha256-test", Snapshot: snapshot}
}

// settle waits for a worker to leave STARTING, then for the given state.
func (h harness) settle(t *testing.T, name string, want State) Worker {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		w, err := h.supervisor.Store.Get(h.project.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if w.State == want {
			return w
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s (%s), want %s", name, w.State, w.Reason, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sessionsUsed returns the session slots in use.
func (h harness) sessionsUsed() int {
	return h.capacity.Usage()[scheduler.Sessions].Used
}

// events returns the recorded events of a worker, by name.
func (h harness) events(t *testing.T, name string) []Event {
	t.Helper()
	records, err := h.supervisor.Store.Events(h.project.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	for _, record := range records {
		events = append(events, record.Event)
	}
	return events
}

// nativeIdentity reads the persisted native identity of a worker.
func (h harness) nativeIdentity(t *testing.T, name string) agent.NativeIdentity {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.project.Dir, "workers", name, "supervisor", "native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var identity agent.NativeIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestSupervisorStartsWorkersUpToCapacityAndStopsThem(t *testing.T) {
	h := newHarness(t, confined(t), 2, map[string]string{"claude": claudeFixture})
	snapshot := config.Snapshot{
		Config:       config.Defaults(),
		Instructions: map[string]string{"agents/claude-code/CLAUDE.md": "work carefully\n"},
	}
	snapshot.Config.MCP = map[string]config.MCP{"github": {Command: "github-mcp", Scope: "shared"}}

	started, err := h.supervisor.Start(context.Background(), h.request("claude-code", 2, snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 2 || started[0].Name != "claude-code-01" || started[1].Name != "claude-code-02" ||
		started[0].State != Starting || started[0].Driver != "claude-code-2.1" || started[0].AgentKind != "claude-code" {
		t.Fatalf("started %+v", started)
	}
	for _, w := range started {
		idle := h.settle(t, w.Name, Idle)
		identity := h.nativeIdentity(t, w.Name)
		if !identity.Confirmed || !strings.Contains(idle.Reason, identity.ID) {
			t.Fatalf("%s is idle without its confirmed native session: %+v %q", w.Name, identity, idle.Reason)
		}
		workTree := h.project.WorkerWorktree(w.Name)
		if data, err := os.ReadFile(filepath.Join(workTree, "CLAUDE.md")); err != nil || string(data) != "work carefully\n" {
			t.Fatalf("instructions of %s: %q %v", w.Name, data, err)
		}
		mcp, err := os.ReadFile(filepath.Join(h.project.Dir, "workers", w.Name, "home", ".claude", ".claude.json"))
		if err != nil || !strings.Contains(string(mcp), "github-mcp") {
			t.Fatalf("MCP configuration of %s: %q %v", w.Name, mcp, err)
		}
	}
	if h.sessionsUsed() != 2 {
		t.Fatalf("sessions used %d", h.sessionsUsed())
	}

	// Above max_sessions, nothing is registered nor started.
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); !errors.Is(err, scheduler.ErrFull) {
		t.Fatalf("a start above capacity was accepted: %v", err)
	}
	if workers, _ := h.supervisor.Store.List(h.project.ID); len(workers) != 2 {
		t.Fatalf("workers %+v", workers)
	}

	stopped, err := h.supervisor.Stop(h.project.ID, "claude-code-01")
	if err != nil || stopped.State != Stopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	if h.sessionsUsed() != 1 {
		t.Fatalf("the stop kept its slot: %d used", h.sessionsUsed())
	}
	events := h.events(t, "claude-code-01")
	if got := events[len(events)-2:]; got[0] != ScaleDown || got[1] != Drained {
		t.Fatalf("events %v", events)
	}
	if _, err := h.supervisor.Stop(h.project.ID, "claude-code-01"); !errors.Is(err, ErrTransition) {
		t.Fatalf("a stopped worker was stopped again: %v", err)
	}

	// A stopped worker of the same agent is started again by name.
	again, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot))
	if err != nil || len(again) != 1 || again[0].Name != "claude-code-01" {
		t.Fatalf("restart: %+v %v", again, err)
	}
	h.settle(t, "claude-code-01", Idle)

	h.supervisor.Close()
	for _, name := range []string{"claude-code-01", "claude-code-02"} {
		h.settle(t, name, Stopped)
	}
	if h.sessionsUsed() != 0 {
		t.Fatalf("close kept %d slots", h.sessionsUsed())
	}
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); !errors.Is(err, ErrClosed) {
		t.Fatalf("a closed supervisor started a worker: %v", err)
	}
}

func TestSupervisorConfirmsCodexSessionsThroughTheirMetadata(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"codex": codexFixture})
	snapshot := config.Snapshot{Config: config.Defaults()}
	snapshot.Config.Agents = map[string]config.Agent{"coder": {Driver: "codex"}}
	snapshot.Config.MCP = map[string]config.MCP{"docs": {Command: "docs-mcp", Scope: "agent:coder"}}

	started, err := h.supervisor.Start(context.Background(), h.request("coder", 1, snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if started[0].Name != "coder-01" || started[0].Agent != "coder" || started[0].AgentKind != "codex" || started[0].Driver != "codex-0.160" {
		t.Fatalf("started %+v", started)
	}
	h.settle(t, "coder-01", Idle)
	if identity := h.nativeIdentity(t, "coder-01"); !identity.Confirmed || identity.ID != "0199a0a0-0000-4000-8000-000000000001" {
		t.Fatalf("identity %+v", identity)
	}
	mcp, err := os.ReadFile(filepath.Join(h.project.Dir, "workers", "coder-01", "home", ".codex", "config.toml"))
	if err != nil || !strings.Contains(string(mcp), "[mcp_servers.docs]") {
		t.Fatalf("MCP configuration: %q %v", mcp, err)
	}
	if _, err := h.supervisor.Stop(h.project.ID, "coder-01"); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorFailsAWorkerWhoseAgentExitsBeforeConfirmation(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": crashingFixture})
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, config.Snapshot{Config: config.Defaults()})); err != nil {
		t.Fatal(err)
	}
	failed := h.settle(t, "claude-code-01", Failed)
	if !strings.Contains(failed.Reason, "exited with code 3") {
		t.Fatalf("reason %q", failed.Reason)
	}
	if h.sessionsUsed() != 0 {
		t.Fatalf("a failed start kept its slot: %d used", h.sessionsUsed())
	}
	if _, err := h.supervisor.Stop(h.project.ID, "claude-code-01"); err != nil {
		t.Fatal(err)
	}
	h.settle(t, "claude-code-01", Stopped)
}

func TestSupervisorFailsAWorkerWhoseSessionEnds(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": endingFixture})
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, config.Snapshot{Config: config.Defaults()})); err != nil {
		t.Fatal(err)
	}
	failed := h.settle(t, "claude-code-01", Failed)
	events := h.events(t, "claude-code-01")
	if !strings.Contains(failed.Reason, "session exited with code 0") || events[len(events)-2] != Ready {
		t.Fatalf("reason %q, events %v", failed.Reason, events)
	}
	if h.sessionsUsed() != 0 {
		t.Fatalf("an ended session kept its slot: %d used", h.sessionsUsed())
	}
}

func TestSupervisorRefusesAStopWhileASelfEndedSessionFails(t *testing.T) {
	h := newHarness(t, confined(t), 2, map[string]string{"claude": signalledFixture})
	entered := make(chan struct{})
	gate := make(chan struct{})
	opened := false
	open := func() {
		if !opened {
			opened = true
			close(gate)
		}
	}
	defer open()
	var once sync.Once
	h.supervisor.tearDown = func(live *running) error {
		once.Do(func() { close(entered) })
		<-gate
		return h.supervisor.teardown(live)
	}
	snapshot := config.Snapshot{Config: config.Defaults()}
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	h.settle(t, "claude-code-01", Idle)
	if err := os.WriteFile(filepath.Join(h.project.WorkerWorktree("claude-code-01"), "end-session"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-entered

	// The watcher is tearing the ended session down and has not recorded
	// the failure yet: a stop is refused and changes nothing, and the
	// slot is still held.
	refused, err := h.supervisor.Stop(h.project.ID, "claude-code-01")
	if !errors.Is(err, ErrTransition) || !strings.Contains(err.Error(), "being torn down") || refused.State != Idle {
		t.Fatalf("a stop took a failing worker: %+v %v", refused, err)
	}
	if h.sessionsUsed() != 1 {
		t.Fatalf("the slot was released before the group ended: %d used", h.sessionsUsed())
	}
	open()
	failed := h.settle(t, "claude-code-01", Failed)
	if !strings.Contains(failed.Reason, "session exited with code 0") {
		t.Fatalf("reason %q", failed.Reason)
	}
	// Once the failure is recorded, the worker stops normally.
	deadline := time.Now().Add(5 * time.Second)
	for {
		stopped, err := h.supervisor.Stop(h.project.ID, "claude-code-01")
		if err == nil {
			if stopped.State != Stopped {
				t.Fatalf("stop: %+v", stopped)
			}
			break
		}
		if !errors.Is(err, ErrTransition) || time.Now().After(deadline) {
			t.Fatalf("the failed worker was not stopped: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h.sessionsUsed() != 0 {
		t.Fatalf("the ended session kept its slot: %d used", h.sessionsUsed())
	}
	if events := h.events(t, "claude-code-01"); events[len(events)-1] != Stop || events[len(events)-2] != Fail {
		t.Fatalf("events %v", events)
	}
}

func TestSupervisorRefusesWhatItCannotStart(t *testing.T) {
	// The launcher is never used: every request is refused before.
	h := newHarness(t, &launcher.Launcher{}, 1, map[string]string{"claude": claudeFixture})
	snapshot := config.Snapshot{Config: config.Defaults()}
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 0, snapshot)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a count of 0 was accepted: %v", err)
	}
	_, err := h.supervisor.Start(context.Background(), h.request("gpt", 1, snapshot))
	if !errors.Is(err, ErrAgent) || !errors.Is(err, agent.ErrNoDriver) {
		t.Fatalf("an unknown agent was accepted: %v", err)
	}
	if _, err := h.supervisor.Start(context.Background(), h.request("codex", 1, snapshot)); !errors.Is(err, ErrAgent) {
		t.Fatalf("an agent without its binary was accepted: %v", err)
	}
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 2, snapshot)); !errors.Is(err, scheduler.ErrFull) {
		t.Fatalf("a count above capacity was accepted: %v", err)
	}
	if workers, _ := h.supervisor.Store.List(h.project.ID); len(workers) != 0 || h.sessionsUsed() != 0 {
		t.Fatalf("a refused start left workers %+v or slots %d", workers, h.sessionsUsed())
	}
	if _, err := h.supervisor.Stop(h.project.ID, "claude-code-01"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown worker was stopped: %v", err)
	}

	h.supervisor.Launcher = nil
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); !errors.Is(err, launcher.ErrUnsupported) {
		t.Fatalf("a host without sandbox started a worker: %v", err)
	}
}

func TestSupervisorRefusesAConcurrentStop(t *testing.T) {
	h := newHarness(t, confined(t), 2, map[string]string{"claude": claudeFixture})
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	opened := false
	open := func() {
		if !opened {
			opened = true
			close(gate)
		}
	}
	defer open()
	h.supervisor.tearDown = func(live *running) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
		return h.supervisor.teardown(live)
	}
	snapshot := config.Snapshot{Config: config.Defaults()}
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	h.settle(t, "claude-code-01", Idle)

	type result struct {
		worker Worker
		err    error
	}
	first := make(chan result, 1)
	go func() {
		w, err := h.supervisor.Stop(h.project.ID, "claude-code-01")
		first <- result{w, err}
	}()
	<-entered
	// The first stop is tearing the group down: a second one is refused
	// and changes nothing.
	second, err := h.supervisor.Stop(h.project.ID, "claude-code-01")
	if !errors.Is(err, ErrTransition) || !strings.Contains(err.Error(), "already being stopped") || second.State != Draining {
		t.Fatalf("a concurrent stop was not refused: %+v %v", second, err)
	}
	if h.sessionsUsed() != 1 {
		t.Fatalf("the slot was released before the group ended: %d used", h.sessionsUsed())
	}
	open()
	got := <-first
	if got.err != nil || got.worker.State != Stopped {
		t.Fatalf("first stop: %+v %v", got.worker, got.err)
	}
	if h.sessionsUsed() != 0 {
		t.Fatalf("the stop kept its slot: %d used", h.sessionsUsed())
	}
	if events := h.events(t, "claude-code-01"); events[len(events)-1] != Drained || events[len(events)-2] != ScaleDown {
		t.Fatalf("events %v", events)
	}
}

func TestSupervisorKeepsTheSlotOfAGroupItCannotTerminate(t *testing.T) {
	h := newHarness(t, confined(t), 2, map[string]string{"claude": claudeFixture})
	var stuck atomic.Bool
	h.supervisor.tearDown = func(live *running) error {
		if stuck.Load() {
			return errors.New("the group is stuck")
		}
		return h.supervisor.teardown(live)
	}
	snapshot := config.Snapshot{Config: config.Defaults()}
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	h.settle(t, "claude-code-01", Idle)

	stuck.Store(true)
	failed, err := h.supervisor.Stop(h.project.ID, "claude-code-01")
	if err == nil || failed.State != Failed || !strings.Contains(failed.Reason, "the group is stuck") {
		t.Fatalf("a failed teardown: %+v %v", failed, err)
	}
	if h.sessionsUsed() != 1 {
		t.Fatalf("a group that may be alive lost its slot: %d used", h.sessionsUsed())
	}
	// A new stop retries the termination: it fails again, so the worker
	// stays FAILED and its name is not reused while its group may be
	// alive.
	if still, err := h.supervisor.Stop(h.project.ID, "claude-code-01"); err == nil || still.State != Failed {
		t.Fatalf("a stop over a stuck group: %+v %v", still, err)
	}
	stuck.Store(false)
	again, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot))
	if err != nil || len(again) != 1 || again[0].Name != "claude-code-02" {
		t.Fatalf("a worker whose group may be alive was reused: %+v %v", again, err)
	}
	h.settle(t, "claude-code-02", Idle)
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); !errors.Is(err, scheduler.ErrFull) {
		t.Fatalf("the stuck group's slot was handed out: %v", err)
	}

	// Once the group is gone, its slot is released and the name free.
	h.supervisor.mu.Lock()
	live := h.supervisor.ending[liveKey{h.project.ID, "claude-code-01"}]
	h.supervisor.mu.Unlock()
	if live == nil {
		t.Fatal("the stuck group is not tracked")
	}
	if err := h.supervisor.teardown(live); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.sessionsUsed() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the ended group kept its slot: %d used", h.sessionsUsed())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stopped, err := h.supervisor.Stop(h.project.ID, "claude-code-01"); err != nil || stopped.State != Stopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	reused, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot))
	if err != nil || len(reused) != 1 || reused[0].Name != "claude-code-01" {
		t.Fatalf("restart: %+v %v", reused, err)
	}
	h.settle(t, "claude-code-01", Idle)
}

// stuckHarness starts one worker whose teardown fails while stuck is
// set, then stops it with stuck set: the worker is FAILED and its group
// alive.
func stuckHarness(t *testing.T) (harness, *atomic.Bool) {
	t.Helper()
	h := newHarness(t, confined(t), 1, map[string]string{"claude": claudeFixture})
	stuck := &atomic.Bool{}
	h.supervisor.tearDown = func(live *running) error {
		if stuck.Load() {
			return errors.New("the group is stuck")
		}
		return h.supervisor.teardown(live)
	}
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, config.Snapshot{Config: config.Defaults()})); err != nil {
		t.Fatal(err)
	}
	h.settle(t, "claude-code-01", Idle)
	stuck.Store(true)
	if failed, err := h.supervisor.Stop(h.project.ID, "claude-code-01"); err == nil || failed.State != Failed {
		t.Fatalf("a failed teardown: %+v %v", failed, err)
	}
	return h, stuck
}

func TestSupervisorStopRetriesAFailedTeardown(t *testing.T) {
	h, stuck := stuckHarness(t)
	stuck.Store(false)
	stopped, err := h.supervisor.Stop(h.project.ID, "claude-code-01")
	if err != nil || stopped.State != Stopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	h.supervisor.mu.Lock()
	ending := len(h.supervisor.ending)
	h.supervisor.mu.Unlock()
	if ending != 0 || h.sessionsUsed() != 0 {
		t.Fatalf("the retried stop left %d sessions and %d slots", ending, h.sessionsUsed())
	}
	reused, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, config.Snapshot{Config: config.Defaults()}))
	if err != nil || len(reused) != 1 || reused[0].Name != "claude-code-01" {
		t.Fatalf("restart: %+v %v", reused, err)
	}
	h.settle(t, "claude-code-01", Idle)
}

func TestSupervisorCloseRetriesAFailedTeardown(t *testing.T) {
	h, stuck := stuckHarness(t)
	stuck.Store(false)
	closed := make(chan struct{})
	go func() {
		h.supervisor.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(20 * time.Second):
		t.Fatal("Close hangs on a group a stop could not terminate")
	}
	h.supervisor.mu.Lock()
	ending := len(h.supervisor.ending)
	h.supervisor.mu.Unlock()
	if ending != 0 || h.sessionsUsed() != 0 {
		t.Fatalf("close left %d sessions and %d slots", ending, h.sessionsUsed())
	}
	if w := h.settle(t, "claude-code-01", Failed); w.State != Failed {
		t.Fatalf("worker %+v", w)
	}
}

func TestSupervisorCloseBoundsItsWaitForAStuckGroup(t *testing.T) {
	h, stuck := stuckHarness(t)
	h.supervisor.CloseTimeout = 300 * time.Millisecond
	closed := make(chan struct{})
	go func() {
		h.supervisor.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(20 * time.Second):
		t.Fatal("Close waits forever for a group it cannot terminate")
	}
	if h.sessionsUsed() != 1 {
		t.Fatalf("a group that may be alive lost its slot: %d used", h.sessionsUsed())
	}

	// The test ends the group itself.
	stuck.Store(false)
	h.supervisor.mu.Lock()
	live := h.supervisor.ending[liveKey{h.project.ID, "claude-code-01"}]
	h.supervisor.mu.Unlock()
	if live == nil {
		t.Fatal("the stuck group is not tracked")
	}
	if err := h.supervisor.teardown(live); err != nil {
		t.Fatal(err)
	}
	h.supervisor.watchers.Wait()
}

func TestSupervisorCloseTearsDownWhatStopLeaves(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": claudeFixture})
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, config.Snapshot{Config: config.Defaults()})); err != nil {
		t.Fatal(err)
	}
	h.settle(t, "claude-code-01", Idle)
	// The store refuses every transition: Stop returns before its
	// teardown.
	if _, err := h.supervisor.Store.DB.Exec(`CREATE TRIGGER refuse BEFORE UPDATE ON workers
		BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.supervisor.Stop(h.project.ID, "claude-code-01"); err == nil {
		t.Fatal("the stop was not refused")
	}
	if h.sessionsUsed() != 1 {
		t.Fatalf("a refused stop released the slot: %d used", h.sessionsUsed())
	}
	closed := make(chan struct{})
	go func() {
		h.supervisor.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(20 * time.Second):
		t.Fatal("Close hangs on a session Stop left behind")
	}
	h.supervisor.mu.Lock()
	left := len(h.supervisor.live) + len(h.supervisor.ending)
	h.supervisor.mu.Unlock()
	if left != 0 || h.sessionsUsed() != 0 {
		t.Fatalf("close left %d sessions and %d slots", left, h.sessionsUsed())
	}
}

func TestSupervisorRestartsOverADirtyWorktree(t *testing.T) {
	h := newHarness(t, confined(t), 1, map[string]string{"claude": claudeFixture})
	snapshot := config.Snapshot{Config: config.Defaults()}
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	h.settle(t, "claude-code-01", Idle)
	if _, err := h.supervisor.Stop(h.project.ID, "claude-code-01"); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(h.project.WorkerWorktree("claude-code-01"), "draft.txt")
	if err := os.WriteFile(draft, []byte("left over\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot))
	if err != nil || len(again) != 1 || again[0].Name != "claude-code-01" {
		t.Fatalf("restart: %+v %v", again, err)
	}
	h.settle(t, "claude-code-01", Idle)
	if _, err := os.Stat(draft); !os.IsNotExist(err) {
		t.Fatalf("the previous run's work survived the restart: %v", err)
	}
}

func TestSupervisorRegistersNothingWhenAStartIsRefused(t *testing.T) {
	h := newHarness(t, confined(t), 3, map[string]string{"claude": claudeFixture})
	snapshot := config.Snapshot{Config: config.Defaults()}
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	h.settle(t, "claude-code-01", Idle)
	if _, err := h.supervisor.Stop(h.project.ID, "claude-code-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.supervisor.Store.DB.Exec(`CREATE TRIGGER refuse BEFORE UPDATE ON workers
		WHEN NEW.name = 'claude-code-03' BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	// claude-code-01 is reused and claude-code-02 registered, both
	// STARTING, before claude-code-03 is refused.
	if _, err := h.supervisor.Start(context.Background(), h.request("claude-code", 3, snapshot)); err == nil {
		t.Fatal("the start was not refused")
	}
	workers, err := h.supervisor.Store.List(h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].Name != "claude-code-01" || workers[0].State != Stopped {
		t.Fatalf("a refused start left %+v", workers)
	}
	if events := h.events(t, "claude-code-01"); events[len(events)-2] != Start || events[len(events)-1] != Stop {
		t.Fatalf("events %v", events)
	}
	if h.sessionsUsed() != 0 {
		t.Fatalf("a refused start kept %d slots", h.sessionsUsed())
	}
}

func TestSupervisorStopsAFailedWorkerThatHoldsAnAssignment(t *testing.T) {
	e := fixture(t)
	register(t, e, "claude-01")
	step(t, e, "claude-01",
		Input{Event: Start, Guard: Guard{CapacityReserved: true}},
		Input{Event: Ready, Guard: Guard{SessionReady: true, ProfileConfirmed: true}},
		e.assign(e.turns[0]),
		Input{Event: Fail, Reason: "the turn failed"})
	supervisor := &Supervisor{Store: e.store}
	stopped, err := supervisor.Stop(e.project.ID, "claude-01")
	if err != nil || stopped.State != Stopped || stopped.Assignment != nil {
		t.Fatalf("stop: %+v %v", stopped, err)
	}

	// A live worker holding an assignment is still refused.
	register(t, e, "claude-02")
	step(t, e, "claude-02",
		Input{Event: Start, Guard: Guard{CapacityReserved: true}},
		Input{Event: Ready, Guard: Guard{SessionReady: true, ProfileConfirmed: true}},
		e.assign(e.turns[1]))
	if busy, err := supervisor.Stop(e.project.ID, "claude-02"); !errors.Is(err, ErrTransition) || busy.State != Busy {
		t.Fatalf("a busy worker was stopped: %+v %v", busy, err)
	}
}
