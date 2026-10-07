// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/goabonga/maestro/internal/agent"
	"github.com/goabonga/maestro/internal/budget"
	"github.com/goabonga/maestro/internal/handoff"
	"github.com/goabonga/maestro/internal/provision"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/testrun"
	"github.com/goabonga/maestro/internal/turn"
	"github.com/goabonga/maestro/internal/worktree"
)

// DefaultPollInterval is the period at which the engine polls the
// detection of a running turn.
const DefaultPollInterval = 500 * time.Millisecond

// producer is the worker id of the artifacts Maestro produces itself:
// test reports and fix requests.
const producer = "maestro"

// Session is the live agent session of a started worker, as the engine
// drives it one turn at a time. The engine never writes to the PTY
// itself: the session writes the prompt, detects the end of the turn
// through the worker's versioned driver and enforces the turn's rights.
type Session interface {
	// Checkout prepares the session's worktree for a turn on a task:
	// the task branch at revision, without uncommitted changes. It
	// returns the worktree, where the turn's handoff document is read
	// and its commits are verified.
	Checkout(t task.Task, revision string) (string, error)
	// RuntimePaths lists the repository-relative paths provisioned in
	// the worktree: a commit of the task must not add or modify them.
	RuntimePaths() provision.RuntimePaths
	// Send admits a turn: it grants the turn's rights, writes the
	// prompt and starts the detection of the end of the turn.
	Send(prompt string) error
	// Poll reports the detected state of the current turn.
	Poll() agent.Detection
	// Interrupt stops the current turn.
	Interrupt() error
	// Settle ends the turn's rights: once it returns nil, the agent can
	// no longer write the task's sources.
	Settle() error
	// Close ends the session of a worker the engine failed: its agent
	// is terminated and its session slot released once its group is
	// gone.
	Close() error
}

// Sessions finds the live session of a started worker.
type Sessions interface {
	Session(projectID, name string) (Session, bool)
}

// pauses is implemented by the Sessions that pause workers, such as the
// Supervisor: Settling marks the worker as settling the end of its
// turn, no pause taking the turn until the returned function ends the
// mark; while a pause of the worker is in progress, it marks nothing and
// returns a channel closed once the pause is over.
type pauses interface {
	Settling(projectID, name string) (func(), <-chan struct{})
}

// Tester runs test commands against one exact revision of a repository
// in a fresh clone; *testrun.Runner is one.
type Tester interface {
	Run(repository, sha, clone string, commands []testrun.Command) (testrun.Run, error)
}

// Engine assigns the tasks of a project to its IDLE workers and drives
// them turn by turn through the sequential workflow: planning,
// implementation, tests, review and corrections, until each task is
// READY_TO_INTEGRATE or BLOCKED. Every turn reserves one turn of the
// task's budget before its prompt is sent, and every active step
// counts against the task's time budget. A turn that fails, times out,
// waits for input or leaves an invalid handoff after its single format
// repair blocks the task with its continuation. One Drive at a time
// advances a project, across every Engine of the process on the same
// DB.
type Engine struct {
	DB *state.DB
	// Projects imports the commits of the workers into the canonical
	// repository.
	Projects worktree.Store
	// Sessions finds the live sessions of the workers; a worker without
	// one is never assigned.
	Sessions Sessions
	// Tester runs the tests of a task; nil blocks every task that
	// reaches TESTING.
	Tester Tester
	// Now is the clock of every store; nil means time.Now.
	Now func() time.Time
	// PollInterval is the period of the polls of a running turn; 0
	// means DefaultPollInterval.
	PollInterval time.Duration
	// Wait sleeps between two polls, returning early with the context's
	// error; nil means a timer.
	Wait func(ctx context.Context, d time.Duration) error
}

// driveKey names a project of one store.
type driveKey struct {
	db      *state.DB
	project string
}

// drives holds the projects a Drive is advancing, with whether another
// Drive was called meanwhile and asks for one more pass.
var drives = struct {
	mu    sync.Mutex
	again map[driveKey]bool
}{again: map[driveKey]bool{}}

// stage is the role and the handoff kind of the worker turn that
// advances a task from one state.
type stage struct {
	role Role
	kind handoff.Kind
}

// stages lists the states a worker turn advances. TESTING is advanced
// by Maestro itself.
var stages = map[task.State]stage{
	task.New:          {Planning, handoff.Plan},
	task.Planning:     {Planning, handoff.Plan},
	task.Implementing: {Implementation, handoff.Implementation},
	task.Fixing:       {Correction, handoff.Implementation},
	task.Reviewing:    {Review, handoff.Review},
}

func (e *Engine) workers() Store        { return Store{DB: e.DB, Now: e.Now} }
func (e *Engine) tasks() task.Store     { return task.Store{DB: e.DB, Now: e.Now} }
func (e *Engine) turns() turn.Store     { return turn.Store{DB: e.DB, Now: e.Now} }
func (e *Engine) budgets() budget.Store { return budget.Store{DB: e.DB, Now: e.Now} }

// Drive advances the tasks of a project, oldest first and one at a
// time, each until it is READY_TO_INTEGRATE or BLOCKED, or until its
// next step finds no IDLE worker with a live session. It first fails
// the workers whose input wait has expired, and the BUSY workers whose
// turn no Drive follows any more. It returns once no task can progress,
// or with the context's error or the first failure of a store; the turn
// it was driving then is interrupted, its worker failed and its task
// blocked. A Drive called while another advances the project returns
// at once: the running one makes one more pass instead.
func (e *Engine) Drive(ctx context.Context, project worktree.Project) error {
	if !e.begin(project.ID) {
		return nil
	}
	for {
		if err := e.pass(ctx, project); err != nil {
			e.release(project.ID)
			return err
		}
		if !e.finish(project.ID) {
			return nil
		}
	}
}

// begin marks a project as advanced by a Drive and reports whether the
// caller is that Drive. A project another Drive advances is asked for
// one more pass instead.
func (e *Engine) begin(projectID string) bool {
	drives.mu.Lock()
	defer drives.mu.Unlock()
	key := driveKey{e.DB, projectID}
	if _, running := drives.again[key]; running {
		drives.again[key] = true
		return false
	}
	drives.again[key] = false
	return true
}

// finish ends a pass of the Drive advancing a project and reports
// whether another Drive asked for one more; otherwise the project is
// released.
func (e *Engine) finish(projectID string) bool {
	drives.mu.Lock()
	defer drives.mu.Unlock()
	key := driveKey{e.DB, projectID}
	if drives.again[key] {
		drives.again[key] = false
		return true
	}
	delete(drives.again, key)
	return false
}

// release ends the Drive advancing a project.
func (e *Engine) release(projectID string) {
	drives.mu.Lock()
	defer drives.mu.Unlock()
	delete(drives.again, driveKey{e.DB, projectID})
}

// pass recovers what no Drive follows any more, then advances every
// task of the project as far as it can go.
func (e *Engine) pass(ctx context.Context, project worktree.Project) error {
	if err := e.expireWaits(project); err != nil {
		return err
	}
	if err := e.recoverTurns(project); err != nil {
		return err
	}
	tasks, err := e.tasks().List(project.ID)
	if err != nil {
		return err
	}
	for _, listed := range tasks {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			progressed, err := e.advance(ctx, project, listed.ID)
			if err != nil {
				return err
			}
			if !progressed {
				break
			}
		}
	}
	return nil
}

// advance runs the next step of a task, if it has one that can run
// now, and reports whether it ran one.
func (e *Engine) advance(ctx context.Context, project worktree.Project, taskID string) (bool, error) {
	t, err := e.tasks().Get(taskID)
	if err != nil {
		return false, err
	}
	next, ok := stages[t.State]
	if !ok && t.State != task.Testing {
		return false, nil
	}
	if held, err := e.held(project.ID, t.ID); err != nil || held {
		return false, err
	}
	j := &job{e: e, project: project, task: t}
	if t.State == task.Testing {
		if err := j.test(); err != nil {
			return true, errors.Join(err, j.stop(err))
		}
		return true, nil
	}
	w, session, found, err := e.idle(project.ID)
	if err != nil || !found {
		return false, err
	}
	j.worker, j.session, j.role, j.kind = w, session, next.role, next.kind
	if err := j.run(ctx); err != nil {
		return true, errors.Join(err, j.stop(err))
	}
	return true, nil
}

// recoverTurns ends the turns no Drive follows any more: those of the
// BUSY workers holding an assignment, left by a Drive that returned or
// a process that stopped in the middle of a turn, since only the Drive
// calling it advances the project. Nothing would poll their detection
// or check their bounds again.
func (e *Engine) recoverTurns(project worktree.Project) error {
	workers, err := e.workers().List(project.ID)
	if err != nil {
		return err
	}
	for _, w := range workers {
		if w.State != Busy || w.Assignment == nil {
			continue
		}
		if err := e.abort(project, w, "no engine follows the turn any more"); err != nil {
			return err
		}
	}
	return nil
}

// abort ends the turn of a BUSY worker that the engine stopped
// following: the worker failed, its session closed, the turn
// interrupted, the task's open active interval charged and the task
// blocked with its continuation. It acts only if the worker is still at
// the state and version it was read at, BUSY with the same turn:
// otherwise the turn has already ended and nothing is changed.
func (e *Engine) abort(project worktree.Project, w Worker, cause string) error {
	if w.State != Busy || w.Assignment == nil {
		return nil
	}
	u, err := e.turns().Get(w.Assignment.TurnID)
	if err != nil {
		return err
	}
	reason := fmt.Sprintf("turn %s of worker %s: %s", u.ID, w.Name, cause)
	if _, err := e.workers().transitionFrom(w, Input{Event: Fail, Reason: reason}); err != nil {
		if errors.Is(err, ErrTransition) {
			return nil
		}
		return err
	}
	closed := e.closeSession(project, w.Name)
	return errors.Join(interruptTurn(e.workers(), u, w.Assignment.TaskID, reason, cause), closed)
}

// interruptTurn ends a turn no engine drives any more: the turn, unless
// it already ended, is interrupted with the cause, the task's open
// active interval charged and the task blocked with the reason and its
// continuation, or on its time budget when it ran out of time. A task
// that changed meanwhile is left as is.
func interruptTurn(s Store, u turn.Turn, taskID, reason, cause string) error {
	if !u.State.Terminal() {
		if _, err := (turn.Store{DB: s.DB, Now: s.Now}).Transition(u.ID, turn.Interrupted, cause); err != nil {
			return err
		}
	}
	budgets := budget.Store{DB: s.DB, Now: s.Now}
	_, err := budgets.Recover(taskID)
	switch {
	case errors.Is(err, budget.ErrExceeded) || errors.Is(err, budget.ErrClockRegression):
		_, err = budgets.Block(taskID, err)
	case err == nil:
		_, err = (task.Store{DB: s.DB, Now: s.Now}).Transition(taskID, task.Input{Event: task.Block, Reason: reason})
	}
	return refused(err)
}

// closeSession ends the live session of a worker the engine failed, if
// it has one: the agent is interrupted, then its group is terminated and
// its session slot released.
func (e *Engine) closeSession(project worktree.Project, name string) error {
	if e.Sessions == nil {
		return nil
	}
	session, ok := e.Sessions.Session(project.ID, name)
	if !ok {
		return nil
	}
	return endSession(session, name)
}

// endSession interrupts the agent of a failed worker, then closes its
// session.
func endSession(session Session, name string) error {
	_ = session.Interrupt()
	if err := session.Close(); err != nil {
		return fmt.Errorf("close the session of worker %s: %w", name, err)
	}
	return nil
}

// held reports whether a live worker holds an assignment on the task:
// its turn is in progress or waits for input, and nothing else may
// drive the task meanwhile.
func (e *Engine) held(projectID, taskID string) (bool, error) {
	workers, err := e.workers().List(projectID)
	if err != nil {
		return false, err
	}
	for _, w := range workers {
		if w.State.Active() && w.Assignment != nil && w.Assignment.TaskID == taskID {
			return true, nil
		}
	}
	return false, nil
}

// idle returns the first IDLE worker of the project, by name, that has
// a live session.
func (e *Engine) idle(projectID string) (Worker, Session, bool, error) {
	if e.Sessions == nil {
		return Worker{}, nil, false, nil
	}
	workers, err := e.workers().List(projectID)
	if err != nil {
		return Worker{}, nil, false, err
	}
	for _, w := range workers {
		if w.State != Idle {
			continue
		}
		if session, ok := e.Sessions.Session(projectID, w.Name); ok {
			return w, session, true, nil
		}
	}
	return Worker{}, nil, false, nil
}

// expireWaits fails the workers of a project whose turn waited for
// input past its bound: the turn fails with the expired bound, the
// worker fails and its session is closed. Their task is already
// blocked with its continuation.
func (e *Engine) expireWaits(project worktree.Project) error {
	workers, err := e.workers().List(project.ID)
	if err != nil {
		return err
	}
	var closed []error
	for _, w := range workers {
		if w.State != WaitingInput || w.Assignment == nil {
			continue
		}
		expired, found, err := e.turns().Expire(w.Assignment.TurnID)
		if err != nil {
			return errors.Join(append(closed, err)...)
		}
		if !found {
			continue
		}
		reason := fmt.Sprintf("turn %s: %s", expired.ID, expired.Reason)
		if _, err := e.workers().Transition(project.ID, w.Name, Input{Event: Fail, Reason: reason}); err != nil {
			if errors.Is(err, ErrTransition) {
				continue
			}
			return errors.Join(append(closed, err)...)
		}
		if err := e.closeSession(project, w.Name); err != nil {
			closed = append(closed, err)
		}
	}
	return errors.Join(closed...)
}

// wait sleeps one poll interval.
func (e *Engine) wait(ctx context.Context) error {
	interval := e.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	if e.Wait != nil {
		return e.Wait(ctx, interval)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// revision is the commit a turn on the task starts from: the revision
// under test and review once there is one, the task base before.
func revision(t task.Task) string {
	if t.HeadSHA != "" {
		return t.HeadSHA
	}
	return t.BaseSHA
}

// diffDigest is the SHA-256 of the binary diff between two commits of
// a repository: the diff a review is about.
func diffDigest(repository, base, head string) (string, error) {
	diff, err := runGit(repository, "diff", "--binary", "--no-ext-diff", "--no-textconv", base, head, "--")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(diff)
	return hex.EncodeToString(sum[:]), nil
}

// runGit runs one git command in dir and returns its standard output.
// None of the process's GIT_* variables, no system or global
// configuration and no hook apply: an inherited GIT_DIR would aim the
// command at another repository.
func runGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=" + os.DevNull}, args...)...) // #nosec G204 -- fixed verbs; paths and revisions come from Maestro
	cmd.Dir = dir
	var env []string
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); !strings.HasPrefix(name, "GIT_") {
			env = append(env, entry)
		}
	}
	cmd.Env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

// newRunID returns a random UUID version 4.
func newRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
