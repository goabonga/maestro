// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package worktree owns Maestro's Git storage: the per-project data
// directory and the private canonical repository imported from the
// user's repository. The user's repository is never modified.
package worktree

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Store is the base data directory holding every project.
type Store struct {
	Base string
}

// DefaultStore resolves the data directory: $MAESTRO_DATA_HOME, else
// $XDG_DATA_HOME/maestro, else ~/.local/share/maestro.
func DefaultStore() (Store, error) {
	if dir := os.Getenv("MAESTRO_DATA_HOME"); dir != "" {
		return Store{Base: dir}, nil
	}
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return Store{Base: filepath.Join(dir, "maestro")}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Store{}, err
	}
	return Store{Base: filepath.Join(home, ".local", "share", "maestro")}, nil
}

// Project is a registered user repository with its private data directory.
type Project struct {
	ID             string    `json:"id"`
	UserRepository string    `json:"user_repository"`
	CreatedAt      time.Time `json:"created_at"`

	Dir string `json:"-"`
}

// Repository returns the path of the private canonical repository.
func (p Project) Repository() string {
	return filepath.Join(p.Dir, "repository.git")
}

// Init registers the repository containing path and imports it into a
// private canonical repository. It is idempotent: a repository already
// registered, under any of its worktrees or path aliases, returns the
// existing project and created=false.
func (s Store) Init(path string) (Project, bool, error) {
	canonical, err := canonicalGitDir(path)
	if err != nil {
		return Project{}, false, err
	}
	if existing, ok, err := s.Find(path); err != nil {
		return Project{}, false, err
	} else if ok {
		return existing, false, nil
	}
	head, err := git(path, "rev-parse", "HEAD")
	if err != nil {
		return Project{}, false, fmt.Errorf("the repository has no commits to import: %w", err)
	}
	project := Project{ID: newID(), UserRepository: canonical, CreatedAt: time.Now().UTC()}
	project.Dir = filepath.Join(s.Base, "projects", project.ID)
	if err := os.MkdirAll(project.Dir, 0o700); err != nil {
		return Project{}, false, err
	}
	// --no-local forces the regular transport: objects are copied, never
	// hardlinked, and no alternates can point back at the user's repository.
	if _, err := git("", "clone", "--bare", "--quiet", "--no-local", canonical, project.Repository()); err != nil {
		_ = os.RemoveAll(project.Dir)
		return Project{}, false, err
	}
	if _, err := git(project.Repository(), "update-ref", "refs/heads/maestro/integration", head); err != nil {
		_ = os.RemoveAll(project.Dir)
		return Project{}, false, err
	}
	metadata, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		_ = os.RemoveAll(project.Dir)
		return Project{}, false, err
	}
	if err := os.WriteFile(filepath.Join(project.Dir, "project.json"), append(metadata, '\n'), 0o600); err != nil {
		_ = os.RemoveAll(project.Dir)
		return Project{}, false, err
	}
	return project, true, nil
}

// Find returns the project registered for the repository containing path.
func (s Store) Find(path string) (Project, bool, error) {
	canonical, err := canonicalGitDir(path)
	if err != nil {
		return Project{}, false, err
	}
	projects, err := s.Projects()
	if err != nil {
		return Project{}, false, err
	}
	for _, project := range projects {
		if project.UserRepository == canonical {
			return project, true, nil
		}
	}
	return Project{}, false, nil
}

// Projects lists every registered project.
func (s Store) Projects() ([]Project, error) {
	entries, err := os.ReadDir(filepath.Join(s.Base, "projects"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Reads stay anchored under the store base, symbolic links included.
	root, err := os.OpenRoot(s.Base)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var projects []Project
	for _, entry := range entries {
		dir := filepath.Join(s.Base, "projects", entry.Name())
		metadata, err := readFile(root, filepath.Join("projects", entry.Name(), "project.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var project Project
		if err := json.Unmarshal(metadata, &project); err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		project.Dir = dir
		projects = append(projects, project)
	}
	return projects, nil
}

// canonicalGitDir resolves the common Git directory of the repository
// containing path, with symbolic links resolved, so every worktree and
// path alias of one repository maps to the same identity.
func canonicalGitDir(path string) (string, error) {
	dir, err := git(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("not a Git repository: %s", path)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// git runs one git command and returns its trimmed output. Arguments are
// built by Maestro, never from task text.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...) // #nosec G204
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

// readFile reads one file through the anchored root.
func readFile(root *os.Root, path string) ([]byte, error) {
	file, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(file)
}

// newID returns a random UUID version 4.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
