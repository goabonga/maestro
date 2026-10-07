// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goabonga/maestro/internal/agent"
	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/provision"
	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/task"
)

// Bounds of the live sessions.
const (
	// detectorBound is the turn and input-wait bound of the driver's
	// detection: the engine enforces the task's own bounds, so the
	// detector never ends a turn on its own clock first.
	detectorBound = 24 * time.Hour
	// settleQuiet is how long a resumed agent's terminal must stay quiet,
	// once it has drawn something, before it is ready for a prompt.
	settleQuiet = 300 * time.Millisecond
	// outputQueue bounds the output chunks waiting for the detector.
	outputQueue = 1024
	// interruptKey is the key that interrupts the turn of Claude Code and
	// Codex: Esc.
	interruptKey = "\x1b"
)

// A supervisor finds the live sessions the engine drives.
var _ Sessions = (*Supervisor)(nil)

// Session returns the live session of a started worker of a project, as
// the engine drives it: none for a worker that is not IDLE-capable —
// starting, stopping, failed or stopped — in this supervisor.
func (s *Supervisor) Session(projectID, name string) (Session, bool) {
	s.init()
	key := liveKey{projectID, name}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.live[key]
	if live == nil || live.stopping || live.ended || s.closed {
		return nil, false
	}
	return &liveSession{s: s, key: key, live: live}, true
}

// liveSession drives the agent session of one started worker turn by
// turn. A turn runs with write access to the worker's worktree and its
// private repository; before its first turn and between two turns, the
// agent runs with both read only. Each change of rights stops the whole
// confined group and resumes the exact native conversation in a new PTY
// under the new profile.
type liveSession struct {
	s    *Supervisor
	key  liveKey
	live *running
}

// Checkout fetches the canonical repository's branches and Maestro
// references into the worker's private repository, then checks the
// task branch out at revision in the worker's worktree, discarding its
// uncommitted changes and untracked files; the ignored ones, such as the
// provisioned instruction files, are kept.
func (l *liveSession) Checkout(t task.Task, revision string) (string, error) {
	dir := l.live.workTree
	if _, err := runGit(dir, "fetch", "--quiet", l.live.project.Repository(),
		"+refs/heads/*:refs/canonical/heads/*", "+refs/maestro/*:refs/canonical/maestro/*"); err != nil {
		return "", err
	}
	if _, err := runGit(dir, "checkout", "--quiet", "--force", "-B", t.Branch, revision); err != nil {
		return "", err
	}
	if _, err := runGit(dir, "clean", "--quiet", "-ffd"); err != nil {
		return "", err
	}
	return dir, nil
}

// RuntimePaths lists the instruction files provisioned in the worktree.
func (l *liveSession) RuntimePaths() provision.RuntimePaths {
	return append(provision.RuntimePaths(nil), l.live.runtime...)
}

// Send grants the turn's rights, resuming the agent with write access
// when it runs read only, then writes the prompt to its terminal and
// starts the detection of the end of the turn.
func (l *liveSession) Send(prompt string) error {
	input, err := agent.PromptInput(prompt)
	if err != nil {
		return err
	}
	l.live.turn.Lock()
	defer l.live.turn.Unlock()
	if l.live.role != session.Coding {
		if err := l.s.replace(l.key, l.live, session.Coding); err != nil {
			return fmt.Errorf("grant the turn's rights: %w", err)
		}
	}
	l.live.detector.Begin(time.Now())
	_, err = l.live.epoch.Session().Write(input)
	return err
}

// Poll reports the driver's detection of the current turn.
func (l *liveSession) Poll() agent.Detection {
	return l.live.detector.Poll(time.Now(), l.live.epoch.Session().State())
}

// Interrupt records the interruption and sends the agent its interrupt
// key.
func (l *liveSession) Interrupt() error {
	l.live.detector.Interrupt()
	_, err := l.live.epoch.Session().Write([]byte(interruptKey))
	return err
}

// Settle revokes the turn's rights: the whole confined group is stopped,
// then the agent is resumed with its worktree and private repository
// read only. Once it returns nil, the agent cannot write the task's
// sources any more.
func (l *liveSession) Settle() error {
	l.live.turn.Lock()
	defer l.live.turn.Unlock()
	if l.live.role == session.Review {
		return nil
	}
	return l.s.replace(l.key, l.live, session.Review)
}

// Close ends the session of a worker the engine failed: the session is
// taken over for termination, its confined group, whatever profile it
// runs under, is terminated and its slot released once the group is
// confirmed gone. A teardown that fails keeps the slot until the group
// ends, and a Stop of the failed worker retries it.
func (l *liveSession) Close() error {
	s := l.s
	s.mu.Lock()
	if s.live[l.key] != l.live {
		s.mu.Unlock()
		return nil
	}
	l.live.stopping = true
	delete(s.live, l.key)
	if !l.live.ended {
		s.ending[l.key] = l.live
	}
	s.mu.Unlock()
	return s.terminate(l.key, l.live)
}

// replace moves a live session to a new permissions profile: its group
// is stopped and confirmed gone, then the exact native conversation is
// resumed in a new confined PTY with the role's rights, and the agent's
// terminal is given time to settle. The caller holds live.turn. A session
// taken over for termination or paused is never resumed here.
func (s *Supervisor) replace(key liveKey, live *running, role session.Role) error {
	s.mu.Lock()
	if live.paused {
		s.mu.Unlock()
		return fmt.Errorf("the session of %s is paused", key.name)
	}
	if live.stopping || live.ended || s.closed {
		s.mu.Unlock()
		return fmt.Errorf("the session of %s is ending", key.name)
	}
	replaced := make(chan struct{})
	live.replacing = replaced
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		live.replacing = nil
		s.mu.Unlock()
		close(replaced)
	}()
	profile, resume, err := live.profile(role, s.environment(live.agentConfig))
	if err != nil {
		return err
	}
	if err := live.epoch.End("permissions change to "+string(role), s.grace(),
		func(session.Boundary) error { return nil }); err != nil {
		return err
	}
	live.role = ""
	if err := live.epoch.Restart(profile, resume); err != nil {
		return err
	}
	live.role = role
	current := live.epoch.Session()
	s.follow(live, current)
	return s.ready(current)
}

// profile builds the next permissions profile of a live session, with
// the exact resume command of its native conversation.
func (live *running) profile(role session.Role, env map[string]string) (session.Profile, session.Resume, error) {
	base := launcher.Spec{Dir: live.workTree, Env: env, ReadOnly: []string{live.binary}, Network: true}
	var spec launcher.Spec
	var resume session.Resume
	var err error
	switch live.kind {
	case provision.KindClaudeCode:
		spec, err = live.native.ClaudeResumeSpec(base)
		resume = live.native.ClaudeResume
	case provision.KindCodex:
		spec, err = live.native.CodexResumeSpec(base)
		resume = live.native.CodexResume
	default:
		err = fmt.Errorf("%w: no native session driver for %s", ErrAgent, live.kind)
	}
	if err != nil {
		return session.Profile{}, nil, err
	}
	revision, err := runGit(live.workTree, "rev-parse", "HEAD")
	if err != nil {
		return session.Profile{}, nil, err
	}
	profile, err := session.NewProfile(session.ProfileSpec{
		Epoch: live.epoch.State().Epoch + 1, Role: role, Revision: strings.TrimSpace(string(revision)),
		Config: session.Config{Spec: spec}, GitDir: live.repository.Dir,
	})
	return profile, resume, err
}

// follow feeds the output of a session of a live worker to its
// detector until the session's output ends.
func (s *Supervisor) follow(live *running, current *session.Session) {
	output, _ := current.Subscribe(outputQueue)
	go func() {
		for chunk := range output {
			live.detector.Feed(chunk, time.Now())
		}
	}()
}

// ready waits for a resumed agent to draw its terminal and then stay
// quiet for settleQuiet, at most the start timeout.
func (s *Supervisor) ready(current *session.Session) error {
	timeout := s.StartTimeout
	if timeout <= 0 {
		timeout = DefaultStartTimeout
	}
	output, cancel := current.Subscribe(outputQueue)
	defer cancel()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	quiet := time.NewTimer(timeout)
	defer quiet.Stop()
	drawn := false
	for {
		select {
		case _, open := <-output:
			if !open {
				state := current.State()
				if state.Phase != session.Running {
					return fmt.Errorf("the resumed agent exited with code %d", state.ExitCode)
				}
				return errors.New("the resumed agent's output was lost")
			}
			drawn = true
			quiet.Reset(settleQuiet)
		case <-quiet.C:
			if drawn {
				return nil
			}
		case <-deadline.C:
			return fmt.Errorf("the resumed agent did not settle within %s", timeout)
		case <-s.done:
			return ErrClosed
		}
	}
}
