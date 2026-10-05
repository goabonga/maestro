// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestTUIOpensOnTheProjectAndQuits(t *testing.T) {
	_, socket, _, projectID := taskDaemon(t)
	var output bytes.Buffer
	err := tuiCommand(context.Background(), []string{"--socket", socket, "--project", projectID},
		strings.NewReader("q"), &output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "maestro · tasks of "+projectID) {
		t.Fatalf("output %q", output.String())
	}
}

func TestTUIRefusesBadArguments(t *testing.T) {
	for _, args := range [][]string{{"extra"}, {"--interval", "1ms"}, {"--unknown"}} {
		var output bytes.Buffer
		if err := tuiCommand(context.Background(), args, strings.NewReader(""), &output); err == nil {
			t.Fatalf("args %v accepted", args)
		}
	}
}

func TestTUIHelp(t *testing.T) {
	var output bytes.Buffer
	if err := tuiCommand(context.Background(), []string{"--help"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-socket", "-project", "-interval"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("help misses %s: %s", want, output.String())
		}
	}
}
