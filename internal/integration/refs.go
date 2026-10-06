// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package integration

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/goabonga/maestro/internal/worktree"
)

// Errors of the durable result proof.
var (
	// ErrInvalidResult reports a result commit that is not the one the
	// operation's frozen inputs describe.
	ErrInvalidResult = errors.New("invalid operation result")
	// ErrResultRefConflict reports a result reference that already
	// points at another commit: it is never overwritten.
	ErrResultRefConflict = errors.New("the operation result reference points at another commit")
)

// ResultRef returns the durable reference of an operation's result:
// refs/maestro/operations/<id>/result.
func ResultRef(id string) (string, error) {
	if !operationID.MatchString(id) {
		return "", fmt.Errorf("%w: %q is not an operation id", ErrInvalid, id)
	}
	return resultRefPrefix + id + "/result", nil
}

// Expected is what a result commit must be: a commit whose only parent
// is Base, whose tree is Tree and whose message, author and committer
// are the frozen Metadata.
type Expected struct {
	Base     string
	Tree     string
	Metadata CommitMetadata
}

// expected returns what the result of an operation must be.
func (op Operation) expected() (Expected, error) {
	if op.CommitMetadata == nil || op.CandidateTreeSHA == "" {
		return Expected{}, fmt.Errorf("%w: operation %s has no frozen tree and metadata", ErrGuard, op.ID)
	}
	return Expected{Base: op.IntegrationBaseSHA, Tree: op.CandidateTreeSHA, Metadata: *op.CommitMetadata}, nil
}

// BuildResult creates, in repository, the commit of tree with base as
// its only parent and the frozen metadata, and returns its id. The
// same inputs always build the same commit.
func BuildResult(repository string, want Expected) (string, error) {
	if !shaID.MatchString(want.Base) || !shaID.MatchString(want.Tree) {
		return "", fmt.Errorf("%w: base %q and tree %q must be object ids", ErrInvalid, want.Base, want.Tree)
	}
	metadata, err := want.Metadata.normalize()
	if err != nil {
		return "", err
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + metadata.Author.Name, "GIT_AUTHOR_EMAIL=" + metadata.Author.Email,
		"GIT_AUTHOR_DATE=" + gitDate(metadata.Author),
		"GIT_COMMITTER_NAME=" + metadata.Committer.Name, "GIT_COMMITTER_EMAIL=" + metadata.Committer.Email,
		"GIT_COMMITTER_DATE=" + gitDate(metadata.Committer),
	}
	sha, err := repoGit(repository, metadata.Message, env, "commit-tree", want.Tree, "-p", want.Base)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(sha), nil
}

// CreateResultRef creates the result reference of operation id at sha
// in repository, atomically and only if it does not exist yet: the
// update carries the null object id as the expected old value. A
// reference already at sha is accepted, so a creation interrupted
// before its journal entry can be repeated; one at another commit fails
// with ErrResultRefConflict and is left untouched.
func CreateResultRef(repository, id, sha string) error {
	ref, err := ResultRef(id)
	if err != nil {
		return err
	}
	if !shaID.MatchString(sha) {
		return fmt.Errorf("%w: result %q is not an object id", ErrInvalid, sha)
	}
	_, createErr := repoGit(repository, "", nil, "update-ref", ref, sha, strings.Repeat("0", len(sha)))
	if createErr == nil {
		return nil
	}
	current, ok, err := ReadResultRef(repository, id)
	switch {
	case err != nil:
		return err
	case !ok:
		return createErr
	case current != sha:
		return fmt.Errorf("%w: %s is at %s, not %s", ErrResultRefConflict, ref, current, sha)
	}
	return nil
}

// ReadResultRef returns the commit the result reference of operation
// id points at in repository, and false when it does not exist.
func ReadResultRef(repository, id string) (string, bool, error) {
	ref, err := ResultRef(id)
	if err != nil {
		return "", false, err
	}
	out, err := repoGit(repository, "", nil, "for-each-ref", "--format=%(objectname)", ref)
	if err != nil {
		return "", false, err
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return "", false, nil
	}
	return sha, true, nil
}

// ValidateResult checks that sha, in repository, is exactly the
// expected result: a commit with the expected base as its only parent,
// the expected tree, and the frozen message, author and committer, with
// no other header such as a signature or an encoding. The presence of
// source commits in its history proves nothing and is not looked at.
func ValidateResult(repository, sha string, want Expected) error {
	if !shaID.MatchString(sha) {
		return fmt.Errorf("%w: %q is not an object id", ErrInvalidResult, sha)
	}
	metadata, err := want.Metadata.normalize()
	if err != nil {
		return err
	}
	raw, err := repoGit(repository, "", nil, "cat-file", "commit", sha)
	if err != nil {
		return fmt.Errorf("%w: %s is not a commit: %w", ErrInvalidResult, sha, err)
	}
	header, message, ok := strings.Cut(raw, "\n\n")
	if !ok {
		return fmt.Errorf("%w: %s has no message", ErrInvalidResult, sha)
	}
	var trees, parents []string
	var author, committer string
	for _, line := range strings.Split(header, "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "tree":
			trees = append(trees, value)
		case "parent":
			parents = append(parents, value)
		case "author":
			if author != "" {
				return fmt.Errorf("%w: %s has two authors", ErrInvalidResult, sha)
			}
			author = value
		case "committer":
			if committer != "" {
				return fmt.Errorf("%w: %s has two committers", ErrInvalidResult, sha)
			}
			committer = value
		default:
			return fmt.Errorf("%w: %s carries an unexpected header %q", ErrInvalidResult, sha, key)
		}
	}
	switch {
	case len(trees) != 1 || trees[0] != want.Tree:
		return fmt.Errorf("%w: %s has tree %v, expected %s", ErrInvalidResult, sha, trees, want.Tree)
	case len(parents) != 1 || parents[0] != want.Base:
		return fmt.Errorf("%w: %s has parents %v, expected only %s", ErrInvalidResult, sha, parents, want.Base)
	case author != gitIdent(metadata.Author):
		return fmt.Errorf("%w: %s has author %q, expected %q", ErrInvalidResult, sha, author, gitIdent(metadata.Author))
	case committer != gitIdent(metadata.Committer):
		return fmt.Errorf("%w: %s has committer %q, expected %q", ErrInvalidResult, sha, committer, gitIdent(metadata.Committer))
	case message != metadata.Message:
		return fmt.Errorf("%w: %s does not carry the frozen message", ErrInvalidResult, sha)
	}
	return nil
}

// ProveResult reads the durable result of an integration in the
// project's canonical repository and validates it against the
// operation's frozen inputs. It returns false when no result reference
// exists, and ErrInvalidResult when the reference points at another
// commit than the operation describes.
func ProveResult(project worktree.Project, op Operation) (string, bool, error) {
	if op.Type != Integrate {
		return "", false, fmt.Errorf("%w: only an integration has a result reference", ErrInvalid)
	}
	sha, ok, err := ReadResultRef(project.Repository(), op.ID)
	if err != nil || !ok {
		return "", false, err
	}
	want, err := op.expected()
	if err != nil {
		return "", false, err
	}
	if err := ValidateResult(project.Repository(), sha, want); err != nil {
		return "", false, err
	}
	return sha, true, nil
}

// ApplyIntegration records the result of a started integration. The
// result commit is first validated against the operation's frozen base,
// tree and metadata, then its durable reference is created in the
// project's canonical repository and validated again, and only then is
// APPLIED stored with result_sha. A crash between the reference and the
// journal leaves a valid reference that ProveResult finds and that this
// call accepts again with the same result; it never leaves a result in
// the journal without its reference.
func (s Store) ApplyIntegration(project worktree.Project, id, resultSHA string) (Operation, error) {
	op, err := s.Get(id)
	if err != nil {
		return Operation{}, err
	}
	if op.Type != Integrate {
		return op, fmt.Errorf("%w: operation %s is a %s", ErrInvalid, id, op.Type)
	}
	if op.State != Started {
		return op, fmt.Errorf("%w: a result is applied in %s, not %s", ErrTransition, Started, op.State)
	}
	want, err := op.expected()
	if err != nil {
		return op, err
	}
	if err := ValidateResult(project.Repository(), resultSHA, want); err != nil {
		return op, err
	}
	if err := CreateResultRef(project.Repository(), id, resultSHA); err != nil {
		return op, err
	}
	crashHook(crashApplyRef)
	proven, ok, err := ProveResult(project, op)
	switch {
	case err != nil:
		return op, err
	case !ok || proven != resultSHA:
		return op, fmt.Errorf("%w: the result reference of %s does not hold %s", ErrInvalidResult, id, resultSHA)
	}
	return s.recordResult(id, resultSHA)
}

// gitIdent renders a signature as Git records it in a commit header.
func gitIdent(s Signature) string {
	return fmt.Sprintf("%s <%s> %s", s.Name, s.Email, gitDate(s))
}

// gitDate renders an instant in Git's internal date format: seconds
// since the epoch and the time-zone offset.
func gitDate(s Signature) string {
	return fmt.Sprintf("%d %s", s.When.Unix(), s.When.Format("-0700"))
}

// repoGit runs one Git command against the bare repository with a
// controlled configuration: no system or global configuration, no
// hooks, no signing, and objects and references synced to disk before
// the command returns. Arguments are fixed verbs, validated object ids
// and reference names built by Maestro. input, when not empty, is the
// standard input; env adds variables to the minimal environment.
func repoGit(repository, input string, env []string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "commit.gpgSign=false",
		"-c", "core.fsync=committed",
		"-c", "core.fsyncMethod=fsync",
	}, args...)
	cmd := exec.Command("git", full...) // #nosec G204 -- fixed git verbs; object ids and reference names are validated by Maestro
	// Only the search path is inherited: no variable of the daemon's
	// environment redirects these commands to another repository.
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"), "GIT_DIR=" + repository,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
	}, env...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
