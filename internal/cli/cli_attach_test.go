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
	err := Run(context.Background(), []string{"attach", "s1", "--socket", shortSocket(t)}, &output, "0.0.0")
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("error %v", err)
	}
}
