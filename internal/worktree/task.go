// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worktree

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrDirtyWorktree reports a worktree holding uncommitted changes, which
// is never removed: it is kept in place or moved to quarantine.
var ErrDirtyWorktree = errors.New("the worktree has uncommitted changes")

// Quarantine is an abandoned dirty worktree kept with its reference and
// its original path.
type Quarantine struct {
	Worker        string    `json:"worker"`
	TaskID        string    `json:"task_id"`
	Branch        string    `json:"branch"`
	OriginalPath  string    `json:"original_path"`
	QuarantinedAt time.Time `json:"quarantined_at"`

	Dir string `json:"-"`
}

// Worktree returns the path of the kept worktree.
func (q Quarantine) Worktree() string {
	return filepath.Join(q.Dir, "worktree")
}

// TaskBranch returns the branch of a task, independent of the worker
// holding it and kept for the task's whole life.
func TaskBranch(taskID string) string {
	return "maestro/task-" + taskID
}

// TaskWorktree returns the path of the worktree of a task under the
// storage root of a worker.
func (p Project) TaskWorktree(worker Worker, taskID string) string {
	return filepath.Join(p.Dir, "worktrees", worker.Name, "tasks", taskID)
}

// AddTaskWorktree checks the task branch out as a worktree of the worker
// repository, creating the branch from the integration head when the
// worker does not hold it yet. An existing branch is used as it stands:
// its commits survive reassignment and worktree removal. It is
// idempotent: an existing worktree is returned with created=false.
func (s Store) AddTaskWorktree(project Project, worker Worker, taskID string) (string, bool, error) {
	if !workerName.MatchString(taskID) {
		return "", false, fmt.Errorf("invalid task id: %q", taskID)
	}
	branch := TaskBranch(taskID)
	path := project.TaskWorktree(worker, taskID)
	if _, err := os.Stat(path); err == nil {
		return path, false, nil
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	if _, err := git(worker.Dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		if _, err := s.Provision(project, worker, branch); err != nil {
			return "", false, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	if _, err := git(worker.Dir, "worktree", "add", "--quiet", path, branch); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// RemoveTaskWorktree removes a clean task worktree and prunes stale
// worktree metadata. The task branch survives in the worker repository.
// A dirty worktree is refused with ErrDirtyWorktree and left untouched.
func (s Store) RemoveTaskWorktree(project Project, worker Worker, taskID string) error {
	path := project.TaskWorktree(worker, taskID)
	if err := checkWorktree(path); err != nil {
		return err
	}
	if dirty, err := isDirty(path); err != nil {
		return err
	} else if dirty {
		return fmt.Errorf("%s: %w", path, ErrDirtyWorktree)
	}
	if _, err := git(worker.Dir, "worktree", "remove", path); err != nil {
		return err
	}
	_, err := git(worker.Dir, "worktree", "prune")
	return err
}

// QuarantineTaskWorktree abandons a task worktree without losing data:
// the worktree is moved under the project's quarantine directory and its
// reference and original path are recorded next to it. The worktree stays
// registered with the worker repository.
func (s Store) QuarantineTaskWorktree(project Project, worker Worker, taskID string) (Quarantine, error) {
	path := project.TaskWorktree(worker, taskID)
	if err := checkWorktree(path); err != nil {
		return Quarantine{}, err
	}
	quarantine := Quarantine{
		Worker:        worker.Name,
		TaskID:        taskID,
		Branch:        TaskBranch(taskID),
		OriginalPath:  path,
		QuarantinedAt: time.Now().UTC(),
	}
	quarantine.Dir = filepath.Join(project.Dir, "quarantine",
		fmt.Sprintf("%s-%s-%d", worker.Name, taskID, quarantine.QuarantinedAt.UnixNano()))
	if err := os.MkdirAll(quarantine.Dir, 0o700); err != nil {
		return Quarantine{}, err
	}
	if _, err := git(worker.Dir, "worktree", "move", path, quarantine.Worktree()); err != nil {
		_ = os.Remove(quarantine.Dir)
		return Quarantine{}, err
	}
	metadata, err := json.MarshalIndent(quarantine, "", "  ")
	if err != nil {
		return Quarantine{}, err
	}
	if err := os.WriteFile(filepath.Join(quarantine.Dir, "quarantine.json"), append(metadata, '\n'), 0o600); err != nil {
		return Quarantine{}, err
	}
	return quarantine, nil
}

// Quarantines lists the quarantined worktrees of a project.
func (p Project) Quarantines() ([]Quarantine, error) {
	base := filepath.Join(p.Dir, "quarantine")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var quarantines []Quarantine
	for _, entry := range entries {
		metadata, err := readFile(root, filepath.Join(entry.Name(), "quarantine.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var quarantine Quarantine
		if err := json.Unmarshal(metadata, &quarantine); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		quarantine.Dir = filepath.Join(base, entry.Name())
		quarantines = append(quarantines, quarantine)
	}
	return quarantines, nil
}

// checkWorktree verifies through Git that path really is the root of a
// working tree, instead of trusting a reconstructed path.
func checkWorktree(path string) error {
	if inside, err := git(path, "rev-parse", "--is-inside-work-tree"); err != nil || inside != "true" {
		return fmt.Errorf("not a worktree: %s", path)
	}
	top, err := git(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if top != resolved {
		return fmt.Errorf("not the root of a worktree: %s", path)
	}
	return nil
}

// isDirty reports whether a worktree holds uncommitted changes, tracked
// or untracked.
func isDirty(path string) (bool, error) {
	status, err := git(path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return status != "", nil
}
