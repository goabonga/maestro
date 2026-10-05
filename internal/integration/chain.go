// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/goabonga/maestro/internal/provision"
)

// ErrSourceChain reports a source chain that cannot be integrated: a
// merge commit, a commit that does not descend from the task base, a
// commit foreign to the chain or a runtime path change.
var ErrSourceChain = errors.New("invalid source chain")

// ErrEmptySourceChain reports a source head equal to the task base: the
// task has no commit to integrate.
var ErrEmptySourceChain = errors.New("empty source chain")

// ErrSourceRevision reports a task base or source head that is not the
// full object id of a commit of the repository.
var ErrSourceRevision = errors.New("invalid source revision")

// ValidateSourceChain checks the source chain of a task before
// integration and returns its commits, oldest first, for the candidate
// builder. base is the task base and head the source head, both full
// commit ids.
//
// The commits of base..head must form a linear first-parent chain whose
// oldest commit has base as its only parent, so the range holds no
// commit foreign to that chain: a merge commit and a chain that does not
// reach base are refused with ErrSourceChain. A commit that adds or modifies
// one of the runtime paths is refused as well, wrapping both
// ErrSourceChain and provision.ErrRuntimePath. Later commits correcting
// earlier ones are part of the chain and allowed. Every error names the
// offending commit.
func ValidateSourceChain(repository, base, head string, runtime provision.RuntimePaths) ([]string, error) {
	if err := runtime.Validate(); err != nil {
		return nil, err
	}
	for _, id := range []string{base, head} {
		if err := chainCommitExists(repository, id); err != nil {
			return nil, err
		}
	}
	if base == head {
		return nil, fmt.Errorf("%w: source head %s is the task base", ErrEmptySourceChain, head)
	}
	if _, err := chainGit(repository, "merge-base", "--is-ancestor", base, head); err != nil {
		return nil, fmt.Errorf("%w: source head %s does not descend from the task base %s", ErrSourceChain, head, base)
	}
	listed, err := chainGit(repository, "rev-list", "--first-parent", "--parents", base+".."+head, "--")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(listed), "\n")
	chain := make([]string, len(lines))
	for i, line := range lines {
		fields := strings.Fields(line)
		commit := fields[0]
		if len(fields) > 2 {
			return nil, fmt.Errorf("%w: commit %s is a merge commit", ErrSourceChain, commit)
		}
		// The first-parent walk runs newest first and stops at base, so
		// without merges the oldest commit sits on base. Checked anyway:
		// the chain handed to the candidate builder must start there.
		if i == len(lines)-1 && (len(fields) < 2 || fields[1] != base) {
			return nil, fmt.Errorf("%w: commit %s does not have the task base %s as parent", ErrSourceChain, commit, base)
		}
		chain[len(lines)-1-i] = commit
	}
	// Every commit of the chain has a single parent and the oldest sits on
	// base, so base..head holds exactly the chain: no foreign commit can
	// be reachable from head without passing through base.
	if err := provision.CheckCommits(repository, base, head, runtime); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceChain, err)
	}
	return chain, nil
}

// chainCommitExists checks that id is the full lowercase object id of a
// commit of the repository, so only validated ids ever reach Git.
func chainCommitExists(repository, id string) error {
	if !chainObjectID(id) {
		return fmt.Errorf("%w: %q is not a full object id", ErrSourceRevision, id)
	}
	resolved, err := chainGit(repository, "rev-parse", "--verify", "--quiet", "--end-of-options", id+"^{commit}")
	if err != nil || strings.TrimSpace(resolved) != id {
		return fmt.Errorf("%w: %s is not a commit of the repository", ErrSourceRevision, id)
	}
	return nil
}

// chainObjectID reports whether id is a full SHA-1 or SHA-256 object id
// in lowercase hexadecimal.
func chainObjectID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// chainGit runs one read-only Git command in repository with a controlled
// configuration: no system or global configuration, no hooks and no
// optional locks, and only the search path inherited from the daemon.
func chainGit(repository string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"--no-optional-locks",
	}, args...)
	cmd := exec.Command("git", full...) // #nosec G204 -- fixed git verbs; revisions are validated full object ids
	cmd.Dir = repository
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
