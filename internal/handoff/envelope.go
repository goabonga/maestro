// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package handoff defines the documents agents exchange through
// Maestro: a common versioned envelope around a typed payload,
// validated against its schema, the assignment that produced it and
// the Git history it describes. Nothing is ever parsed from terminal
// text.
package handoff

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// SchemaVersion is the envelope version this Maestro writes and reads.
const SchemaVersion = 1

// ErrInvalid wraps every contract violation.
var ErrInvalid = errors.New("invalid handoff")

// Kind is the type of a handoff document.
type Kind string

// The handoff kinds.
const (
	Plan               Kind = "PLAN"
	Implementation     Kind = "IMPLEMENTATION"
	Review             Kind = "REVIEW"
	TestReport         Kind = "TEST_REPORT"
	FixRequest         Kind = "FIX_REQUEST"
	ConflictResolution Kind = "CONFLICT_RESOLUTION"
)

// codeKinds are the kinds whose role works on code: they must carry
// the task base and the source head.
var codeKinds = map[Kind]bool{Implementation: true, Review: true, ConflictResolution: true}

// Envelope is the common part of every handoff document.
type Envelope struct {
	SchemaVersion    int             `json:"schema_version"`
	ArtifactID       string          `json:"artifact_id"`
	Kind             Kind            `json:"kind"`
	TaskID           string          `json:"task_id"`
	TurnID           string          `json:"turn_id"`
	AttemptID        string          `json:"attempt_id"`
	WorkerID         string          `json:"worker_id"`
	ConfigID         string          `json:"config_id"`
	InputArtifactIDs []string        `json:"input_artifact_ids"`
	TaskBaseSHA      string          `json:"task_base_sha,omitempty"`
	SourceHeadSHA    string          `json:"source_head_sha,omitempty"`
	Payload          json.RawMessage `json:"payload"`
}

// Assignment is what Maestro handed to the agent: every identifier the
// envelope must repeat exactly.
type Assignment struct {
	Kind      Kind
	TaskID    string
	TurnID    string
	AttemptID string
	WorkerID  string
	ConfigID  string
	// TaskBaseSHA is required for code kinds.
	TaskBaseSHA string
}

var (
	identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	objectID   = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// invalid builds a contract violation.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// Decode parses a handoff document strictly: unknown fields, trailing
// data and an unsupported schema version are refused, and the payload
// must match its kind's schema.
func Decode(data []byte) (Envelope, any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope Envelope
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, nil, invalid("envelope: %v", err)
	}
	if decoder.More() {
		return Envelope{}, nil, invalid("trailing data after the document")
	}
	if envelope.SchemaVersion != SchemaVersion {
		return Envelope{}, nil, invalid("schema version %d, this Maestro reads %d", envelope.SchemaVersion, SchemaVersion)
	}
	for name, value := range map[string]string{
		"artifact_id": envelope.ArtifactID, "task_id": envelope.TaskID, "turn_id": envelope.TurnID,
		"attempt_id": envelope.AttemptID, "worker_id": envelope.WorkerID, "config_id": envelope.ConfigID,
	} {
		if !identifier.MatchString(value) {
			return Envelope{}, nil, invalid("%s %q is not a valid identifier", name, value)
		}
	}
	for _, input := range envelope.InputArtifactIDs {
		if !identifier.MatchString(input) {
			return Envelope{}, nil, invalid("input artifact %q is not a valid identifier", input)
		}
	}
	if codeKinds[envelope.Kind] {
		if !objectID.MatchString(envelope.TaskBaseSHA) || !objectID.MatchString(envelope.SourceHeadSHA) {
			return Envelope{}, nil, invalid("%s needs task_base_sha and source_head_sha", envelope.Kind)
		}
	}
	payload, err := decodePayload(envelope.Kind, envelope.Payload)
	if err != nil {
		return Envelope{}, nil, err
	}
	return envelope, payload, nil
}

// Check verifies that the envelope answers exactly the assignment it
// was produced for: a document from another task, turn, attempt,
// worker or configuration never satisfies a turn.
func (e Envelope) Check(assignment Assignment) error {
	for _, field := range []struct{ name, got, want string }{
		{"kind", string(e.Kind), string(assignment.Kind)},
		{"task_id", e.TaskID, assignment.TaskID},
		{"turn_id", e.TurnID, assignment.TurnID},
		{"attempt_id", e.AttemptID, assignment.AttemptID},
		{"worker_id", e.WorkerID, assignment.WorkerID},
		{"config_id", e.ConfigID, assignment.ConfigID},
	} {
		if field.got != field.want {
			return invalid("%s is %q, the assignment expects %q", field.name, field.got, field.want)
		}
	}
	if codeKinds[e.Kind] && e.TaskBaseSHA != assignment.TaskBaseSHA {
		return invalid("task_base_sha is %s, the assignment expects %s", e.TaskBaseSHA, assignment.TaskBaseSHA)
	}
	return nil
}
