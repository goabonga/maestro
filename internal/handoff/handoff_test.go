// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package handoff

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var (
	sha1 = strings.Repeat("a", 40)
	sha2 = strings.Repeat("b", 40)
	sum  = strings.Repeat("c", 64)
)

// document builds a handoff document of a kind around a payload, with
// the identifiers of the default assignment.
func document(t *testing.T, kind Kind, payload any, change func(map[string]any)) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope := map[string]any{
		"schema_version": SchemaVersion, "artifact_id": "art-1", "kind": kind,
		"task_id": "task-1", "turn_id": "turn-1", "attempt_id": "attempt-1",
		"worker_id": "claude-01", "config_id": "config-1", "input_artifact_ids": []string{},
		"task_base_sha": sha1, "source_head_sha": sha2, "payload": json.RawMessage(raw),
	}
	if change != nil {
		change(envelope)
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assignment(kind Kind) Assignment {
	return Assignment{Kind: kind, TaskID: "task-1", TurnID: "turn-1", AttemptID: "attempt-1",
		WorkerID: "claude-01", ConfigID: "config-1", TaskBaseSHA: sha1}
}

const planBody = "# Plan\n## Objective\nDo it.\n## Files\na.go\n## Steps\n1. edit\n## Tests\ngo test\n"

func TestDecodeAcceptsEveryKind(t *testing.T) {
	for kind, payload := range map[Kind]any{
		Plan:               PlanPayload{Body: planBody},
		Implementation:     ImplementationPayload{Summary: "done", SourceSHA: sha2, Commits: []string{sha1, sha2}},
		Review:             ReviewPayload{Verdict: "approve", ReviewedSHA: sha2, DiffDigest: sum},
		TestReport:         TestReportPayload{TestedSHA: sha2, Argv: []string{"go", "test", "./..."}},
		FixRequest:         FixRequestPayload{Revision: sha2, References: []string{"issue-1"}},
		ConflictResolution: ConflictResolutionPayload{OperationID: "op-1", IntegrationBaseSHA: sha1, SourceSHA: sha2, ResolutionSHA: sha2},
	} {
		envelope, decoded, err := Decode(document(t, kind, payload, nil))
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if envelope.Kind != kind || decoded == nil {
			t.Fatalf("%s: envelope %+v payload %v", kind, envelope, decoded)
		}
		if err := envelope.Check(assignment(kind)); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
}

func TestDecodeRefusesContractViolations(t *testing.T) {
	plan := PlanPayload{Body: planBody}
	cases := map[string][]byte{
		"unknown field":   document(t, Plan, plan, func(e map[string]any) { e["extra"] = 1 }),
		"schema version":  document(t, Plan, plan, func(e map[string]any) { e["schema_version"] = 2 }),
		"bad identifier":  document(t, Plan, plan, func(e map[string]any) { e["task_id"] = "../escape" }),
		"bad input id":    document(t, Plan, plan, func(e map[string]any) { e["input_artifact_ids"] = []string{"a b"} }),
		"unknown kind":    document(t, "POEM", plan, nil),
		"missing payload": document(t, Plan, plan, func(e map[string]any) { delete(e, "payload") }),
		"plan sections":   document(t, Plan, PlanPayload{Body: "## Objective\n## Steps\n## Files\n## Tests\n"}, nil),
		"payload field":   document(t, Plan, map[string]string{"body": planBody, "x": "y"}, nil),
		"code kind shas": document(t, Implementation, ImplementationPayload{Summary: "s", SourceSHA: sha2, Commits: []string{sha2}},
			func(e map[string]any) { delete(e, "task_base_sha") }),
		"last commit":       document(t, Implementation, ImplementationPayload{Summary: "s", SourceSHA: sha2, Commits: []string{sha2, sha1}}, nil),
		"no commits":        document(t, Implementation, ImplementationPayload{Summary: "s", SourceSHA: sha2}, nil),
		"verdict":           document(t, Review, ReviewPayload{Verdict: "maybe", ReviewedSHA: sha2, DiffDigest: sum}, nil),
		"changes, no issue": document(t, Review, ReviewPayload{Verdict: "changes_requested", ReviewedSHA: sha2, DiffDigest: sum}, nil),
		"severity": document(t, Review, ReviewPayload{Verdict: "changes_requested", ReviewedSHA: sha2, DiffDigest: sum,
			Issues: []ReviewIssue{{ID: "i1", Description: "d", Severity: "urgent"}}}, nil),
		"diff digest":    document(t, Review, ReviewPayload{Verdict: "approve", ReviewedSHA: sha2, DiffDigest: "abc"}, nil),
		"test argv":      document(t, TestReport, TestReportPayload{TestedSHA: sha2}, nil),
		"fix references": document(t, FixRequest, FixRequestPayload{Revision: sha2}, nil),
		"resolution sha": document(t, ConflictResolution, ConflictResolutionPayload{OperationID: "op-1", IntegrationBaseSHA: sha1, SourceSHA: sha2, ResolutionSHA: "nope"}, nil),
		"trailing data":  append(document(t, Plan, plan, nil), []byte(` {}`)...),
	}
	for name, data := range cases {
		if _, _, err := Decode(data); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: expected ErrInvalid, got %v", name, err)
		}
	}
}

func TestCheckRefusesAnotherAssignment(t *testing.T) {
	envelope, _, err := Decode(document(t, Implementation,
		ImplementationPayload{Summary: "s", SourceSHA: sha2, Commits: []string{sha2}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Assignment){
		"kind":      func(a *Assignment) { a.Kind = Review },
		"task":      func(a *Assignment) { a.TaskID = "task-2" },
		"turn":      func(a *Assignment) { a.TurnID = "turn-2" },
		"attempt":   func(a *Assignment) { a.AttemptID = "attempt-0" },
		"worker":    func(a *Assignment) { a.WorkerID = "codex-01" },
		"config":    func(a *Assignment) { a.ConfigID = "config-2" },
		"task base": func(a *Assignment) { a.TaskBaseSHA = sha2 },
	} {
		expected := assignment(Implementation)
		change(&expected)
		if err := envelope.Check(expected); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: expected ErrInvalid, got %v", name, err)
		}
	}
}

// history builds a repository with a base commit and two commits on
// top, and returns its path, the base and the two commits.
func history(t *testing.T) (string, string, []string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		command := exec.Command("git", args...)
		command.Dir = dir
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.test")
	run("config", "commit.gpgsign", "false")
	var commits []string
	for i, name := range []string{"base", "first", "second"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		run("add", name)
		run("commit", "-q", "-m", "feat: "+name)
		if i > 0 {
			commits = append(commits, run("rev-parse", "HEAD"))
		}
	}
	return dir, run("rev-parse", "HEAD~2"), commits
}

func TestVerifyCommitsMatchesGit(t *testing.T) {
	repository, base, commits := history(t)
	envelope := Envelope{TaskBaseSHA: base, SourceHeadSHA: commits[1]}
	good := ImplementationPayload{Summary: "s", SourceSHA: commits[1], Commits: commits}
	if err := VerifyCommits(repository, envelope, good); err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string]ImplementationPayload{
		"missing commit": {SourceSHA: commits[1], Commits: commits[1:]},
		"wrong order":    {SourceSHA: commits[1], Commits: []string{commits[1], commits[0]}},
		"head mismatch":  {SourceSHA: commits[0], Commits: commits[:1]},
		"unknown head":   {SourceSHA: sha2, Commits: []string{sha2}},
	} {
		check := envelope
		if name == "unknown head" {
			check.SourceHeadSHA = sha2
		}
		if err := VerifyCommits(repository, check, payload); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: expected ErrInvalid, got %v", name, err)
		}
	}
	// A head that does not descend from the base is refused.
	reversed := Envelope{TaskBaseSHA: commits[1], SourceHeadSHA: base}
	if err := VerifyCommits(repository, reversed, ImplementationPayload{SourceSHA: base, Commits: []string{base}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-descendant: %v", err)
	}
}
