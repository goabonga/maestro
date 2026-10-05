// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/provision"
)

var chainRuntime = provision.RuntimePaths{"CLAUDE.md", ".claude/", "AGENTS.md", ".mcp.json", ".maestro/"}

func chainTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func chainTestWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	file := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// chainTestCommit writes name with content, commits it with -f so ignored
// runtime paths are staged too, and returns the new commit id.
func chainTestCommit(t *testing.T, dir, name, content, message string) string {
	t.Helper()
	chainTestWrite(t, dir, name, content)
	chainTestGit(t, dir, "add", "-f", name)
	chainTestGit(t, dir, "commit", "-q", "-m", message)
	return chainTestGit(t, dir, "rev-parse", "HEAD")
}

// chainTestRepository creates a repository on branch main with a base
// commit tracking README.md, and returns its directory and the base id.
func chainTestRepository(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	chainTestGit(t, dir, "init", "-q", "-b", "main")
	chainTestGit(t, dir, "config", "user.name", "Test")
	chainTestGit(t, dir, "config", "user.email", "test@example.test")
	chainTestGit(t, dir, "config", "commit.gpgsign", "false")
	base := chainTestCommit(t, dir, "README.md", "project\n", "initial")
	return dir, base
}

func TestValidateSourceChainReturnsCommitsOldestFirst(t *testing.T) {
	dir, base := chainTestRepository(t)
	first := chainTestCommit(t, dir, "main.go", "package main\n", "add main")
	second := chainTestCommit(t, dir, "util.go", "package main\n", "add util")
	correction := chainTestCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n", "fix main")
	chain, err := ValidateSourceChain(dir, base, correction, chainRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{first, second, correction}; !reflect.DeepEqual(chain, want) {
		t.Fatalf("chain %v, want %v", chain, want)
	}
	chain, err = ValidateSourceChain(dir, first, second, chainRuntime)
	if err != nil || !reflect.DeepEqual(chain, []string{second}) {
		t.Fatalf("chain %v, error %v", chain, err)
	}
}

func TestValidateSourceChainAcceptsRuntimePathRemoval(t *testing.T) {
	dir, _ := chainTestRepository(t)
	base := chainTestCommit(t, dir, "CLAUDE.md", "tracked by the project\n", "track instructions")
	chainTestGit(t, dir, "rm", "-q", "CLAUDE.md")
	chainTestGit(t, dir, "commit", "-q", "-m", "drop instructions")
	head := chainTestGit(t, dir, "rev-parse", "HEAD")
	chain, err := ValidateSourceChain(dir, base, head, chainRuntime)
	if err != nil || !reflect.DeepEqual(chain, []string{head}) {
		t.Fatalf("chain %v, error %v", chain, err)
	}
}

func TestValidateSourceChainRefusesMergeCommit(t *testing.T) {
	dir, base := chainTestRepository(t)
	chainTestCommit(t, dir, "main.go", "package main\n", "add main")
	chainTestGit(t, dir, "checkout", "-q", "-b", "side", base)
	chainTestCommit(t, dir, "side.go", "package main\n", "add side")
	chainTestGit(t, dir, "checkout", "-q", "main")
	chainTestGit(t, dir, "merge", "-q", "--no-ff", "--no-edit", "side")
	merge := chainTestGit(t, dir, "rev-parse", "HEAD")
	after := chainTestCommit(t, dir, "after.go", "package main\n", "add after")
	_, err := ValidateSourceChain(dir, base, after, chainRuntime)
	if !errors.Is(err, ErrSourceChain) || !strings.Contains(err.Error(), merge) || !strings.Contains(err.Error(), "merge commit") {
		t.Fatalf("error %v, want a merge commit refusal naming %s", err, merge)
	}
}

func TestValidateSourceChainRefusesBaseReachedThroughMerge(t *testing.T) {
	dir, root := chainTestRepository(t)
	chainTestGit(t, dir, "checkout", "-q", "-b", "base-line", root)
	base := chainTestCommit(t, dir, "base.go", "package main\n", "declared base")
	chainTestGit(t, dir, "checkout", "-q", "main")
	chainTestCommit(t, dir, "foreign.go", "package main\n", "foreign")
	chainTestGit(t, dir, "merge", "-q", "--no-ff", "--no-edit", "base-line")
	merge := chainTestGit(t, dir, "rev-parse", "HEAD")
	_, err := ValidateSourceChain(dir, base, merge, chainRuntime)
	if !errors.Is(err, ErrSourceChain) || !strings.Contains(err.Error(), merge) {
		t.Fatalf("error %v, want a refusal naming %s", err, merge)
	}
}

func TestValidateSourceChainRefusesHeadNotDescendingFromBase(t *testing.T) {
	dir, root := chainTestRepository(t)
	base := chainTestCommit(t, dir, "main.go", "package main\n", "declared base")
	chainTestGit(t, dir, "checkout", "-q", "-b", "other", root)
	head := chainTestCommit(t, dir, "other.go", "package main\n", "sibling")
	for _, pair := range [][2]string{{base, head}, {base, root}} {
		_, err := ValidateSourceChain(dir, pair[0], pair[1], chainRuntime)
		if !errors.Is(err, ErrSourceChain) || !strings.Contains(err.Error(), pair[1]) || !strings.Contains(err.Error(), "does not descend") {
			t.Fatalf("base %s head %s: error %v", pair[0], pair[1], err)
		}
	}
}

func TestValidateSourceChainRefusesEmptyChain(t *testing.T) {
	dir, base := chainTestRepository(t)
	if _, err := ValidateSourceChain(dir, base, base, chainRuntime); !errors.Is(err, ErrEmptySourceChain) {
		t.Fatalf("error %v", err)
	}
}

func TestValidateSourceChainRefusesRuntimePathChanges(t *testing.T) {
	dir, base := chainTestRepository(t)
	chainTestCommit(t, dir, "main.go", "package main\n", "add main")
	added := chainTestCommit(t, dir, ".claude/settings.json", "{}\n", "add settings")
	chainTestGit(t, dir, "rm", "-q", "-r", ".claude")
	chainTestGit(t, dir, "commit", "-q", "-m", "remove settings")
	head := chainTestGit(t, dir, "rev-parse", "HEAD")
	_, err := ValidateSourceChain(dir, base, head, chainRuntime)
	if !errors.Is(err, ErrSourceChain) || !errors.Is(err, provision.ErrRuntimePath) || !strings.Contains(err.Error(), added) {
		t.Fatalf("error %v, want a runtime path refusal naming %s", err, added)
	}
}

func TestValidateSourceChainRefusesInvalidRevisions(t *testing.T) {
	dir, base := chainTestRepository(t)
	head := chainTestCommit(t, dir, "main.go", "package main\n", "add main")
	tree := chainTestGit(t, dir, "rev-parse", "HEAD^{tree}")
	for _, id := range []string{"", "HEAD", "main", "--all", base[:12], strings.ToUpper(base), strings.Repeat("0", 40), tree, base + "\n"} {
		if _, err := ValidateSourceChain(dir, id, head, chainRuntime); !errors.Is(err, ErrSourceRevision) {
			t.Fatalf("base %q: error %v", id, err)
		}
		if _, err := ValidateSourceChain(dir, base, id, chainRuntime); !errors.Is(err, ErrSourceRevision) {
			t.Fatalf("head %q: error %v", id, err)
		}
	}
}

func TestValidateSourceChainRefusesInvalidRuntimePaths(t *testing.T) {
	dir, base := chainTestRepository(t)
	head := chainTestCommit(t, dir, "main.go", "package main\n", "add main")
	if _, err := ValidateSourceChain(dir, base, head, provision.RuntimePaths{"../outside"}); !errors.Is(err, provision.ErrInvalidRuntimePath) {
		t.Fatalf("error %v", err)
	}
}
