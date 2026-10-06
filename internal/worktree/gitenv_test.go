// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
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

// isolationChild marks the test binary re-executed by
// TestGitHelpersIgnoreAnInheritedGitDir.
const isolationChild = "MAESTRO_TEST_GIT_ISOLATION_CHILD"

func TestGitHelpersIgnoreAnInheritedGitDir(t *testing.T) {
	victim := t.TempDir()
	run(t, victim, "git", "init", "-q", "-b", "trunk")
	run(t, victim, "git", "config", "user.name", "Victim")
	run(t, victim, "git", "config", "user.email", "victim@example.test")
	run(t, victim, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(victim, "victim.txt"), []byte("victim\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, victim, "git", "add", "victim.txt")
	run(t, victim, "git", "commit", "-q", "-m", "feat: victim")
	configPath := filepath.Join(victim, ".git", "config")
	config, err := os.ReadFile(configPath) // #nosec G304 -- the test's own temporary repository
	if err != nil {
		t.Fatal(err)
	}
	refs := run(t, victim, "git", "for-each-ref")

	// The child runs the package's git helpers as a git-spawned command
	// would: with GIT_DIR, GIT_WORK_TREE and GIT_INDEX_FILE naming the
	// victim repository.
	child := exec.Command(os.Args[0], "-test.run=^TestGitIsolationChild$", "-test.count=1") // #nosec G204 -- the test binary itself
	child.Env = append(os.Environ(), isolationChild+"=1",
		"GIT_DIR="+filepath.Join(victim, ".git"),
		"GIT_WORK_TREE="+victim,
		"GIT_INDEX_FILE="+filepath.Join(victim, ".git", "index"))
	if output, err := child.CombinedOutput(); err != nil || !strings.Contains(string(output), "PASS") {
		t.Fatalf("child: %v: %s", err, output)
	}

	after, err := os.ReadFile(configPath) // #nosec G304 -- the test's own temporary repository
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(config) {
		t.Fatalf("victim configuration changed:\n%s", after)
	}
	if got := run(t, victim, "git", "for-each-ref"); got != refs {
		t.Fatalf("victim references changed:\n%s", got)
	}
	if run(t, victim, "git", "status", "--porcelain") != "" {
		t.Fatal("victim worktree changed")
	}
}

// TestGitIsolationChild only runs inside TestGitHelpersIgnoreAnInheritedGitDir.
func TestGitIsolationChild(t *testing.T) {
	if os.Getenv(isolationChild) != "1" {
		t.Skip("runs only as the child of TestGitHelpersIgnoreAnInheritedGitDir")
	}
	user := userRepository(t)
	if _, _, err := (Store{Base: t.TempDir()}).Init(user); err != nil {
		t.Fatal(err)
	}
}
