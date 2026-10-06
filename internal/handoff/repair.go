// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package handoff

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// excluded keeps the handoff directory out of the worktree fingerprint:
// a repair legitimately writes its document there.
const excluded = ":(exclude).maestro/handoff"

// Snapshot fingerprints a worktree: its HEAD, its index and every
// change to tracked or untracked files, the handoff directory aside.
type Snapshot struct {
	Head    string
	Index   string
	Changes string
}

// Capture fingerprints a worktree.
func Capture(worktree string) (Snapshot, error) {
	head, err := git(worktree, "rev-parse", "HEAD")
	if err != nil {
		return Snapshot{}, err
	}
	index, err := digestOf(worktree, "ls-files", "--stage", "-z")
	if err != nil {
		return Snapshot{}, err
	}
	changes := sha256.New()
	diff, err := rawGit(worktree, "diff", "HEAD", "--binary", "--no-ext-diff", "--", ".", excluded)
	if err != nil {
		return Snapshot{}, err
	}
	changes.Write(diff)
	untracked, err := rawGit(worktree, "ls-files", "--others", "--exclude-standard", "-z", "--", ".", excluded)
	if err != nil {
		return Snapshot{}, err
	}
	for _, name := range bytes.Split(untracked, []byte{0}) {
		if len(name) == 0 {
			continue
		}
		content, err := os.ReadFile(filepath.Join(worktree, string(name))) // #nosec G304 -- a path Git lists inside the worktree
		if err != nil {
			return Snapshot{}, err
		}
		fmt.Fprintf(changes, "%s\x00%d\x00", name, len(content))
		changes.Write(content)
	}
	return Snapshot{Head: head, Index: index, Changes: hex.EncodeToString(changes.Sum(nil))}, nil
}

// Outcome is the verdict on a turn's handoff.
type Outcome string

// The outcomes.
const (
	// Accepted: the document is valid for the assignment.
	Accepted Outcome = "accepted"
	// RepairRequested: the document is missing or invalid; one repair
	// attempt may be asked for, from the captured snapshot.
	RepairRequested Outcome = "repair_requested"
	// Blocked: the task needs a human; nothing is retried.
	Blocked Outcome = "blocked"
)

// Verdict is the decision on one turn or repair, with what was read.
type Verdict struct {
	Outcome  Outcome
	Reason   string
	Envelope Envelope
	Payload  any
	Document []byte
	// Snapshot is the worktree before the repair, set with
	// RepairRequested.
	Snapshot Snapshot
}

// EndTurn decides on the handoff of a finished turn: accepted when
// valid, a single repair when missing or invalid, blocked on any other
// error — an error that is neither is ambiguous.
func EndTurn(worktree string, assignment Assignment) (Verdict, error) {
	envelope, payload, document, err := Read(worktree, assignment)
	if err == nil {
		return Verdict{Outcome: Accepted, Envelope: envelope, Payload: payload, Document: document}, nil
	}
	if !errors.Is(err, ErrMissing) && !errors.Is(err, ErrInvalid) {
		return Verdict{Outcome: Blocked, Reason: "ambiguous handoff: " + err.Error()}, nil
	}
	snapshot, captureErr := Capture(worktree)
	if captureErr != nil {
		return Verdict{}, captureErr
	}
	return Verdict{Outcome: RepairRequested, Reason: err.Error(), Snapshot: snapshot}, nil
}

// EndRepair decides on the single repair attempt. The repair must use
// a new attempt id and must not change the code: any difference in
// HEAD, index or files blocks the task, and so does a second missing
// or invalid document.
func EndRepair(worktree string, failed, repair Assignment, before Snapshot) (Verdict, error) {
	if repair.AttemptID == failed.AttemptID {
		return Verdict{}, invalid("the repair reuses attempt %s", failed.AttemptID)
	}
	after, err := Capture(worktree)
	if err != nil {
		return Verdict{}, err
	}
	switch {
	case after.Head != before.Head:
		return Verdict{Outcome: Blocked, Reason: "the repair created or moved commits"}, nil
	case after.Index != before.Index:
		return Verdict{Outcome: Blocked, Reason: "the repair changed the index"}, nil
	case after.Changes != before.Changes:
		return Verdict{Outcome: Blocked, Reason: "the repair changed the code"}, nil
	}
	envelope, payload, document, err := Read(worktree, repair)
	if err != nil {
		return Verdict{Outcome: Blocked, Reason: "the repair failed too: " + err.Error()}, nil
	}
	return Verdict{Outcome: Accepted, Envelope: envelope, Payload: payload, Document: document}, nil
}

// digestOf hashes the output of a git command.
func digestOf(worktree string, args ...string) (string, error) {
	output, err := rawGit(worktree, args...)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(output)
	return hex.EncodeToString(sum[:]), nil
}

// rawGit runs one read-only git command and returns its raw output.
func rawGit(worktree string, args ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", worktree}, args...)...) // #nosec G204 -- fixed verbs and pathspecs, never task text
	command.Env = gitEnvironment()
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git %v: %w", args, err)
	}
	return output, nil
}
