// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goabonga/maestro/internal/handoff"
	"github.com/goabonga/maestro/internal/turn"
)

// maxLogTail bounds the part of a failed test's log quoted in a
// correction prompt.
const maxLogTail = 4 << 10

// prompt renders the prompt of a turn for the job's role: the task, the
// role's instructions with its input artifacts, and the handoff
// document the turn must leave.
func (j *job) prompt(u turn.Turn) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "You are worker %s of Maestro, in the %s turn of task %s.\n\nTask:\n%s\n\n",
		j.worker.Name, j.role, j.task.ID, j.task.Description)
	var inputs []string
	switch j.role {
	case Planning:
		b.WriteString("Write a plan for this task. Do not modify any file and do not create any commit.\n")
	case Implementation:
		plan, err := handoff.Latest(j.e.DB, j.task.ID, handoff.Plan)
		if err != nil {
			return "", err
		}
		body, err := planBody(plan)
		if err != nil {
			return "", err
		}
		inputs = append(inputs, plan.Envelope.ArtifactID)
		fmt.Fprintf(&b, "Implement the task on the current branch, starting from %s, following this plan:\n\n%s\n\n",
			j.task.BaseSHA, body)
		b.WriteString(commitRules)
	case Correction:
		request, details, err := j.fixRequest()
		if err != nil {
			return "", err
		}
		inputs = append(inputs, request)
		fmt.Fprintf(&b, "Fix revision %s on the current branch. The fix request %s asks for:\n\n%s\n",
			j.task.HeadSHA, request, details)
		b.WriteString(commitRules)
	case Review:
		implementation, err := handoff.Latest(j.e.DB, j.task.ID, handoff.Implementation)
		if err != nil {
			return "", err
		}
		digest, err := diffDigest(j.project.Repository(), j.task.BaseSHA, j.task.HeadSHA)
		if err != nil {
			return "", err
		}
		inputs = append(inputs, implementation.Envelope.ArtifactID)
		fmt.Fprintf(&b, "Review the changes of revision %s against the task base %s (git diff %s %s), whose diff digest is %s. "+
			"Do not modify any file and do not create any commit.\n", j.task.HeadSHA, j.task.BaseSHA,
			j.task.BaseSHA, j.task.HeadSHA, digest)
		return j.withDocument(&b, u, inputs, j.payloadTemplate(digest))
	}
	return j.withDocument(&b, u, inputs, j.payloadTemplate(""))
}

// commitRules are the rules of a turn that writes code.
const commitRules = "Commit your changes on the current branch and leave no uncommitted change. " +
	"Do not add or modify the files Maestro provisioned, nor anything under .maestro/ but your handoff document.\n"

// repairPrompt asks for the single format repair of a refused handoff,
// in a new attempt, without any change to the code.
func (j *job) repairPrompt(r turn.Turn, failed handoff.Assignment, reason string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Your handoff document for turn %s, attempt %s, was refused: %s\n\n", failed.TurnID, failed.AttemptID, reason)
	b.WriteString("Write it again for this new attempt. Only write the handoff document: do not modify any other file, " +
		"do not stage anything and do not create any commit.\n")
	var inputs []string
	if kind, ok := inputKind[j.role]; ok {
		prior, err := handoff.Latest(j.e.DB, j.task.ID, kind)
		if err != nil {
			return "", err
		}
		inputs = append(inputs, prior.Envelope.ArtifactID)
	}
	digest := ""
	if j.kind == handoff.Review {
		var err error
		if digest, err = diffDigest(j.project.Repository(), j.task.BaseSHA, j.task.HeadSHA); err != nil {
			return "", err
		}
	}
	return j.withDocument(&b, r, inputs, j.payloadTemplate(digest))
}

// inputKind is the artifact a role works from.
var inputKind = map[Role]handoff.Kind{
	Implementation: handoff.Plan,
	Correction:     handoff.FixRequest,
	Review:         handoff.Implementation,
}

// payloadTemplate describes the payload of the job's handoff; a review
// carries the digest of the reviewed diff.
func (j *job) payloadTemplate(digest string) map[string]any {
	switch j.kind {
	case handoff.Plan:
		return map[string]any{"body": "<Markdown with the sections ## Objective, ## Files, ## Steps and ## Tests>"}
	case handoff.Review:
		return map[string]any{
			"verdict": "<approve or changes_requested>",
			"issues": []map[string]string{{"id": "<issue id>", "description": "<the problem>", "file": "<path>",
				"severity": "<low, medium, high or critical>"}},
			"reviewed_sha": j.task.HeadSHA, "diff_digest": digest,
		}
	}
	return map[string]any{
		"summary":    "<what you changed>",
		"source_sha": "<the last commit of the branch>",
		"commits":    []string{"<every commit since " + j.task.BaseSHA + ", oldest first, the last one being source_sha>"},
		"commands":   []string{"<the commands you ran>"},
	}
}

// withDocument appends the handoff instructions of a turn to a prompt:
// where to write the document, and the document with every identifier
// Maestro checks.
func (j *job) withDocument(b *strings.Builder, u turn.Turn, inputs []string, payload map[string]any) (string, error) {
	path, err := handoff.Path(u.ID, u.AttemptID)
	if err != nil {
		return "", err
	}
	if inputs == nil {
		inputs = []string{}
	}
	document := map[string]any{
		"schema_version": handoff.SchemaVersion, "artifact_id": strings.ToLower(string(j.kind)) + "-" + u.AttemptID,
		"kind": j.kind, "task_id": j.task.ID, "turn_id": u.ID, "attempt_id": u.AttemptID, "worker_id": j.worker.Name,
		"config_id": j.task.ConfigID, "input_artifact_ids": inputs, "payload": payload,
	}
	switch j.kind {
	case handoff.Implementation:
		document["task_base_sha"] = j.task.BaseSHA
		document["source_head_sha"] = "<the same commit as source_sha>"
	case handoff.Review:
		document["task_base_sha"] = j.task.BaseSHA
		document["source_head_sha"] = j.task.HeadSHA
	}
	var rendered bytes.Buffer
	encoder := json.NewEncoder(&rendered)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return "", err
	}
	fmt.Fprintf(b, "\nWhen you are done, write your handoff document to %s.tmp in the root of your worktree, "+
		"then rename it to %s. Fill in the values in angle brackets and keep every other value as given:\n\n%s",
		path, path, rendered.String())
	return b.String(), nil
}

// planBody returns the Markdown body of an accepted plan.
func planBody(plan handoff.Stored) (string, error) {
	_, payload, err := handoff.Decode(plan.Document)
	if err != nil {
		return "", err
	}
	body, ok := payload.(handoff.PlanPayload)
	if !ok {
		return "", fmt.Errorf("artifact %s is not a plan", plan.Envelope.ArtifactID)
	}
	return body.Body, nil
}

// fixRequest returns the fix request of the revision under correction
// and the details of what it references: the issues of a review, the
// failed tests with the tail of their log.
func (j *job) fixRequest() (string, string, error) {
	request, err := handoff.Latest(j.e.DB, j.task.ID, handoff.FixRequest)
	if err != nil {
		return "", "", err
	}
	_, decoded, err := handoff.Decode(request.Document)
	if err != nil {
		return "", "", err
	}
	payload, ok := decoded.(handoff.FixRequestPayload)
	if !ok || payload.Revision != j.task.HeadSHA {
		return "", "", fmt.Errorf("no fix request for revision %s", j.task.HeadSHA)
	}
	wanted := map[string]bool{}
	for _, reference := range payload.References {
		wanted[reference] = true
	}
	var details strings.Builder
	for _, input := range request.Envelope.InputArtifactIDs {
		stored, err := handoff.Load(j.e.DB, input)
		if err != nil {
			return "", "", err
		}
		_, decoded, err := handoff.Decode(stored.Document)
		if err != nil {
			return "", "", err
		}
		switch artifact := decoded.(type) {
		case handoff.ReviewPayload:
			for _, issue := range artifact.Issues {
				if wanted[issue.ID] {
					fmt.Fprintf(&details, "- %s (%s, %s): %s\n", issue.ID, issue.Severity, issue.File, issue.Description)
				}
			}
		case handoff.TestReportPayload:
			if !wanted[input] {
				continue
			}
			log := artifact.Log
			if len(log) > maxLogTail {
				log = log[len(log)-maxLogTail:]
			}
			fmt.Fprintf(&details, "- test %s (%s) exited %d; end of its log:\n%s\n",
				artifact.Name, strings.Join(artifact.Argv, " "), artifact.ExitCode, log)
		}
	}
	return request.Envelope.ArtifactID, details.String(), nil
}
