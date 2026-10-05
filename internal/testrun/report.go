// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package testrun

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/goabonga/maestro/internal/handoff"
	"github.com/goabonga/maestro/internal/state"
)

// ErrRevision reports a test report used for a revision it did not test.
var ErrRevision = errors.New("test report is for another revision")

// Identity places a run's reports in the task's history. Each command
// gets the artifact id ArtifactPrefix-<command name>.
type Identity struct {
	ArtifactPrefix   string
	TaskID           string
	TurnID           string
	AttemptID        string
	WorkerID         string
	ConfigID         string
	InputArtifactIDs []string
}

// Documents renders one TEST_REPORT handoff document per command, each
// validated against the handoff contract.
func (r Run) Documents(identity Identity) ([][]byte, error) {
	if len(r.Results) == 0 {
		return nil, ErrNoCommands
	}
	inputs := identity.InputArtifactIDs
	if inputs == nil {
		inputs = []string{}
	}
	documents := make([][]byte, 0, len(r.Results))
	for _, result := range r.Results {
		payload, err := json.Marshal(handoff.TestReportPayload{
			TestedSHA:      r.TestedSHA,
			Argv:           result.Argv,
			Environment:    Environment(),
			ExitCode:       result.ExitCode,
			Log:            result.Output,
			Name:           result.Name,
			DurationMS:     result.Duration.Milliseconds(),
			TimedOut:       result.TimedOut,
			LogTruncated:   result.Truncated,
			ChangedPaths:   result.Changed,
			UntrackedPaths: result.Untracked,
		})
		if err != nil {
			return nil, err
		}
		document, err := json.Marshal(handoff.Envelope{
			SchemaVersion:    handoff.SchemaVersion,
			ArtifactID:       identity.ArtifactPrefix + "-" + result.Name,
			Kind:             handoff.TestReport,
			TaskID:           identity.TaskID,
			TurnID:           identity.TurnID,
			AttemptID:        identity.AttemptID,
			WorkerID:         identity.WorkerID,
			ConfigID:         identity.ConfigID,
			InputArtifactIDs: inputs,
			Payload:          payload,
		})
		if err != nil {
			return nil, err
		}
		if _, _, err := handoff.Decode(document); err != nil {
			return nil, fmt.Errorf("test %s: %w", result.Name, err)
		}
		documents = append(documents, document)
	}
	return documents, nil
}

// Accept persists the run's reports as accepted artifacts. Every
// document is rendered and validated before the first one is stored.
func Accept(db *state.DB, run Run, identity Identity) ([]handoff.Stored, error) {
	documents, err := run.Documents(identity)
	if err != nil {
		return nil, err
	}
	stored := make([]handoff.Stored, 0, len(documents))
	for _, document := range documents {
		envelope, _, err := handoff.Decode(document)
		if err != nil {
			return stored, err
		}
		accepted, err := handoff.Accept(db, envelope, document)
		if err != nil {
			return stored, err
		}
		stored = append(stored, accepted)
	}
	return stored, nil
}

// ForRevision decodes a TEST_REPORT document and returns its payload
// only when it tested revision exactly: a result never carries over to
// another SHA.
func ForRevision(document []byte, revision string) (handoff.TestReportPayload, error) {
	envelope, decoded, err := handoff.Decode(document)
	if err != nil {
		return handoff.TestReportPayload{}, err
	}
	if envelope.Kind != handoff.TestReport {
		return handoff.TestReportPayload{}, fmt.Errorf("%w: %s is a %s, not a test report", handoff.ErrInvalid, envelope.ArtifactID, envelope.Kind)
	}
	payload := decoded.(handoff.TestReportPayload)
	if payload.TestedSHA != revision {
		return handoff.TestReportPayload{}, fmt.Errorf("%w: %s tested %s, the revision under evaluation is %s",
			ErrRevision, envelope.ArtifactID, payload.TestedSHA, revision)
	}
	return payload, nil
}

// Passed reports a test report whose command exited 0 in time without
// changing the tracked files or the index.
func Passed(payload handoff.TestReportPayload) bool {
	return payload.ExitCode == 0 && !payload.TimedOut && len(payload.ChangedPaths) == 0
}
