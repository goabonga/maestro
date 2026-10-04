// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// workerName constrains worker names to safe path components.
var workerName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Worker is a private clone belonging to one worker. Its references,
// hooks and metadata are never shared with the canonical repository or
// with other workers.
type Worker struct {
	Name string
	Dir  string
}

// WorkerRepository returns the path of the private repository of the
// named worker.
func (p Project) WorkerRepository(name string) string {
	return filepath.Join(p.Dir, "worker-repositories", name+".git")
}

// AddWorker creates the private repository of a worker: a bare clone of
// the canonical repository with fully copied objects, so no hardlink and
// no alternate can reach a protected repository. It is idempotent: an
// existing worker is returned with created=false.
func (s Store) AddWorker(project Project, name string) (Worker, bool, error) {
	if !workerName.MatchString(name) {
		return Worker{}, false, fmt.Errorf("invalid worker name: %q", name)
	}
	worker := Worker{Name: name, Dir: project.WorkerRepository(name)}
	if _, err := os.Stat(worker.Dir); err == nil {
		return worker, false, nil
	} else if !os.IsNotExist(err) {
		return Worker{}, false, err
	}
	if err := os.MkdirAll(filepath.Dir(worker.Dir), 0o700); err != nil {
		return Worker{}, false, err
	}
	// --no-local forces the regular transport, as for the canonical import.
	if _, err := git("", "clone", "--bare", "--quiet", "--no-local", project.Repository(), worker.Dir); err != nil {
		_ = os.RemoveAll(worker.Dir)
		return Worker{}, false, err
	}
	// A clone keeps a remote pointing at its origin; a worker repository
	// must hold no path back to the canonical repository.
	if _, err := git(worker.Dir, "remote", "remove", "origin"); err != nil {
		_ = os.RemoveAll(worker.Dir)
		return Worker{}, false, err
	}
	return worker, true, nil
}

// Workers lists the existing workers of a project.
func (p Project) Workers() ([]Worker, error) {
	entries, err := os.ReadDir(filepath.Join(p.Dir, "worker-repositories"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var workers []Worker
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".git" {
			continue
		}
		name = name[:len(name)-len(".git")]
		workers = append(workers, Worker{Name: name, Dir: p.WorkerRepository(name)})
	}
	return workers, nil
}

// Provision points refs/heads/<branch> of the worker repository at the
// current integration head of the canonical repository, copying the
// objects into the worker repository, and returns that head. The worker
// repository keeps no reference back to the canonical repository.
func (s Store) Provision(project Project, worker Worker, branch string) (string, error) {
	head, err := git(project.Repository(), "rev-parse", "refs/heads/maestro/integration")
	if err != nil {
		return "", fmt.Errorf("the canonical repository has no integration head: %w", err)
	}
	if _, err := git(worker.Dir, "fetch", "--quiet", project.Repository(), "refs/heads/maestro/integration"); err != nil {
		return "", err
	}
	if _, err := git(worker.Dir, "update-ref", "refs/heads/"+branch, head); err != nil {
		return "", err
	}
	return head, nil
}

// Import copies refs/heads/<branch> of the worker repository, with its
// objects, into the canonical repository under
// refs/maestro/workers/<worker>/<branch>, and returns the imported head.
// The integration branch is never moved by an import.
func (s Store) Import(project Project, worker Worker, branch string) (string, error) {
	destination := fmt.Sprintf("refs/maestro/workers/%s/%s", worker.Name, branch)
	if _, err := git(project.Repository(), "fetch", "--quiet", worker.Dir,
		fmt.Sprintf("+refs/heads/%s:%s", branch, destination)); err != nil {
		return "", err
	}
	return git(project.Repository(), "rev-parse", destination)
}
