// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"
)

// Impact is what a configuration change means for a task that adopts
// it, from the weakest to the strongest.
type Impact string

// The impacts of a change.
const (
	// ImpactNone: no change at all.
	ImpactNone Impact = ""
	// ImpactCeiling: a budget ceiling is raised or removed; nothing a
	// task produced depends on it.
	ImpactCeiling Impact = "ceiling"
	// ImpactVerification: the test commands changed; the verifications
	// of the task must run again.
	ImpactVerification Impact = "verification"
	// ImpactObjective: what the task is asked to do or how it is done
	// changed: instruction files, agents, MCP servers, the handoff
	// contract, the drivers, or a budget that is not a raised ceiling.
	ImpactObjective Impact = "objective"
)

// rank orders the impacts.
func (i Impact) rank() int {
	return slices.Index([]Impact{ImpactNone, ImpactCeiling, ImpactVerification, ImpactObjective}, i)
}

// The kinds of a change.
const (
	Added   = "added"
	Removed = "removed"
	Changed = "changed"
)

// Change is one key that differs between two snapshots.
type Change struct {
	// Key names the setting, such as budgets.max_turns_per_task,
	// agents.coder.model, tests.unit or instructions/coder.md.
	Key string `json:"key"`
	// Kind is added, removed or changed.
	Kind   string `json:"kind"`
	Impact Impact `json:"impact"`
}

// String renders the change for a journal or a terminal.
func (c Change) String() string {
	return fmt.Sprintf("%s %s (%s)", c.Key, c.Kind, c.Impact)
}

// Changes are the differences between two snapshots, sorted by key.
type Changes []Change

// Impact is the strongest impact of the changes; none without change.
func (c Changes) Impact() Impact {
	impact := ImpactNone
	for _, change := range c {
		if change.Impact.rank() > impact.rank() {
			impact = change.Impact
		}
	}
	return impact
}

// String renders the changes on one line.
func (c Changes) String() string {
	if len(c) == 0 {
		return "no change"
	}
	parts := make([]string, 0, len(c))
	for _, change := range c {
		parts = append(parts, change.String())
	}
	return strings.Join(parts, "; ")
}

// Diff compares the snapshot a task runs on with the one it would
// adopt, key by key. A raised or removed budget ceiling (a higher turn
// cap or timeout, a removed per-agent cap or wall_timeout) is a ceiling
// change; a lowered one, or any other budget change, is an objective
// change. A change of test command is a verification change.
// Everything else — instruction files, agents other than their raised
// ceilings, MCP servers, the handoff schema version and the drivers —
// is an objective change.
func Diff(old, next Snapshot) Changes {
	var changes Changes
	add := func(key, kind string, impact Impact) {
		changes = append(changes, Change{Key: key, Kind: kind, Impact: impact})
	}
	budgets(old.Config.Budgets, next.Config.Budgets, add)
	agents(old.Config, next.Config, add)
	tables(old.Config.MCP, next.Config.MCP, "mcp.", ImpactObjective, add)
	tables(old.Config.Tests, next.Config.Tests, "tests.", ImpactVerification, add)
	tables(old.Instructions, next.Instructions, InstructionsDir+"/", ImpactObjective, add)
	if old.HandoffSchema != next.HandoffSchema {
		add("handoff_schema", Changed, ImpactObjective)
	}
	if !reflect.DeepEqual(old.Drivers, next.Drivers) {
		add("drivers", Changed, ImpactObjective)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Key < changes[j].Key })
	return changes
}

// ceiling classifies a changed bound: raising it is a ceiling change,
// lowering it an objective one.
func ceiling(raised bool) Impact {
	if raised {
		return ImpactCeiling
	}
	return ImpactObjective
}

// budgets compares the project budgets.
func budgets(old, next Budgets, add func(string, string, Impact)) {
	if old.MaxTurnsPerTask != next.MaxTurnsPerTask {
		add("budgets.max_turns_per_task", Changed, ceiling(next.MaxTurnsPerTask > old.MaxTurnsPerTask))
	}
	for _, bound := range []struct {
		key       string
		old, next time.Duration
	}{
		{"budgets.turn_timeout", old.TurnTimeout.Duration, next.TurnTimeout.Duration},
		{"budgets.input_wait_timeout", old.InputWaitTimeout.Duration, next.InputWaitTimeout.Duration},
		{"budgets.task_timeout", old.TaskTimeout.Duration, next.TaskTimeout.Duration},
	} {
		if bound.old != bound.next {
			add(bound.key, Changed, ceiling(bound.next > bound.old))
		}
	}
	switch {
	case old.WallTimeout == nil && next.WallTimeout != nil:
		add("budgets.wall_timeout", Added, ImpactObjective)
	case old.WallTimeout != nil && next.WallTimeout == nil:
		add("budgets.wall_timeout", Removed, ImpactCeiling)
	case old.WallTimeout != nil && old.WallTimeout.Duration != next.WallTimeout.Duration:
		add("budgets.wall_timeout", Changed, ceiling(next.WallTimeout.Duration > old.WallTimeout.Duration))
	}
}

// agents compares the named agents. An added or removed agent is an
// objective change; within an agent, its turn cap (0 meaning none) and
// its effective turn timeout are ceilings, every other field an
// objective change.
func agents(old, next Config, add func(string, string, Impact)) {
	for _, name := range keys(old.Agents, next.Agents) {
		before, inOld := old.Agents[name]
		after, inNext := next.Agents[name]
		prefix := "agents." + name
		switch {
		case !inOld:
			add(prefix, Added, ImpactObjective)
			continue
		case !inNext:
			add(prefix, Removed, ImpactObjective)
			continue
		}
		for _, field := range []struct{ key, old, next string }{
			{"driver", before.Driver, after.Driver},
			{"base_url", before.BaseURL, after.BaseURL},
			{"model", before.Model, after.Model},
			{"api_key_env", before.APIKeyEnv, after.APIKeyEnv},
		} {
			if field.old != field.next {
				add(prefix+"."+field.key, Changed, ImpactObjective)
			}
		}
		if before.MaxTurnsPerTask != after.MaxTurnsPerTask {
			raised := after.MaxTurnsPerTask == 0 || before.MaxTurnsPerTask != 0 && after.MaxTurnsPerTask > before.MaxTurnsPerTask
			add(prefix+".max_turns_per_task", Changed, ceiling(raised))
		}
		if !reflect.DeepEqual(before.TurnTimeout, after.TurnTimeout) {
			effective := func(cfg Config, agent Agent) time.Duration {
				if agent.TurnTimeout != nil {
					return agent.TurnTimeout.Duration
				}
				return cfg.Budgets.TurnTimeout.Duration
			}
			add(prefix+".turn_timeout", Changed, ceiling(effective(next, after) >= effective(old, before)))
		}
	}
}

// tables compares two named tables whose every change has one impact.
func tables[T any](old, next map[string]T, prefix string, impact Impact, add func(string, string, Impact)) {
	for _, name := range keys(old, next) {
		before, inOld := old[name]
		after, inNext := next[name]
		switch {
		case !inOld:
			add(prefix+name, Added, impact)
		case !inNext:
			add(prefix+name, Removed, impact)
		case !equal(before, after):
			add(prefix+name, Changed, impact)
		}
	}
}

// equal compares two values by their canonical JSON form, the form a
// snapshot is identified by.
func equal(a, b any) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	return errLeft == nil && errRight == nil && string(left) == string(right)
}

// keys returns the sorted union of the keys of two maps.
func keys[T any](old, next map[string]T) []string {
	var union []string
	for key := range old {
		union = append(union, key)
	}
	for key := range next {
		if _, ok := old[key]; !ok {
			union = append(union, key)
		}
	}
	sort.Strings(union)
	return union
}
