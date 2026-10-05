// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package config

import (
	"reflect"
	"testing"
	"time"
)

// base is a snapshot with one agent, one MCP server, one test command
// and one instruction file.
func base() Snapshot {
	cfg := Defaults()
	cfg.Agents = map[string]Agent{"coder": {Driver: "claude", MaxTurnsPerTask: 5}}
	cfg.MCP = map[string]MCP{"github": {Command: "github-mcp-server", Scope: "shared"}}
	cfg.Tests = map[string]TestCommand{"unit": {Argv: []string{"go", "test", "./..."}}}
	return Snapshot{
		Config:        cfg,
		Instructions:  map[string]string{"coder.md": "write tests"},
		HandoffSchema: 1,
		Drivers:       []DriverRange{{Kind: "claude", Name: "claude", Min: "1.0.0", Max: "2.0.0"}},
	}
}

// changed applies a modification to a fresh base snapshot.
func changed(modify func(*Snapshot)) Snapshot {
	snapshot := base()
	snapshot.Config.Agents = map[string]Agent{"coder": snapshot.Config.Agents["coder"]}
	modify(&snapshot)
	return snapshot
}

func TestDiffOfEqualSnapshotsIsEmpty(t *testing.T) {
	changes := Diff(base(), base())
	if len(changes) != 0 || changes.Impact() != ImpactNone || changes.String() != "no change" {
		t.Fatalf("changes %v", changes)
	}
}

func TestDiffClassifiesEveryKey(t *testing.T) {
	hour := Duration{time.Hour}
	for name, test := range map[string]struct {
		modify func(*Snapshot)
		want   Change
	}{
		"turn cap raised":     {func(s *Snapshot) { s.Config.Budgets.MaxTurnsPerTask = 40 }, Change{"budgets.max_turns_per_task", Changed, ImpactCeiling}},
		"turn cap lowered":    {func(s *Snapshot) { s.Config.Budgets.MaxTurnsPerTask = 10 }, Change{"budgets.max_turns_per_task", Changed, ImpactObjective}},
		"task timeout raised": {func(s *Snapshot) { s.Config.Budgets.TaskTimeout = Duration{3 * time.Hour} }, Change{"budgets.task_timeout", Changed, ImpactCeiling}},
		"turn timeout lowered": {func(s *Snapshot) { s.Config.Budgets.TurnTimeout = Duration{time.Minute} },
			Change{"budgets.turn_timeout", Changed, ImpactObjective}},
		"wait raised":        {func(s *Snapshot) { s.Config.Budgets.InputWaitTimeout = hour }, Change{"budgets.input_wait_timeout", Changed, ImpactCeiling}},
		"wall timeout added": {func(s *Snapshot) { s.Config.Budgets.WallTimeout = &hour }, Change{"budgets.wall_timeout", Added, ImpactObjective}},
		"agent cap raised":   {func(s *Snapshot) { s.Config.Agents["coder"] = Agent{Driver: "claude", MaxTurnsPerTask: 9} }, Change{"agents.coder.max_turns_per_task", Changed, ImpactCeiling}},
		"agent cap removed":  {func(s *Snapshot) { s.Config.Agents["coder"] = Agent{Driver: "claude"} }, Change{"agents.coder.max_turns_per_task", Changed, ImpactCeiling}},
		"agent cap lowered":  {func(s *Snapshot) { s.Config.Agents["coder"] = Agent{Driver: "claude", MaxTurnsPerTask: 2} }, Change{"agents.coder.max_turns_per_task", Changed, ImpactObjective}},
		"agent model":        {func(s *Snapshot) { s.Config.Agents["coder"] = Agent{Driver: "claude", Model: "x", MaxTurnsPerTask: 5} }, Change{"agents.coder.model", Changed, ImpactObjective}},
		"agent timeout raise": {func(s *Snapshot) {
			s.Config.Agents["coder"] = Agent{Driver: "claude", MaxTurnsPerTask: 5, TurnTimeout: &hour}
		}, Change{"agents.coder.turn_timeout", Changed, ImpactCeiling}},
		"agent added":   {func(s *Snapshot) { s.Config.Agents["reviewer"] = Agent{Driver: "codex"} }, Change{"agents.reviewer", Added, ImpactObjective}},
		"agent removed": {func(s *Snapshot) { delete(s.Config.Agents, "coder") }, Change{"agents.coder", Removed, ImpactObjective}},
		"mcp changed":   {func(s *Snapshot) { s.Config.MCP = map[string]MCP{"github": {Command: "gh", Scope: "shared"}} }, Change{"mcp.github", Changed, ImpactObjective}},
		"test changed":  {func(s *Snapshot) { s.Config.Tests = map[string]TestCommand{"unit": {Argv: []string{"make"}}} }, Change{"tests.unit", Changed, ImpactVerification}},
		"test added": {func(s *Snapshot) {
			s.Config.Tests = map[string]TestCommand{"unit": {Argv: []string{"go", "test", "./..."}}, "lint": {Argv: []string{"make", "lint"}}}
		}, Change{"tests.lint", Added, ImpactVerification}},
		"instruction edited":  {func(s *Snapshot) { s.Instructions = map[string]string{"coder.md": "write more tests"} }, Change{"maestro/coder.md", Changed, ImpactObjective}},
		"instruction removed": {func(s *Snapshot) { s.Instructions = map[string]string{} }, Change{"maestro/coder.md", Removed, ImpactObjective}},
		"handoff schema":      {func(s *Snapshot) { s.HandoffSchema = 2 }, Change{"handoff_schema", Changed, ImpactObjective}},
		"drivers":             {func(s *Snapshot) { s.Drivers[0].Max = "3.0.0" }, Change{"drivers", Changed, ImpactObjective}},
	} {
		got := Diff(base(), changed(test.modify))
		if !reflect.DeepEqual(got, Changes{test.want}) {
			t.Errorf("%s: got %v, want %v", name, got, test.want)
		}
	}
}

func TestDiffRemovingTheWallTimeoutRaisesACeiling(t *testing.T) {
	hour := Duration{time.Hour}
	old := base()
	old.Config.Budgets.WallTimeout = &hour
	changes := Diff(old, base())
	if !reflect.DeepEqual(changes, Changes{{"budgets.wall_timeout", Removed, ImpactCeiling}}) {
		t.Fatalf("changes %v", changes)
	}
	longer := Duration{2 * time.Hour}
	next := base()
	next.Config.Budgets.WallTimeout = &longer
	if changes := Diff(old, next); changes.Impact() != ImpactCeiling {
		t.Fatalf("changes %v", changes)
	}
}

func TestChangesReportTheStrongestImpact(t *testing.T) {
	next := changed(func(s *Snapshot) {
		s.Config.Budgets.MaxTurnsPerTask = 50
		s.Config.Tests = map[string]TestCommand{}
	})
	changes := Diff(base(), next)
	if changes.Impact() != ImpactVerification {
		t.Fatalf("impact %q of %v", changes.Impact(), changes)
	}
	want := "budgets.max_turns_per_task changed (ceiling); tests.unit removed (verification)"
	if changes.String() != want {
		t.Fatalf("got %q", changes.String())
	}
	next.Instructions = map[string]string{"coder.md": "x", "reviewer.md": "y"}
	if impact := Diff(base(), next).Impact(); impact != ImpactObjective {
		t.Fatalf("impact %q", impact)
	}
}
