// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
		fail bool
	}{
		{[]string{"--version"}, "maestro 0.0.0\n", false},
		{[]string{"--help"}, "Commands:", false},
		{[]string{"-h"}, "Commands:", false},
		{[]string{"help"}, "Commands:", false},
		{nil, "Commands:", false},
		{[]string{"create"}, "", true},
		{[]string{"--unknown"}, "", true},
	} {
		var output bytes.Buffer
		err := Run(context.Background(), tc.args, &output, "0.0.0")
		if (err != nil) != tc.fail || !strings.Contains(output.String(), tc.want) {
			t.Fatalf("args %v: output %q, error %v", tc.args, output.String(), err)
		}
	}
}
