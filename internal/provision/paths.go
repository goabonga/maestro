// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package provision

import (
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"
)

// ErrRuntimePath reports an addition or a modification of a runtime path,
// in the index of a worktree or in an incoming commit.
var ErrRuntimePath = errors.New("runtime path change refused")

// ErrInvalidRuntimePath reports a runtime path that is not a clean
// repository-relative path.
var ErrInvalidRuntimePath = errors.New("invalid runtime path")

// ErrInvalidRevision reports a base or head that does not name a commit.
var ErrInvalidRevision = errors.New("invalid revision")

// RuntimePaths is a list of repository-relative runtime paths. An entry
// names a file ("CLAUDE.md") or a directory (".claude/"); either form
// covers the path itself and everything below it. Paths are compared
// exactly, never interpreted as patterns.
type RuntimePaths []string

// Validate checks that every entry is a clean, relative path inside the
// repository.
func (r RuntimePaths) Validate() error {
	for _, entry := range r {
		name := strings.TrimSuffix(entry, "/")
		switch {
		case name == "", name == ".", strings.HasPrefix(name, "/"),
			strings.Contains(name, "\\"), strings.ContainsRune(name, 0),
			path.Clean(name) != name, name == "..", strings.HasPrefix(name, "../"):
			return fmt.Errorf("%w: %q", ErrInvalidRuntimePath, entry)
		}
	}
	return nil
}

// Covers reports whether a repository-relative path is a runtime path or
// lies below one.
func (r RuntimePaths) Covers(name string) bool {
	for _, entry := range r {
		root := strings.TrimSuffix(entry, "/")
		if name == root || strings.HasPrefix(name, root+"/") {
			return true
		}
	}
	return false
}

// CheckIndex refuses any runtime path added or modified in the index of
// the worktree at dir relative to the task base commit, including a path
// staged with git add -f. A runtime path already tracked at base and left
// unchanged is not a violation, and neither is its removal.
func CheckIndex(dir, base string, runtime RuntimePaths) error {
	if err := runtime.Validate(); err != nil {
		return err
	}
	baseCommit, err := resolveCommit(dir, base)
	if err != nil {
		return err
	}
	out, err := git(dir, "diff-index", "--cached", "--no-renames", "--name-status", "-z", baseCommit, "--")
	if err != nil {
		return err
	}
	changes, err := parseNameStatus(out)
	if err != nil {
		return err
	}
	var violations []error
	for _, change := range changes {
		if change.status != 'D' && runtime.Covers(change.path) {
			violations = append(violations, fmt.Errorf("%w: index %s %s", ErrRuntimePath, verb(change.status), change.path))
		}
	}
	return errors.Join(violations...)
}

// CheckCommits refuses every commit of base..head that adds or modifies a
// runtime path. Each commit is compared with its first parent, so a path
// added then removed by a later commit is still refused. A runtime path
// removed by a commit is not a violation.
func CheckCommits(dir, base, head string, runtime RuntimePaths) error {
	if err := runtime.Validate(); err != nil {
		return err
	}
	baseCommit, err := resolveCommit(dir, base)
	if err != nil {
		return err
	}
	headCommit, err := resolveCommit(dir, head)
	if err != nil {
		return err
	}
	out, err := git(dir, "rev-list", "--reverse", "--parents", baseCommit+".."+headCommit, "--")
	if err != nil {
		return err
	}
	var violations []error
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		commit := fields[0]
		args := []string{"diff-tree", "-r", "--no-commit-id", "--no-renames", "--name-status", "-z"}
		if len(fields) > 1 {
			args = append(args, fields[1], commit)
		} else {
			args = append(args, "--root", commit)
		}
		diff, err := git(dir, append(args, "--")...)
		if err != nil {
			return err
		}
		changes, err := parseNameStatus(diff)
		if err != nil {
			return err
		}
		for _, change := range changes {
			if change.status != 'D' && runtime.Covers(change.path) {
				violations = append(violations, fmt.Errorf("%w: commit %s %s %s", ErrRuntimePath, commit, verb(change.status), change.path))
			}
		}
	}
	return errors.Join(violations...)
}

// change is one entry of a NUL-separated name-status listing.
type change struct {
	status byte
	path   string
}

// parseNameStatus parses the output of --name-status -z without renames:
// alternating status and path fields.
func parseNameStatus(out string) ([]change, error) {
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if len(fields) == 1 && fields[0] == "" {
		return nil, nil
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("unexpected name-status output: %q", out)
	}
	changes := make([]change, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		if fields[i] == "" {
			return nil, fmt.Errorf("unexpected name-status output: %q", out)
		}
		changes = append(changes, change{status: fields[i][0], path: fields[i+1]})
	}
	return changes, nil
}

// verb names a name-status letter in an error message.
func verb(status byte) string {
	switch status {
	case 'A':
		return "adds"
	case 'M':
		return "modifies"
	case 'T':
		return "changes the type of"
	case 'U':
		return "leaves unmerged"
	default:
		return "changes"
	}
}

// resolveCommit resolves a revision to a full commit hash. A revision
// that could be read as an option is refused before reaching Git.
func resolveCommit(dir, rev string) (string, error) {
	if rev == "" || strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, "\x00\n") {
		return "", fmt.Errorf("%w: %q", ErrInvalidRevision, rev)
	}
	hash, err := git(dir, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", ErrInvalidRevision, rev, err)
	}
	return strings.TrimSpace(hash), nil
}

// git runs a read-only Git command in dir and returns its standard output
// untrimmed, so NUL-separated listings keep their exact paths.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...) // #nosec G204 -- fixed git verbs; revisions are resolved to hashes and option-like values refused
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
