// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestAttachCommandErrors(t *testing.T) {
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"attach"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "usage: maestro attach") {
		t.Fatalf("error %v", err)
	}
	if err := Run(context.Background(), []string{"attach", "claude-01", "extra"}, &output, "0.0.0"); err == nil || !strings.Contains(err.Error(), "usage: maestro attach <worker>") {
		t.Fatalf("error %v", err)
	}
	err := Run(context.Background(), []string{"attach", "claude-01", "--project", "p1", "--socket", shortSocket(t)}, &output, "0.0.0")
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("error %v", err)
	}
}

func TestAttachCommandReportsTheDaemonsRefusal(t *testing.T) {
	_, socket, _, project := workerDaemon(t)
	var output bytes.Buffer
	err := Run(context.Background(), []string{"attach", "claude-01", "--project", project.ID, "--socket", socket}, &output, "0.0.0")
	if err == nil || err.Error() != "this daemon attaches no worker" {
		t.Fatalf("error %v", err)
	}
}
