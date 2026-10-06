// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package tui

import (
	"os"
	"strings"
	"testing"
)

// TestMain runs the tests without any GIT_* variable of the caller: a
// suite started from a git-spawned command (rebase --exec, a hook)
// inherits GIT_DIR and its siblings, which would point every git command
// of the tests at the caller's repository instead of a temporary one.
// The user's global and system Git configuration is ignored as well.
func TestMain(m *testing.M) {
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, "GIT_") {
			_ = os.Unsetenv(name)
		}
	}
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	_ = os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Exit(m.Run())
}
