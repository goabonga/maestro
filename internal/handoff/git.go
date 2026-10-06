// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package handoff

import (
	"fmt"
	"os/exec"
	"strings"
)

// VerifyCommits checks an implementation against Git: the listed
// commits must be exactly the first-parent chain from the task base
// to the source head, oldest first, in the given repository.
func VerifyCommits(repository string, envelope Envelope, payload ImplementationPayload) error {
	if payload.SourceSHA != envelope.SourceHeadSHA {
		return invalid("source_sha %s differs from the envelope's source_head_sha %s", payload.SourceSHA, envelope.SourceHeadSHA)
	}
	if _, err := git(repository, "cat-file", "-e", envelope.TaskBaseSHA+"^{commit}"); err != nil {
		return invalid("task base %s is not a commit of the repository", envelope.TaskBaseSHA)
	}
	if _, err := git(repository, "cat-file", "-e", payload.SourceSHA+"^{commit}"); err != nil {
		return invalid("source head %s is not a commit of the repository", payload.SourceSHA)
	}
	if _, err := git(repository, "merge-base", "--is-ancestor", envelope.TaskBaseSHA, payload.SourceSHA); err != nil {
		return invalid("source head %s does not descend from the task base %s", payload.SourceSHA, envelope.TaskBaseSHA)
	}
	listed, err := git(repository, "rev-list", "--first-parent", "--reverse", envelope.TaskBaseSHA+".."+payload.SourceSHA)
	if err != nil {
		return err
	}
	actual := strings.Fields(listed)
	if len(actual) != len(payload.Commits) {
		return invalid("%d commits listed, Git has %d since the task base", len(payload.Commits), len(actual))
	}
	for i, commit := range actual {
		if payload.Commits[i] != commit {
			return invalid("commit %d is %s, Git has %s", i+1, payload.Commits[i], commit)
		}
	}
	return nil
}

// git runs one read-only git command in a repository.
func git(repository string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", repository}, args...)...) // #nosec G204 -- arguments are validated object ids and fixed verbs, never task text
	command.Env = gitEnvironment()
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output)), nil
}
