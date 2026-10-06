// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/goabonga/maestro/internal/agent"
	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/provision"
	"github.com/goabonga/maestro/internal/scheduler"
	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/worktree"
)

// Defaults of the supervisor.
const (
	// DefaultStartTimeout bounds the wait for an agent to confirm its
	// native session.
	DefaultStartTimeout = time.Minute
	// DefaultStopGrace is the time a stopped agent group gets between
	// SIGTERM and SIGKILL.
	DefaultStopGrace = 2 * time.Second
	// DefaultCloseTimeout bounds the wait of Close for the groups it could
	// not terminate.
	DefaultCloseTimeout = 10 * time.Second
)

// sessionPath is the environment PATH of an agent session; the agent's
// own binary is mounted read-only and called by its absolute path.
const sessionPath = "/usr/local/bin:/usr/bin:/bin"

// Errors of the supervisor.
var (
	// ErrAgent reports an agent Maestro cannot start: unknown, without a
	// validated driver, or not installed.
	ErrAgent = errors.New("agent not available")
	// ErrClosed reports a supervisor that is shutting down.
	ErrClosed = errors.New("the supervisor is shutting down")
)

// Supervisor starts workers in confined agent sessions through their
// validated driver, and stops them. Every started worker holds one
// session slot of the global capacity until it stops or fails.
type Supervisor struct {
	Store    Store
	Projects worktree.Store
	// Capacity bounds the concurrent sessions; it is required.
	Capacity *scheduler.Capacity
	// Launcher confines the sessions; nil refuses every start.
	Launcher *launcher.Launcher
	// Registry selects the drivers; nil means agent.Builtin.
	Registry *agent.Registry
	// LookPath finds an agent binary; nil means exec.LookPath.
	LookPath func(string) (string, error)
	// Getenv reads the daemon's environment for an agent's api_key_env;
	// nil means os.Getenv.
	Getenv func(string) string
	// StartTimeout bounds the native session confirmation; 0 means
	// DefaultStartTimeout.
	StartTimeout time.Duration
	// StopGrace is the SIGTERM grace of a stop; 0 means
	// DefaultStopGrace.
	StopGrace time.Duration
	// CloseTimeout bounds the wait of Close for the groups it could not
	// terminate; 0 means DefaultCloseTimeout.
	CloseTimeout time.Duration

	// tearDown replaces teardown in tests; nil means teardown.
	tearDown func(*running) error

	once sync.Once
	mu   sync.Mutex
	// live holds the started sessions a watcher owns: one that ends on
	// its own fails its worker.
	live map[liveKey]*running
	// stopping holds the workers a Stop is in progress for.
	stopping map[liveKey]bool
	// ending holds the sessions taken over for termination whose group
	// is not yet confirmed gone: they keep their slot and their worker's
	// name is not reused until then.
	ending map[liveKey]*running
	// failing holds the workers whose session a watcher or Close tears
	// down without a Stop, until their failure is recorded: no Stop takes
	// them meanwhile and their name is not reused.
	failing  map[liveKey]bool
	closed   bool
	done     chan struct{}
	launches sync.WaitGroup
	watchers sync.WaitGroup
}

// liveKey identifies a worker of a project.
type liveKey struct{ project, name string }

// running is the live session of a started worker. Its fields but the
// immutable ones are guarded by the supervisor's mutex.
type running struct {
	epoch  *session.Epoch
	native *agent.NativeSession
	slot   *scheduler.Slot
	// stopping marks a session taken over for termination: its watcher
	// then only releases its slot once the group is gone.
	stopping bool
	// ended is set by the watcher once the group is gone.
	ended bool
}

// StartRequest asks for workers of one agent of a project.
type StartRequest struct {
	Project worktree.Project
	// Agent is the agent's name: a configured agent whose driver names
	// its kind, or the kind itself.
	Agent string
	// Count is the number of workers to start, at least 1.
	Count int
	// ConfigID and Snapshot are the project's configuration snapshot
	// the workers are provisioned from.
	ConfigID string
	Snapshot config.Snapshot
}

// launchPlan is everything one background start needs.
type launchPlan struct {
	project   worktree.Project
	worker    Worker
	slot      *scheduler.Slot
	installed agent.Installation
	configID  string
	snapshot  config.Snapshot
}

// init prepares the private state once.
func (s *Supervisor) init() {
	s.once.Do(func() {
		s.live = map[liveKey]*running{}
		s.stopping = map[liveKey]bool{}
		s.ending = map[liveKey]*running{}
		s.failing = map[liveKey]bool{}
		s.done = make(chan struct{})
	})
}

// Start resolves the agent's installed version and validated driver,
// reserves one session slot per worker — all of them or none — and
// moves each worker from STOPPED to STARTING: STOPPED workers of the
// same agent and driver are reused by name, then new workers named
// <agent>-01, <agent>-02… are registered. It returns the STARTING
// workers; each one is then provisioned and launched in the background,
// and reaches IDLE once its native session is confirmed, or FAILED with
// its slot released. A worker still being stopped, or whose group is not
// confirmed gone, is never reused. A refused start registers nothing: the
// workers it registered are removed and the reused ones are STOPPED again.
func (s *Supervisor) Start(ctx context.Context, request StartRequest) ([]Worker, error) {
	s.init()
	if request.Count < 1 {
		return nil, fmt.Errorf("%w: the count must be at least 1", ErrInvalid)
	}
	if s.Launcher == nil {
		return nil, fmt.Errorf("%w: no worker can start", launcher.ErrUnsupported)
	}
	if s.Capacity == nil {
		return nil, errors.New("the supervisor has no capacity")
	}
	kind := request.Agent
	if configured, ok := request.Snapshot.Config.Agents[request.Agent]; ok && configured.Driver != "" {
		kind = configured.Driver
	}
	registry := s.Registry
	if registry == nil {
		registry = agent.Builtin()
	}
	installed, err := registry.Installed(ctx, kind, s.LookPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrAgent, request.Agent, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	var slots []*scheduler.Slot
	releaseFrom := func(i int) {
		for _, slot := range slots[i:] {
			slot.Release()
		}
	}
	for range request.Count {
		slot, err := s.Capacity.Reserve(scheduler.Sessions, request.Project.ID)
		if err != nil {
			releaseFrom(0)
			return nil, err
		}
		slots = append(slots, slot)
	}
	selected, registered, err := s.selectWorkers(request, installed)
	if err != nil {
		releaseFrom(0)
		s.abandonStart(request.Project.ID, nil, registered, err)
		return nil, err
	}
	started := make([]Worker, 0, len(selected))
	for _, w := range selected {
		next, err := s.Store.Transition(request.Project.ID, w.Name, Input{
			Event: Start, Reason: "start asked", Guard: Guard{CapacityReserved: true},
		})
		if err != nil {
			releaseFrom(0)
			s.abandonStart(request.Project.ID, started, registered, err)
			return nil, err
		}
		started = append(started, next)
	}
	for i, w := range started {
		plan := launchPlan{
			project: request.Project, worker: w, slot: slots[i], installed: installed,
			configID: request.ConfigID, snapshot: request.Snapshot,
		}
		s.launches.Add(1)
		go s.launch(plan)
	}
	return started, nil
}

// abandonStart undoes a refused start: the workers it registered are
// removed, and the reused workers it already moved to STARTING are
// STOPPED again. The caller holds s.mu and has released the slots.
func (s *Supervisor) abandonStart(projectID string, started []Worker, registered map[string]bool, cause error) {
	for _, w := range started {
		if !registered[w.Name] {
			_, _ = s.Store.Transition(projectID, w.Name, Input{
				Event: Stop, Reason: "start abandoned: " + cause.Error(), Guard: Guard{Reconciled: true},
			})
		}
	}
	for name := range registered {
		_ = s.Store.unregister(projectID, name)
	}
}

// selectWorkers picks the workers to start: the STOPPED workers of the
// same agent, kind and driver by name, then new registrations under the
// first free names <agent>-NN, returned by name too. A worker a Stop is
// in progress for, whose group is not confirmed gone, or whose failure is
// being recorded, is skipped. The caller holds s.mu.
func (s *Supervisor) selectWorkers(request StartRequest, installed agent.Installation) ([]Worker, map[string]bool, error) {
	existing, err := s.Store.List(request.Project.ID)
	if err != nil {
		return nil, nil, err
	}
	taken := map[string]bool{}
	registered := map[string]bool{}
	var selected []Worker
	for _, w := range existing {
		taken[w.Name] = true
		key := liveKey{request.Project.ID, w.Name}
		if s.stopping[key] || s.ending[key] != nil || s.failing[key] {
			continue
		}
		if len(selected) < request.Count && w.State == Stopped && w.Agent == request.Agent &&
			w.AgentKind == installed.Driver.Kind && w.Driver == installed.Driver.Name {
			selected = append(selected, w)
		}
	}
	for n := 1; len(selected) < request.Count; n++ {
		name := fmt.Sprintf("%s-%02d", request.Agent, n)
		if taken[name] {
			continue
		}
		w, err := s.Store.Register(request.Project, Spec{
			Name: name, Agent: request.Agent, AgentKind: installed.Driver.Kind, Driver: installed.Driver.Name,
		})
		if err != nil {
			return nil, registered, err
		}
		registered[name] = true
		selected = append(selected, w)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
	return selected, registered, nil
}

// launch provisions and starts one STARTING worker, then moves it to
// IDLE; any failure moves it to FAILED and releases its slot once its
// group, if one started, is gone.
func (s *Supervisor) launch(plan launchPlan) {
	defer s.launches.Done()
	key := liveKey{plan.project.ID, plan.worker.Name}
	live, err := s.boot(key, plan)
	if err != nil {
		if live == nil {
			plan.slot.Release()
			s.fail(key.project, key.name, "start failed: "+err.Error())
			return
		}
		s.abandon(key, live, "start failed: "+err.Error())
		return
	}
	s.mu.Lock()
	switch {
	case s.closed:
		err = ErrClosed
	case live.ended:
		err = errors.New("the agent session ended before the worker was ready")
	default:
		s.live[key] = live
	}
	s.mu.Unlock()
	if err == nil {
		id := live.native.Identity().ID
		_, err = s.Store.Transition(key.project, key.name, Input{
			Event: Ready, Reason: "native session " + id + " confirmed",
			Guard: Guard{SessionReady: true, ProfileConfirmed: true},
		})
		if err != nil {
			s.mu.Lock()
			if s.live[key] == live {
				delete(s.live, key)
			}
			s.mu.Unlock()
		}
	}
	if err != nil {
		s.abandon(key, live, "start failed: "+err.Error())
	}
}

// abandon terminates a session no Stop owns and moves its worker to
// FAILED with the reason. Its slot is released once the group is gone.
func (s *Supervisor) abandon(key liveKey, live *running, reason string) {
	s.mu.Lock()
	live.stopping = true
	if !live.ended {
		s.ending[key] = live
	}
	s.mu.Unlock()
	if err := s.terminate(key, live); err != nil {
		reason += "; " + err.Error()
	}
	s.fail(key.project, key.name, reason)
}

// boot provisions the worker's private repository, worktree, native
// files and private HOME, starts the agent in a confined PTY through its
// driver, watches its session and waits for its native session to be
// confirmed. A failure once the session started returns the session with
// the error, for its termination.
func (s *Supervisor) boot(key liveKey, plan launchPlan) (*running, error) {
	kind := plan.installed.Driver.Kind
	base := filepath.Join(plan.project.Dir, "workers", plan.worker.Name)
	home := filepath.Join(base, "home")
	state := filepath.Join(base, "supervisor")
	// A start opens a fresh native session: the private HOME and the
	// supervisor record of a previous run are discarded.
	for _, dir := range []string{home, state} {
		if err := os.RemoveAll(dir); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	repository, _, err := s.Projects.AddWorker(plan.project, plan.worker.Name)
	if err != nil {
		return nil, fmt.Errorf("private repository: %w", err)
	}
	workTree, head, err := s.Projects.CheckoutWorker(plan.project, repository)
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	if _, err := provision.Instructions(workTree, kind, "", plan.snapshot.Instructions); err != nil {
		return nil, fmt.Errorf("instruction files: %w", err)
	}
	if err := writeMCP(home, plan.snapshot.Config.MCP, plan.worker.Agent, kind); err != nil {
		return nil, fmt.Errorf("MCP configuration: %w", err)
	}
	binary, err := filepath.EvalSymlinks(plan.installed.Path)
	if err != nil {
		return nil, err
	}
	options := agent.NativeOptions{
		WorkerID: plan.project.ID + "/" + plan.worker.Name, ConfigID: plan.configID,
		WorkDir: workTree, Home: home, StateDir: state, Version: plan.installed.Version, Binary: binary,
	}
	spec := launcher.Spec{
		Dir: workTree, Env: s.environment(plan.snapshot.Config.Agents[plan.worker.Agent]),
		ReadOnly: []string{binary},
		// The agent reaches its provider's API.
		Network: true,
	}
	var native *agent.NativeSession
	var confirm func() error
	switch kind {
	case provision.KindClaudeCode:
		if native, err = agent.OpenClaude(options); err == nil {
			confirm = native.ConfirmClaude
			spec, err = native.ClaudeStart(spec)
		}
	case provision.KindCodex:
		if native, err = agent.OpenCodex(options); err == nil {
			confirm = native.ConfirmCodex
			spec, err = native.CodexStart(spec)
		}
	default:
		err = fmt.Errorf("%w: no native session driver for %s", ErrAgent, kind)
	}
	if err != nil {
		if native != nil {
			_ = native.Close()
		}
		return nil, err
	}
	profile, err := session.NewProfile(session.ProfileSpec{
		Epoch: 1, Role: session.Coding, Revision: head, Config: session.Config{Spec: spec}, GitDir: repository.Dir,
	})
	if err != nil {
		_ = native.Close()
		return nil, err
	}
	epoch, err := session.StartEpoch(s.Launcher, profile)
	if err != nil {
		_ = native.Close()
		return nil, err
	}
	live := &running{epoch: epoch, native: native, slot: plan.slot}
	s.watchers.Add(1)
	go s.watch(key, live)
	if err := s.confirm(epoch, confirm); err != nil {
		return live, err
	}
	if err := epoch.ConfirmNativeID(native.Identity().ID); err != nil {
		return live, err
	}
	return live, nil
}

// confirm polls the driver's confirmation of the native session until
// it succeeds, the agent exits, the start times out or the supervisor
// closes.
func (s *Supervisor) confirm(epoch *session.Epoch, confirm func() error) error {
	timeout := s.StartTimeout
	if timeout <= 0 {
		timeout = DefaultStartTimeout
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		err := confirm()
		if err == nil {
			return nil
		}
		if state := epoch.Session().State(); state.Phase != session.Running {
			return fmt.Errorf("the agent exited with code %d before its native session was confirmed: %w", state.ExitCode, err)
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("the native session was not confirmed within %s: %w", timeout, err)
		case <-s.done:
			return ErrClosed
		case <-tick.C:
		}
	}
}

// environment is the allow-listed environment of an agent session: the
// configured api_key_env is passed from the daemon's environment.
func (s *Supervisor) environment(configured config.Agent) map[string]string {
	env := map[string]string{"PATH": sessionPath, "TERM": "xterm-256color", "LANG": "C.UTF-8"}
	if configured.APIKeyEnv != "" {
		getenv := s.Getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		if value := getenv(configured.APIKeyEnv); value != "" {
			env[configured.APIKeyEnv] = value
		}
	}
	return env
}

// writeMCP writes the MCP servers that apply to the agent in its native
// user configuration, inside the private HOME: Claude Code's
// .claude/.claude.json, Codex's .codex/config.toml.
func writeMCP(home string, servers map[string]config.MCP, agentName, kind string) error {
	translated, err := provision.TranslateMCP(servers, agentName, kind, "")
	if err != nil || len(translated.Servers) == 0 {
		return err
	}
	path := filepath.Join(home, ".claude", ".claude.json")
	if kind == provision.KindCodex {
		path = filepath.Join(home, ".codex", "config.toml")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, translated.Content, 0o600)
}

// watch waits for a session's group to end, then releases its slot: the
// group is gone, so the slot no longer counts. A started worker whose
// session ends on its own is moved to FAILED; until that failure is
// recorded, the worker is marked failing, so that no Stop moves it to
// STOPPED and no Start reuses its name meanwhile.
func (s *Supervisor) watch(key liveKey, live *running) {
	defer s.watchers.Done()
	live.epoch.Session().Wait()
	s.mu.Lock()
	live.ended = true
	if s.ending[key] == live {
		delete(s.ending, key)
	}
	owned := s.live[key] == live && !live.stopping
	if owned {
		delete(s.live, key)
		s.failing[key] = true
	}
	s.mu.Unlock()
	if !owned {
		live.slot.Release()
		return
	}
	code := live.epoch.Session().State().ExitCode
	_ = s.end(live)
	live.slot.Release()
	s.fail(key.project, key.name, fmt.Sprintf("the agent session exited with code %d", code))
	s.recorded(key)
}

// recorded clears the failing mark of a worker once its failure is
// recorded.
func (s *Supervisor) recorded(key liveKey) {
	s.mu.Lock()
	delete(s.failing, key)
	s.mu.Unlock()
}

// terminate tears a session taken over for termination down. Once its
// group is confirmed gone, the slot is released and the worker's name is
// free again; when the teardown fails, the group may still be alive, so
// the slot stays held and the name reserved until the watcher sees the
// group end.
func (s *Supervisor) terminate(key liveKey, live *running) error {
	if err := s.end(live); err != nil {
		return err
	}
	s.mu.Lock()
	if s.ending[key] == live {
		delete(s.ending, key)
	}
	s.mu.Unlock()
	live.slot.Release()
	return nil
}

// end runs the teardown of a session.
func (s *Supervisor) end(live *running) error {
	if s.tearDown != nil {
		return s.tearDown(live)
	}
	return s.teardown(live)
}

// teardown terminates a live session's confined group and releases its
// native session ownership.
func (s *Supervisor) teardown(live *running) error {
	end := live.epoch.End("stop", s.grace(), func(session.Boundary) error { return nil })
	return errors.Join(end, live.native.Close())
}

// grace returns the SIGTERM grace of a stop.
func (s *Supervisor) grace() time.Duration {
	if s.StopGrace > 0 {
		return s.StopGrace
	}
	return DefaultStopGrace
}

// fail moves a worker to FAILED with the reason.
func (s *Supervisor) fail(projectID, name, reason string) {
	_, _ = s.Store.Transition(projectID, name, Input{Event: Fail, Reason: reason})
}

// Stop stops a worker. A live worker is drained — DRAINING, so it takes
// no new assignment — then its confined group is terminated, its slot
// released, and it reaches STOPPED. A FAILED worker is moved to STOPPED
// once its group is gone, releasing the assignment it kept: the
// termination of a group a previous teardown failed on is retried first.
// A worker that is starting, already stopped, live and holding an
// assignment, already being stopped by another Stop
// or whose session is being torn down without a Stop is refused with
// ErrTransition. A group that cannot be terminated
// leaves the worker FAILED and keeps its slot until the group is gone.
func (s *Supervisor) Stop(projectID, name string) (Worker, error) {
	s.init()
	key := liveKey{projectID, name}
	s.mu.Lock()
	busy, failing := s.stopping[key], s.failing[key]
	if !busy && !failing {
		s.stopping[key] = true
	}
	s.mu.Unlock()
	if busy || failing {
		current, err := s.Store.Get(projectID, name)
		if err != nil {
			return Worker{}, err
		}
		if failing {
			return current, fmt.Errorf("%w: %s is being torn down", ErrTransition, name)
		}
		return current, fmt.Errorf("%w: %s is already being stopped", ErrTransition, name)
	}
	defer func() {
		s.mu.Lock()
		delete(s.stopping, key)
		s.mu.Unlock()
	}()
	current, err := s.Store.Get(projectID, name)
	if err != nil {
		return Worker{}, err
	}
	switch {
	case current.State == Stopped:
		return current, fmt.Errorf("%w: %s is already stopped", ErrTransition, name)
	case current.State == Starting:
		return current, fmt.Errorf("%w: %s is still starting", ErrTransition, name)
	case current.State == Failed:
		s.mu.Lock()
		pending := s.ending[key]
		s.mu.Unlock()
		if pending != nil {
			if err := s.terminate(key, pending); err != nil {
				return current, fmt.Errorf("stop failed: the group of %s may still be running: %w", name, err)
			}
		}
		return s.Store.Transition(projectID, name, Input{
			Event: Stop, Reason: "stopped by the user", Guard: Guard{Reconciled: true},
		})
	case current.Assignment != nil:
		return current, fmt.Errorf("%w: %s holds an assignment", ErrTransition, name)
	}
	if current.State != Draining {
		draining, err := s.Store.Transition(projectID, name, Input{Event: ScaleDown, Reason: "stop asked"})
		if err != nil {
			return current, err
		}
		current = draining
	}
	s.mu.Lock()
	live := s.live[key]
	ending := s.ending[key] != nil || s.failing[key]
	if live != nil {
		live.stopping = true
		delete(s.live, key)
		s.ending[key] = live
	}
	s.mu.Unlock()
	if live == nil && ending {
		return current, fmt.Errorf("%w: %s is being torn down", ErrTransition, name)
	}
	// A worker without a live session here lost it with a previous
	// daemon: its group died with that daemon.
	if live != nil {
		if err := s.terminate(key, live); err != nil {
			failed, failErr := s.Store.Transition(projectID, name, Input{Event: Fail, Reason: "stop failed: " + err.Error()})
			if failErr != nil {
				return current, errors.Join(err, failErr)
			}
			return failed, err
		}
	}
	return s.Store.Transition(projectID, name, Input{
		Event: Drained, Reason: "stopped by the user",
		Guard: Guard{TurnSettled: true, ClientDetached: true, DescendantsStopped: true},
	})
}

// Close refuses new starts, abandons the starts in progress and stops
// every live worker of this supervisor. A live session that Stop leaves
// behind is torn down anyway, and its worker moved to FAILED; the
// termination of every group a previous teardown failed on is retried.
// Close then waits for the groups it watches to be gone, at most
// CloseTimeout when a group could not be terminated.
func (s *Supervisor) Close() {
	s.init()
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	s.mu.Unlock()
	s.launches.Wait()
	s.mu.Lock()
	keys := make([]liveKey, 0, len(s.live))
	for key := range s.live {
		keys = append(keys, key)
	}
	s.mu.Unlock()
	for _, key := range keys {
		_, _ = s.Stop(key.project, key.name)
	}
	s.mu.Lock()
	remaining := map[liveKey]*running{}
	for key, live := range s.live {
		live.stopping = true
		delete(s.live, key)
		s.ending[key] = live
		s.failing[key] = true
		remaining[key] = live
	}
	s.mu.Unlock()
	for key, live := range remaining {
		reason := "the supervisor closed before the worker stopped"
		if err := s.terminate(key, live); err != nil {
			reason += ": " + err.Error()
		}
		s.fail(key.project, key.name, reason)
		s.recorded(key)
	}
	s.mu.Lock()
	pending := map[liveKey]*running{}
	for key, live := range s.ending {
		pending[key] = live
	}
	s.mu.Unlock()
	stuck := false
	for key, live := range pending {
		if err := s.terminate(key, live); err != nil {
			stuck = true
		}
	}
	gone := make(chan struct{})
	go func() {
		s.watchers.Wait()
		close(gone)
	}()
	if !stuck {
		<-gone
		return
	}
	timeout := s.CloseTimeout
	if timeout <= 0 {
		timeout = DefaultCloseTimeout
	}
	select {
	case <-gone:
	case <-time.After(timeout):
	}
}
