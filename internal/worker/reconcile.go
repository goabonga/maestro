// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/goabonga/maestro/internal/task"
)

// Survivors lists the processes still working in a directory. It only
// observes: no process it returns is signalled.
type Survivors func(dir string) ([]int, error)

// The reasons recorded by a reconciliation.
const (
	// reasonLost explains the stop of a worker whose runtime was lost.
	reasonLost = "runtime lost with the previous daemon"
	// reasonHeld explains the failure of a worker that held a turn.
	reasonHeld = "runtime lost with the previous daemon while holding turn %s of task %s"
	// reasonSurvivors explains the failure of a worker with survivors.
	reasonSurvivors = "unidentified processes %s still work in %s"
	// reasonSearch explains the failure of a worker whose survivors
	// could not be searched.
	reasonSearch = "the search for surviving processes in %s failed: %v"
)

// Reconciliation is the outcome of the startup reconciliation of one
// worker.
type Reconciliation struct {
	Name string
	// From and To are the worker's states before and after; they are
	// equal when the worker was left unchanged.
	From State
	To   State
	// Assignment is the assignment the worker holds after the
	// reconciliation: a failed worker keeps the one it held.
	Assignment *Assignment
	// Survivors are the processes found working in the worker's
	// repository.
	Survivors []int
	// TaskBlocked reports that the task of the held assignment was
	// blocked.
	TaskBlocked bool
	// Reason explains the decision; empty when nothing was found.
	Reason string
}

// Changed reports whether the reconciliation moved the worker.
func (r Reconciliation) Changed() bool {
	return r.From != r.To
}

// Reconcile reconciles the recorded workers of a project with a daemon
// that has just started and holds no runtime: every session of the
// previous daemon is lost. Nothing is signalled and no conversation is
// resumed; a worker is only moved through the transition table.
//
//   - A worker with processes still working in its repository, or whose
//     repository could not be searched, has an unidentified survivor:
//     an active worker fails, so that no second runtime can take the
//     same workspace; a STOPPED or FAILED one is only reported.
//   - An active worker holding an assignment fails and keeps it: the
//     turn lost its runtime and its effects are not reconciled. The
//     task of the assignment is blocked in the same transaction, unless
//     it is already blocked or finished.
//   - A PAUSED worker without survivors stays PAUSED: it had no
//     runtime to lose.
//   - Any other active worker is stopped: it had no turn and no process
//     remains.
//
// The reconciliation is idempotent: a second run finds the workers
// stopped, paused or failed and leaves them unchanged.
func (s Store) Reconcile(projectID string, survivors Survivors) ([]Reconciliation, error) {
	if survivors == nil {
		return nil, errors.New("a reconciliation needs a survivor search")
	}
	workers, err := s.List(projectID)
	if err != nil {
		return nil, err
	}
	outcomes := make([]Reconciliation, 0, len(workers))
	for _, w := range workers {
		outcome, err := s.reconcileOne(w, survivors)
		if err != nil {
			return outcomes, fmt.Errorf("reconcile worker %s: %w", w.Name, err)
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// reconcileOne decides and applies the reconciliation of one worker.
func (s Store) reconcileOne(w Worker, survivors Survivors) (Reconciliation, error) {
	outcome := Reconciliation{Name: w.Name, From: w.State, To: w.State, Assignment: w.Assignment}
	pids, err := survivors(w.Repository)
	outcome.Survivors = pids
	switch {
	case err != nil:
		outcome.Reason = fmt.Sprintf(reasonSearch, w.Repository, err)
	case len(pids) > 0:
		outcome.Reason = fmt.Sprintf(reasonSurvivors, joinPIDs(pids), w.Repository)
	}
	unidentified := outcome.Reason != ""

	var in Input
	switch {
	case !w.State.Active():
		return outcome, nil
	case unidentified && w.Assignment != nil:
		in = Input{Event: Fail, Reason: fmt.Sprintf(reasonHeld, w.Assignment.TurnID, w.Assignment.TaskID) + "; " + outcome.Reason}
	case unidentified:
		in = Input{Event: Fail, Reason: reasonLost + "; " + outcome.Reason}
	case w.Assignment != nil:
		in = Input{Event: Fail, Reason: fmt.Sprintf(reasonHeld, w.Assignment.TurnID, w.Assignment.TaskID)}
	case w.State == Paused:
		return outcome, nil
	default:
		// No turn and no process: there is no effect left to reconcile.
		in = Input{Event: Stop, Reason: reasonLost, Guard: Guard{Reconciled: true}}
	}

	at := s.now()
	next, err := Apply(w, in, at)
	if err != nil {
		return outcome, err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return outcome, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := write(tx, w, next, in, at); err != nil {
		return outcome, err
	}
	if next.State == Failed && next.Assignment != nil {
		tasks := task.Store{DB: s.DB, Now: s.Now}
		_, err := tasks.TransitionTx(tx, next.Assignment.TaskID,
			task.Input{Event: task.Block, Reason: "worker " + w.Name + " failed: " + next.Reason})
		switch {
		case err == nil:
			outcome.TaskBlocked = true
		case !errors.Is(err, task.ErrTransition):
			return outcome, err
		}
	}
	if err := tx.Commit(); err != nil {
		return outcome, err
	}
	outcome.To, outcome.Assignment, outcome.Reason = next.State, next.Assignment, next.Reason
	return outcome, nil
}

// joinPIDs renders process IDs for a reason.
func joinPIDs(pids []int) string {
	rendered := make([]string, len(pids))
	for i, pid := range pids {
		rendered[i] = strconv.Itoa(pid)
	}
	return strings.Join(rendered, ", ")
}
