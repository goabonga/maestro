// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sleeper starts a fixture process working in dir and stops it at the
// end of the test.
func sleeper(t *testing.T, dir string) int {
	t.Helper()
	command := exec.Command("sleep", "30")
	command.Dir = dir
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	return command.Process.Pid
}

func TestSurvivorsFindAProcessWorkingInTheDirectory(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	inside, below := sleeper(t, dir), sleeper(t, nested)
	pids, err := Survivors(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pids, inside) || !slices.Contains(pids, below) {
		t.Fatalf("survivors %v miss %d or %d", pids, inside, below)
	}
	if !slices.IsSorted(pids) {
		t.Fatalf("survivors %v are not sorted", pids)
	}
}

func TestSurvivorsIgnoreASiblingSharingThePrefix(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "worker")
	sibling := filepath.Join(parent, "worker-other")
	for _, path := range []string{dir, sibling} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	outside := sleeper(t, sibling)
	pids, err := Survivors(dir)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pids, outside) {
		t.Fatalf("a process of %s is a survivor of %s: %v", sibling, dir, pids)
	}
}

func TestSurvivorsResolveASymlinkedDirectory(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "real")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	inside := sleeper(t, dir)
	pids, err := Survivors(link)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pids, inside) {
		t.Fatalf("survivors %v of the symlink miss %d", pids, inside)
	}
}

func TestSurvivorsOfAMissingDirectoryAreNone(t *testing.T) {
	pids, err := Survivors(filepath.Join(t.TempDir(), "missing"))
	if err != nil || len(pids) != 0 {
		t.Fatalf("survivors of a missing directory: %v, %v", pids, err)
	}
}

func TestSurvivorsFindAProcessInARemovedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := sleeper(t, dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	pids, err := Survivors(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pids, inside) {
		t.Fatalf("survivors %v of the removed directory miss %d", pids, inside)
	}
}

func TestSurvivorsFindAProcessInARecreatedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := sleeper(t, dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pids, err := Survivors(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pids, inside) {
		t.Fatalf("survivors %v of the recreated directory miss %d", pids, inside)
	}
}

func TestSurvivorsLeaveTheDaemonOut(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	pids, err := Survivors(dir)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pids, os.Getpid()) {
		t.Fatalf("the daemon is its own survivor: %v", pids)
	}
}

func TestSurvivorsSkipUnreadableEntries(t *testing.T) {
	root := t.TempDir()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"self", "12", "not-a-pid"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "34"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(root, "34", "cwd")); err != nil {
		t.Fatal(err)
	}
	pids, err := survivorsIn(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pids, []int{34}) {
		t.Fatalf("survivors from the fixture table: %v, want [34]", pids)
	}
}

func TestSurvivorsFailOnAWorkingDirectoryThatCannotBeRead(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "56"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A cwd entry that is not a link cannot be read as one.
	if err := os.WriteFile(filepath.Join(root, "56", "cwd"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := survivorsIn(root, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "inspect process 56") {
		t.Fatalf("an unreadable working directory: %v", err)
	}
}

func TestSurvivorsSkipAProcessTheDaemonMayNotInspect(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root inspects every process")
	}
	root := t.TempDir()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	process := filepath.Join(root, "56")
	if err := os.Mkdir(process, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(process, "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(process, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(process, 0o700) })
	pids, err := survivorsIn(root, dir)
	if err != nil || len(pids) != 0 {
		t.Fatalf("a process the daemon may not inspect: %v, %v", pids, err)
	}
}

func TestSurvivorsNeedADirectory(t *testing.T) {
	if _, err := Survivors(""); err == nil {
		t.Fatal("a survivor search without a directory is accepted")
	}
}
