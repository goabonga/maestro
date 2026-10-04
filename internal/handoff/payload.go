// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package handoff

import (
	"bytes"
	"encoding/json"
	"strings"
)

// PlanPayload carries a Markdown body with fixed sections.
type PlanPayload struct {
	Body string `json:"body"`
}

// planSections are the headings every plan body must contain, in order.
var planSections = []string{"## Objective", "## Files", "## Steps", "## Tests"}

// ImplementationPayload describes the commits made since the task base.
type ImplementationPayload struct {
	Summary string `json:"summary"`
	// SourceSHA is the head the implementation ends at.
	SourceSHA string `json:"source_sha"`
	// Commits are the commits since the task base, oldest first.
	Commits  []string `json:"commits"`
	Commands []string `json:"commands"`
}

// ReviewIssue is one problem found by a review.
type ReviewIssue struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	File        string `json:"file"`
	Severity    string `json:"severity"`
}

// ReviewPayload is a verdict on one exact revision and diff.
type ReviewPayload struct {
	Verdict     string        `json:"verdict"`
	Issues      []ReviewIssue `json:"issues"`
	ReviewedSHA string        `json:"reviewed_sha"`
	// DiffDigest is the SHA-256 of the reviewed diff.
	DiffDigest string `json:"diff_digest"`
}

// TestReportPayload is produced by Maestro for one tested revision.
type TestReportPayload struct {
	TestedSHA   string            `json:"tested_sha"`
	Argv        []string          `json:"argv"`
	Environment map[string]string `json:"environment"`
	ExitCode    int               `json:"exit_code"`
	Log         string            `json:"log"`
}

// FixRequestPayload is produced by Maestro: what to fix, on which
// revision.
type FixRequestPayload struct {
	Revision   string   `json:"revision"`
	References []string `json:"references"`
}

// ConflictResolutionPayload is the commit resolving an integration
// conflict.
type ConflictResolutionPayload struct {
	OperationID        string `json:"operation_id"`
	IntegrationBaseSHA string `json:"integration_base_sha"`
	SourceSHA          string `json:"source_sha"`
	ResolutionSHA      string `json:"resolution_sha"`
}

var severities = map[string]bool{"low": true, "medium": true, "high": true, "critical": true}

// strict decodes a payload, refusing unknown fields and trailing data.
func strict(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return invalid("missing payload")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return invalid("payload: %v", err)
	}
	if decoder.More() {
		return invalid("trailing data after the payload")
	}
	return nil
}

// decodePayload validates the payload of a kind.
func decodePayload(kind Kind, raw json.RawMessage) (any, error) {
	switch kind {
	case Plan:
		var payload PlanPayload
		if err := strict(raw, &payload); err != nil {
			return nil, err
		}
		position := 0
		for _, section := range planSections {
			index := strings.Index(payload.Body[position:], section+"\n")
			if index < 0 {
				return nil, invalid("plan body misses the %q section, or has it out of order", section)
			}
			position += index + len(section)
		}
		return payload, nil
	case Implementation:
		var payload ImplementationPayload
		if err := strict(raw, &payload); err != nil {
			return nil, err
		}
		if strings.TrimSpace(payload.Summary) == "" || len(payload.Commits) == 0 {
			return nil, invalid("an implementation needs a summary and at least one commit")
		}
		for _, commit := range append([]string{payload.SourceSHA}, payload.Commits...) {
			if !objectID.MatchString(commit) {
				return nil, invalid("%q is not a commit id", commit)
			}
		}
		if payload.Commits[len(payload.Commits)-1] != payload.SourceSHA {
			return nil, invalid("the last commit is not the source head")
		}
		return payload, nil
	case Review:
		var payload ReviewPayload
		if err := strict(raw, &payload); err != nil {
			return nil, err
		}
		if payload.Verdict != "approve" && payload.Verdict != "changes_requested" {
			return nil, invalid("verdict %q is neither approve nor changes_requested", payload.Verdict)
		}
		if payload.Verdict == "changes_requested" && len(payload.Issues) == 0 {
			return nil, invalid("changes requested without any issue")
		}
		if !objectID.MatchString(payload.ReviewedSHA) || !isDigest(payload.DiffDigest) {
			return nil, invalid("a review needs the reviewed SHA and the diff digest")
		}
		for _, issue := range payload.Issues {
			if !identifier.MatchString(issue.ID) || strings.TrimSpace(issue.Description) == "" || !severities[issue.Severity] {
				return nil, invalid("review issue %q needs an id, a description and a known severity", issue.ID)
			}
		}
		return payload, nil
	case TestReport:
		var payload TestReportPayload
		if err := strict(raw, &payload); err != nil {
			return nil, err
		}
		if !objectID.MatchString(payload.TestedSHA) || len(payload.Argv) == 0 {
			return nil, invalid("a test report needs the tested SHA and its argv")
		}
		return payload, nil
	case FixRequest:
		var payload FixRequestPayload
		if err := strict(raw, &payload); err != nil {
			return nil, err
		}
		if !objectID.MatchString(payload.Revision) || len(payload.References) == 0 {
			return nil, invalid("a fix request needs a revision and at least one reference")
		}
		return payload, nil
	case ConflictResolution:
		var payload ConflictResolutionPayload
		if err := strict(raw, &payload); err != nil {
			return nil, err
		}
		if !identifier.MatchString(payload.OperationID) {
			return nil, invalid("operation_id %q is not a valid identifier", payload.OperationID)
		}
		for _, sha := range []string{payload.IntegrationBaseSHA, payload.SourceSHA, payload.ResolutionSHA} {
			if !objectID.MatchString(sha) {
				return nil, invalid("%q is not a commit id", sha)
			}
		}
		return payload, nil
	default:
		return nil, invalid("unknown kind %q", kind)
	}
}

// isDigest reports a lowercase hexadecimal SHA-256.
func isDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
