// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worktree

import (
	"fmt"
	"os"
	"path/filepath"
)

// Project states as reported by State.
const (
	// StateOK means the registered repository is reachable at its path.
	StateOK = "ok"
	// StateMissing means the registered repository path is gone: the
	// repository was moved or deleted, and relocate can fix the former.
	StateMissing = "missing"
)

// State reports whether the registered repository still exists at its
// recorded path.
func (p Project) State() string {
	if info, err := os.Stat(p.UserRepository); err == nil && info.IsDir() {
		return StateOK
	}
	return StateMissing
}

// Lookup returns the project registered under id.
func (s Store) Lookup(id string) (Project, bool, error) {
	projects, err := s.Projects()
	if err != nil {
		return Project{}, false, err
	}
	for _, project := range projects {
		if project.ID == id {
			return project, true, nil
		}
	}
	return Project{}, false, nil
}

// Relocate re-points a registered project at the new path of its moved
// repository, after verifying its identity: the path must be a Git
// repository whose history contains the project's integration head, and
// must not be registered to another project. A path change never
// creates a project implicitly. The caller holds the user lock.
func (s Store) Relocate(id, path string) (Project, error) {
	project, ok, err := s.Lookup(id)
	if err != nil {
		return Project{}, err
	}
	if !ok {
		return Project{}, fmt.Errorf("unknown project: %s", id)
	}
	canonical, err := canonicalGitDir(path)
	if err != nil {
		return Project{}, err
	}
	if canonical == project.UserRepository {
		return project, nil
	}
	if existing, ok, err := s.Find(path); err != nil {
		return Project{}, err
	} else if ok && existing.ID != id {
		return Project{}, fmt.Errorf("%s is already registered to project %s", path, existing.ID)
	}
	head, err := git(project.Repository(), "rev-parse", "refs/heads/maestro/integration")
	if err != nil {
		return Project{}, err
	}
	// The head must be reachable from a branch: a mere object in the
	// store (a stale amend, an unreferenced fetch) is not history.
	branches, err := git(path, "branch", "--all", "--contains", head)
	if err != nil || branches == "" {
		return Project{}, fmt.Errorf("the repository at %s does not contain the project's history (%s)", path, head)
	}
	project.UserRepository = canonical
	if err := s.writeMetadata(project); err != nil {
		return Project{}, err
	}
	return project, nil
}

// writeMetadata rewrites a project's record atomically.
func (s Store) writeMetadata(project Project) error {
	metadata, err := marshalProject(project)
	if err != nil {
		return err
	}
	target := filepath.Join(project.Dir, "project.json")
	temporary := target + ".tmp"
	if err := os.WriteFile(temporary, metadata, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, target)
}
