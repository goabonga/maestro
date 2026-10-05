// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestOverviewListsEveryCommand(t *testing.T) {
	var output bytes.Buffer
	if err := Run(context.Background(), nil, &output, "0.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range commands() {
		if !strings.Contains(output.String(), "  "+entry.name+" ") {
			t.Fatalf("the overview misses %q:\n%s", entry.name, output.String())
		}
	}
	if !strings.Contains(output.String(), "maestro help <command>") {
		t.Fatalf("the overview does not point to command help:\n%s", output.String())
	}
}

func TestEveryCommandHasItsHelp(t *testing.T) {
	seen := map[string]bool{}
	for _, entry := range commands() {
		if seen[entry.name] {
			t.Fatalf("command %q is declared twice", entry.name)
		}
		seen[entry.name] = true
		if entry.summary == "" || len(entry.usage) == 0 || entry.run == nil {
			t.Fatalf("command %q lacks a summary, a usage or a run function", entry.name)
		}
		var output bytes.Buffer
		if err := Run(context.Background(), []string{"help", entry.name}, &output, "0.0.0"); err != nil {
			t.Fatal(err)
		}
		for _, line := range entry.usage {
			if !strings.Contains(output.String(), line) {
				t.Fatalf("help %s misses %q:\n%s", entry.name, line, output.String())
			}
		}
	}
}

func TestEveryListedCommandIsDispatched(t *testing.T) {
	// A listed command is reached through the table: called without its
	// arguments it answers with its own usage, never "unknown command".
	for _, name := range []string{"project", "daemon", "task", "sync", "worktree", "diff", "attach", "agent", "backup", "restore", "gc"} {
		var output bytes.Buffer
		err := Run(context.Background(), []string{name}, &output, "0.0.0")
		if err != nil && strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("%s is not dispatched: %v", name, err)
		}
	}
}

func TestHelpErrors(t *testing.T) {
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"nope"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "run 'maestro help'") {
		t.Fatalf("unknown command: %v", err)
	}
	if err := Run(context.Background(), []string{"help", "nope"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "unknown command: nope") {
		t.Fatalf("help of an unknown command: %v", err)
	}
	if err := Run(context.Background(), []string{"help", "task", "extra"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "usage: maestro help") {
		t.Fatalf("help with two arguments: %v", err)
	}
}
