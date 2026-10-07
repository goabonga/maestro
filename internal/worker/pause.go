// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"errors"
	"fmt"
	"sync"

	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/turn"
)

// The reasons recorded by a pause.
const (
	// reasonPaused explains the pause of a worker.
	reasonPaused = "paused by the user"
	// causePaused ends the turn a paused worker held.
	causePaused = "the worker was paused"
)

// pausable reports whether the table lets a worker in the state pause.
func pausable(s State) bool {
	return Accepts(s, Pause)
}

// Settling marks the worker as settling the end of its turn, for the
// engine: no Pause takes the turn until the returned function ends the
// mark, a Pause asked meanwhile waiting for it. While a Pause of the
// worker is in progress, nothing is marked and a channel closed once it
// is over is returned instead: the pause owns the turn meanwhile.
func (s *Supervisor) Settling(projectID, name string) (func(), <-chan struct{}) {
	s.init()
	key := liveKey{projectID, name}
	s.mu.Lock()
	defer s.mu.Unlock()
	if pausing := s.pausing[key]; pausing != nil {
		return nil, pausing
	}
	done := make(chan struct{})
	s.settling[key] = done
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.settling[key] == done {
				delete(s.settling, key)
			}
			s.mu.Unlock()
			close(done)
		})
	}, nil
}

// Pause pauses an IDLE, BUSY or WAITING_INPUT worker with a live session
// in this supervisor. The engine finds no session for it any more, so it
// takes no new assignment; its agent is sent its interrupt key, then its
// whole confined group, every descendant included, is stopped and
// confirmed gone. Its native session and its slot are kept for its
// resume. The turn it held, if any, is interrupted, its task's open
// active interval charged and the task blocked with its continuation,
// before the worker moves to PAUSED and releases its assignment. A
// worker whose engine job is settling the end of its turn is paused
// once the job has settled it. A worker in another state, without a
// live session, being stopped, torn down or already paused is refused
// with ErrTransition. A group that cannot be stopped, or a worker that
// changed state meanwhile, fails the worker and terminates its session.
func (s *Supervisor) Pause(projectID, name string) (Worker, error) {
	s.init()
	key := liveKey{projectID, name}
	var live *running
	for {
		current, err := s.Store.Get(projectID, name)
		if err != nil {
			return Worker{}, err
		}
		s.mu.Lock()
		live = s.live[key]
		switch {
		case s.closed:
			err = ErrClosed
		case s.pausing[key] != nil:
			err = fmt.Errorf("%w: %s is already being paused", ErrTransition, name)
		case s.stopping[key] || s.failing[key] || s.ending[key] != nil:
			err = fmt.Errorf("%w: %s is being stopped", ErrTransition, name)
		case !pausable(current.State):
			err = fmt.Errorf("%w: %s is %s", ErrTransition, name, current.State)
		case live == nil || live.stopping || live.ended:
			err = fmt.Errorf("%w: %s has no live session in this daemon", ErrTransition, name)
		}
		if err != nil {
			s.mu.Unlock()
			return current, err
		}
		settling := s.settling[key]
		if settling == nil {
			break
		}
		// The engine settles the end of the worker's turn: the pause
		// starts once it is settled, from the worker it left.
		s.mu.Unlock()
		<-settling
	}
	done := make(chan struct{})
	s.pausing[key] = done
	delete(s.live, key)
	live.paused = true
	s.launches.Add(1)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pausing, key)
		s.mu.Unlock()
		close(done)
		s.launches.Done()
	}()

	if err := s.suspend(live); err != nil {
		reason := "pause failed: " + err.Error()
		s.abandonPaused(key, live, reason)
		return s.failed(projectID, name, err)
	}
	for {
		w, err := s.Store.Get(projectID, name)
		if err != nil {
			s.abandonPaused(key, live, "pause failed: "+err.Error())
			return Worker{}, err
		}
		if !pausable(w.State) {
			err := fmt.Errorf("%w: %s became %s during its pause", ErrTransition, name, w.State)
			s.abandonPaused(key, live, "pause failed: "+err.Error())
			return s.failed(projectID, name, err)
		}
		if w.Assignment != nil {
			if err := s.endPausedTurn(w); err != nil {
				s.abandonPaused(key, live, "pause failed: "+err.Error())
				return s.failed(projectID, name, err)
			}
		}
		paused, err := s.Store.transitionFrom(w, Input{Event: Pause, Reason: reasonPaused,
			Guard: Guard{InterruptConfirmed: true, DescendantsStopped: true}})
		if errors.Is(err, ErrTransition) {
			// The worker changed concurrently: read it again.
			continue
		}
		if err != nil {
			s.abandonPaused(key, live, "pause failed: "+err.Error())
			return s.failed(projectID, name, err)
		}
		s.mu.Lock()
		s.paused[key] = live
		s.mu.Unlock()
		return paused, nil
	}
}

// suspend interrupts the agent of a session a Pause took, then stops its
// whole confined group and confirms it gone. The group's watcher then
// waits for the resume, keeping the slot. Any change of rights in
// progress finishes first.
func (s *Supervisor) suspend(live *running) error {
	live.turn.Lock()
	defer live.turn.Unlock()
	s.mu.Lock()
	if live.ended {
		s.mu.Unlock()
		return errors.New("the agent session ended before the pause")
	}
	live.replacing = make(chan struct{})
	s.mu.Unlock()
	live.detector.Interrupt()
	_, _ = live.epoch.Session().Write([]byte(interruptKey))
	if err := live.epoch.End("pause", s.grace(), func(session.Boundary) error { return nil }); err != nil {
		return err
	}
	live.role = ""
	return nil
}

// endPausedTurn interrupts the turn a pausing worker holds and blocks
// its task with its continuation, while the worker still holds the
// assignment: no other worker can take the task meanwhile.
func (s *Supervisor) endPausedTurn(w Worker) error {
	u, err := (turn.Store{DB: s.Store.DB, Now: s.Store.Now}).Get(w.Assignment.TurnID)
	if err != nil {
		return err
	}
	reason := fmt.Sprintf("turn %s of worker %s: %s", u.ID, w.Name, causePaused)
	return interruptTurn(s.Store, u, w.Assignment.TaskID, reason, causePaused)
}

// abandonPaused releases the watcher of a session a failed Pause took,
// then terminates it and fails its worker with the reason.
func (s *Supervisor) abandonPaused(key liveKey, live *running, reason string) {
	s.mu.Lock()
	held := live.replacing
	live.replacing = nil
	s.mu.Unlock()
	if held != nil {
		close(held)
	}
	s.abandon(key, live, reason)
}

// failed returns the worker as it is after a failed pause or resume,
// with the error.
func (s *Supervisor) failed(projectID, name string, cause error) (Worker, error) {
	w, err := s.Store.Get(projectID, name)
	if err != nil {
		return Worker{}, errors.Join(cause, err)
	}
	return w, cause
}

// Resume resumes a PAUSED worker whose session this supervisor kept. The
// profile its agent resumes under — read only, as between two turns —
// and the continuation — the confirmed native session the paused group
// ran — are validated, then the worker moves to STARTING and is
// answered at once. In the background, the exact native conversation is
// resumed in a new confined PTY and the worker reaches IDLE once its
// terminal settles, or FAILED, its session terminated and its slot
// released. Nothing is prompted: the task its pause blocked waits for a
// task resume. A worker that is not PAUSED, or being paused or stopped,
// is refused with ErrTransition, as is a PAUSED worker whose session was
// lost with a previous daemon; a profile or a continuation that does not
// hold is refused with ErrGuard, the worker staying PAUSED.
func (s *Supervisor) Resume(projectID, name string) (Worker, error) {
	s.init()
	key := liveKey{projectID, name}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.Store.Get(projectID, name)
	if err != nil {
		return Worker{}, err
	}
	live := s.paused[key]
	switch {
	case s.closed:
		return current, ErrClosed
	case s.pausing[key] != nil:
		return current, fmt.Errorf("%w: %s is being paused", ErrTransition, name)
	case s.stopping[key]:
		return current, fmt.Errorf("%w: %s is being stopped", ErrTransition, name)
	case current.State != Paused:
		return current, fmt.Errorf("%w: %s is %s, not %s", ErrTransition, name, current.State, Paused)
	case live == nil:
		return current, fmt.Errorf("%w: the paused session of %s was lost with a previous daemon; stop the worker, then start it",
			ErrTransition, name)
	}
	profile, resume, profileErr := live.profile(session.Review, s.environment(live.agentConfig))
	identity, epoch := live.native.Identity(), live.epoch.State()
	continuation := identity.Confirmed && identity.ID != "" && identity.ID == epoch.NativeID &&
		epoch.Phase == session.EpochRevoked
	next, err := s.Store.transitionFrom(current, Input{Event: Resume, Reason: "resume asked",
		Guard: Guard{ProfileValidated: profileErr == nil, ContinuationValid: continuation}})
	if err != nil {
		if profileErr != nil {
			err = fmt.Errorf("%w: %w", err, profileErr)
		}
		return current, err
	}
	delete(s.paused, key)
	s.launches.Add(1)
	go s.revive(key, live, profile, resume)
	return next, nil
}

// revive resumes the exact native conversation of a resumed worker in a
// new confined PTY, then moves the worker to IDLE once its terminal
// settles; any failure terminates the session and fails the worker.
func (s *Supervisor) revive(key liveKey, live *running, profile session.Profile, resume session.Resume) {
	defer s.launches.Done()
	err := s.restart(live, profile, resume)
	s.mu.Lock()
	held := live.replacing
	live.replacing = nil
	switch {
	case err != nil:
	case s.closed:
		err = ErrClosed
	default:
		live.paused = false
		s.live[key] = live
	}
	s.mu.Unlock()
	if held != nil {
		close(held)
	}
	if err == nil {
		_, err = s.Store.Transition(key.project, key.name, Input{
			Event: Ready, Reason: "native session " + live.native.Identity().ID + " resumed",
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
		s.abandon(key, live, "resume failed: "+err.Error())
		return
	}
	if s.Ready != nil {
		s.Ready(live.project)
	}
}

// restart resumes the conversation of a paused session under the
// profile, and waits for its terminal to settle.
func (s *Supervisor) restart(live *running, profile session.Profile, resume session.Resume) error {
	live.turn.Lock()
	defer live.turn.Unlock()
	if err := live.epoch.Restart(profile, resume); err != nil {
		return err
	}
	live.role = session.Review
	current := live.epoch.Session()
	s.follow(live, current)
	return s.ready(current)
}
