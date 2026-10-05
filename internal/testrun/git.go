// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package testrun

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// objectID is a full SHA-1 or SHA-256 object name.
var objectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// maxPaths bounds each reported path list.
const maxPaths = 1000

// checkout clones repository into clone through Git's transport (never
// hard links, so the clone shares no file with it) and checks out sha
// detached. The SHA is fetched explicitly when no branch or tag reaches
// it, as for a candidate held by an operation reference.
func checkout(repository, sha, clone string) error {
	if !objectID.MatchString(sha) {
		return fmt.Errorf("%w: %q is not a full object id", ErrCheckout, sha)
	}
	if _, err := os.Lstat(clone); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s already exists", ErrCheckout, clone)
	}
	if _, err := git("", "clone", "--quiet", "--no-local", "--no-checkout", "--", repository, clone); err != nil {
		return fmt.Errorf("%w: %w", ErrCheckout, err)
	}
	if _, err := git(clone, "cat-file", "-e", sha+"^{commit}"); err != nil {
		if _, err := git(clone, "fetch", "--quiet", "--no-tags", "origin", sha); err != nil {
			return fmt.Errorf("%w: %s is not a commit of %s: %w", ErrCheckout, sha, repository, err)
		}
	}
	if _, err := git(clone, "checkout", "--quiet", "--detach", sha); err != nil {
		return fmt.Errorf("%w: %w", ErrCheckout, err)
	}
	head, err := git(clone, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCheckout, err)
	}
	if strings.TrimSpace(head) != sha {
		return fmt.Errorf("%w: HEAD is %s, expected %s", ErrCheckout, strings.TrimSpace(head), sha)
	}
	return nil
}

// indexEntries lists the index: mode, object and stage by path.
func indexEntries(clone string) (map[string]string, error) {
	listing, err := git(clone, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	entries := map[string]string{}
	for _, record := range strings.Split(listing, "\x00") {
		meta, path, ok := strings.Cut(record, "\t")
		if ok {
			entries[path] = meta
		}
	}
	return entries, nil
}

// inspect compares the clone with the tested revision after a command:
// a moved HEAD, a changed index entry or a modified tracked file is a
// change; untracked and ignored files are reported separately.
func inspect(clone, sha string, baseline map[string]string) ([]string, []string, error) {
	changed := map[string]bool{}
	head, err := git(clone, "rev-parse", "HEAD")
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(head) != sha {
		changed["HEAD"] = true
	}
	entries, err := indexEntries(clone)
	if err != nil {
		return nil, nil, err
	}
	for path, meta := range entries {
		if baseline[path] != meta {
			changed[path] = true
		}
	}
	for path := range baseline {
		if _, ok := entries[path]; !ok {
			changed[path] = true
		}
	}
	status, err := git(clone, "status", "--porcelain=v1", "-z", "--no-renames",
		"--untracked-files=normal", "--ignored=traditional")
	if err != nil {
		return nil, nil, err
	}
	var untracked []string
	for _, record := range strings.Split(status, "\x00") {
		if len(record) < 4 {
			continue
		}
		code, path := record[:2], record[3:]
		if code == "??" || code == "!!" {
			untracked = append(untracked, path)
			continue
		}
		changed[path] = true
	}
	changes := make([]string, 0, len(changed))
	for path := range changed {
		changes = append(changes, path)
	}
	sort.Strings(changes)
	sort.Strings(untracked)
	return bound(changes), bound(untracked), nil
}

// bound caps a path list.
func bound(paths []string) []string {
	if len(paths) > maxPaths {
		return paths[:maxPaths]
	}
	return paths
}

// git runs one Git command with a controlled configuration: no system
// or global configuration, no hooks, no file-system monitor and no
// optional locks, so nothing written by a test can run in the daemon.
func git(dir string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false",
		"--no-optional-locks",
	}, args...)
	cmd := exec.Command("git", full...) // #nosec G204 -- fixed git verbs; the revision is a validated object id and paths come from Maestro
	cmd.Dir = dir
	// Only the search path is inherited: no GIT_DIR or other variable of
	// the daemon's environment redirects these commands.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
