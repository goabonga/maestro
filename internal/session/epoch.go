// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package session

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/goabonga/maestro/internal/launcher"
)

// Epoch phases are separate from the PTY's process lifecycle.
const (
	EpochActive  = "active"
	EpochRevoked = "revoked"
	EpochBlocked = "blocked"
)

// Boundary identifies the epoch whose runtime has been stopped. Reconcile
// inspects Git and handoffs and persists its proofs before returning.
type Boundary struct {
	Epoch    uint64
	Revision string
	Reason   string
}

// Reconcile is required at every boundary. An error keeps the assignment
// blocked, without a replacement process or permission to release it.
type Reconcile func(Boundary) error

// Resume builds an interactive command for exactly the confirmed native ID.
// Drivers must never substitute a latest-session selector.
type Resume func(nativeID string) ([]string, error)

// EpochState reports whether an assignment can admit input or be released.
type EpochState struct {
	Epoch    uint64
	Phase    string
	NativeID string
	Error    string
}

// Epoch supervises a permissions profile using stop-and-recreate. It never
// claims that changing an input structure changes an existing mount namespace.
// Operations serialize; the old Session is permanently fenced at a boundary.
type Epoch struct {
	opMu      sync.Mutex
	mu        sync.Mutex
	launcher  *launcher.Launcher
	profile   Profile
	session   *Session
	phase     string
	nativeID  string
	lastError string
}

// StartEpoch admits a validated profile in a fresh confined PTY. Native ID
// confirmation is separate: the driver must observe or choose the actual ID.
func StartEpoch(l *launcher.Launcher, profile Profile) (*Epoch, error) {
	if l == nil || profile.spec.Epoch == 0 {
		return nil, errors.New("launcher and validated profile are required")
	}
	// Revalidate canonical mount paths at admission, not just construction.
	validated, err := profile.validateAdmission()
	if err != nil {
		return nil, err
	}
	live, err := Start(l, validated.confinedConfig(validated.spec.Config.Spec.Argv))
	if err != nil {
		return nil, err
	}
	return &Epoch{launcher: l, profile: validated, session: live, phase: EpochActive}, nil
}

// ConfirmNativeID binds the conversation once. A different ID is refused.
func (e *Epoch) ConfirmNativeID(id string) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if id == "" || e.phase != EpochActive {
		return errors.New("native ID confirmation requires an active epoch and a nonempty ID")
	}
	if e.nativeID != "" && e.nativeID != id {
		return errors.New("native conversation ID cannot change")
	}
	e.nativeID = id
	return nil
}

// Session returns the PTY for streaming. Retained pointers cannot bypass the
// input fence once End begins, and can never target a replacement PTY.
func (e *Epoch) Session() *Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.session
}

// State exposes the confirmed epoch and the last boundary error.
func (e *Epoch) State() EpochState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return EpochState{Epoch: e.profile.spec.Epoch, Phase: e.phase, NativeID: e.nativeID, Error: e.lastError}
}

// End fences input, stops the complete PID namespace, then reconciles the
// stable sources and handoffs. Success alone authorizes assignment release.
// Reasons include turn completion, interruption, pause and human detach.
func (e *Epoch) End(reason string, grace time.Duration, reconcile Reconcile) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	if e.phase == EpochRevoked {
		e.mu.Unlock()
		return nil
	}
	live := e.session
	boundary := Boundary{Epoch: e.profile.spec.Epoch, Revision: e.profile.spec.Revision, Reason: reason}
	live.BlockInput()
	e.phase = EpochBlocked
	e.mu.Unlock()
	if err := live.Stop(grace); err != nil {
		return e.block(err)
	}
	if reconcile == nil || reason == "" {
		return e.block(errors.New("boundary reason and reconciliation are required"))
	}
	if err := reconcile(boundary); err != nil {
		return e.block(fmt.Errorf("reconcile permissions boundary: %w", err))
	}
	e.mu.Lock()
	e.phase = EpochRevoked
	e.lastError = ""
	e.mu.Unlock()
	return nil
}

// Restart admits a strictly newer epoch only after a successful boundary,
// using the exact confirmed conversation ID in a new confined PTY.
func (e *Epoch) Restart(profile Profile, resume Resume) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	state := e.State()
	if state.Phase != EpochRevoked {
		return errors.New("permissions boundary is not reconciled")
	}
	if profile.spec.Epoch <= state.Epoch {
		return errors.New("permissions epoch must increase")
	}
	if state.NativeID == "" || resume == nil {
		return e.block(errors.New("restart requires a confirmed native session ID and its driver"))
	}
	validated, err := profile.validateAdmission()
	if err != nil {
		return e.block(err)
	}
	argv, err := resume(state.NativeID)
	if err != nil {
		return e.block(err)
	}
	if len(argv) == 0 {
		return e.block(errors.New("empty resume command"))
	}
	e.mu.Lock()
	e.profile = validated
	e.phase = EpochBlocked
	e.mu.Unlock()
	live, err := Start(e.launcher, validated.confinedConfig(argv))
	if err != nil {
		return e.block(fmt.Errorf("restart confined PTY: %w", err))
	}
	e.mu.Lock()
	e.session = live
	e.phase = EpochActive
	e.lastError = ""
	e.mu.Unlock()
	return nil
}

func (e *Epoch) block(err error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.phase = EpochBlocked
	e.lastError = err.Error()
	return err
}
