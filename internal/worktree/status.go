// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// TaskWorktreeStatus is the read-only state of one task worktree.
type TaskWorktreeStatus struct {
	Worker string
	TaskID string
	Branch string
	Head   string
	Dirty  bool
	Path   string
}

// Worker returns the named worker of the project when its repository
// exists.
func (p Project) Worker(name string) (Worker, bool, error) {
	if !workerName.MatchString(name) {
		return Worker{}, false, fmt.Errorf("invalid worker name: %q", name)
	}
	worker := Worker{Name: name, Dir: p.WorkerRepository(name)}
	if _, err := os.Stat(worker.Dir); err != nil {
		if os.IsNotExist(err) {
			return Worker{}, false, nil
		}
		return Worker{}, false, err
	}
	return worker, true, nil
}

// TaskWorktreeStatuses reads every task worktree of the project: its
// branch, HEAD and pending changes. A worktree directory removed by hand
// is skipped; it will be pruned by the next removal.
func (p Project) TaskWorktreeStatuses() ([]TaskWorktreeStatus, error) {
	workers, err := p.Workers()
	if err != nil {
		return nil, err
	}
	var statuses []TaskWorktreeStatus
	for _, worker := range workers {
		tasks := filepath.Join(p.Dir, "worktrees", worker.Name, "tasks")
		entries, err := os.ReadDir(tasks)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			status, err := p.taskWorktreeStatus(worker, entry.Name())
			if err != nil {
				return nil, err
			}
			statuses = append(statuses, status)
		}
	}
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].Worker != statuses[j].Worker {
			return statuses[i].Worker < statuses[j].Worker
		}
		return statuses[i].TaskID < statuses[j].TaskID
	})
	return statuses, nil
}

// taskWorktreeStatus reads one worktree after verifying through Git that
// the path really is the root of a working tree.
func (p Project) taskWorktreeStatus(worker Worker, taskID string) (TaskWorktreeStatus, error) {
	path := p.TaskWorktree(worker, taskID)
	if err := checkWorktree(path); err != nil {
		return TaskWorktreeStatus{}, err
	}
	branch, err := git(path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return TaskWorktreeStatus{}, err
	}
	head, err := git(path, "rev-parse", "HEAD")
	if err != nil {
		return TaskWorktreeStatus{}, err
	}
	dirty, err := isDirty(path)
	if err != nil {
		return TaskWorktreeStatus{}, err
	}
	return TaskWorktreeStatus{
		Worker: worker.Name,
		TaskID: taskID,
		Branch: branch,
		Head:   head,
		Dirty:  dirty,
		Path:   path,
	}, nil
}

// Diff returns the pending changes of a task worktree against its HEAD,
// staged and unstaged, without entering the worktree's history.
func (p Project) Diff(worker Worker, taskID string) (string, error) {
	path := p.TaskWorktree(worker, taskID)
	if err := checkWorktree(path); err != nil {
		return "", err
	}
	return git(path, "diff", "HEAD")
}
