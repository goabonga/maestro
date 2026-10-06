// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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

// WorkerWorktree returns the path of the worktree a worker's session
// starts in, under the worker's own directory of the project.
func (p Project) WorkerWorktree(name string) string {
	return filepath.Join(p.Dir, "workers", name, "worktree")
}

// CheckoutWorker checks the current integration head of the canonical
// repository out, detached, in the worker's own worktree of its private
// repository, copying the objects into that repository, and returns the
// worktree and the head. The worktree is created on the first checkout
// and reset to the new head afterwards: a worker is checked out when it
// starts, holding no assignment, so its uncommitted changes and untracked
// files, ignored ones included, are discarded. Its git commands run
// through isolatedGit, so no inherited variable can turn the forced
// checkout and the clean on another repository.
func (s Store) CheckoutWorker(project Project, worker Worker) (string, string, error) {
	head, err := isolatedGit(project.Repository(), "rev-parse", "refs/heads/maestro/integration")
	if err != nil {
		return "", "", fmt.Errorf("the canonical repository has no integration head: %w", err)
	}
	if _, err := isolatedGit(worker.Dir, "fetch", "--quiet", project.Repository(), "refs/heads/maestro/integration"); err != nil {
		return "", "", err
	}
	path := project.WorkerWorktree(worker.Name)
	if _, err := os.Stat(path); err == nil {
		top, err := isolatedGit(path, "rev-parse", "--show-toplevel")
		if err != nil {
			return "", "", fmt.Errorf("not a worktree: %s", path)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", "", err
		}
		if top != resolved {
			return "", "", fmt.Errorf("not the root of a worktree: %s", path)
		}
		if _, err := isolatedGit(path, "checkout", "--quiet", "--force", "--detach", head); err != nil {
			return "", "", err
		}
		if _, err := isolatedGit(path, "clean", "--quiet", "-ffdx"); err != nil {
			return "", "", err
		}
		return path, head, nil
	} else if !os.IsNotExist(err) {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", err
	}
	if _, err := isolatedGit(worker.Dir, "worktree", "add", "--quiet", "--detach", path, head); err != nil {
		return "", "", err
	}
	return path, head, nil
}

// isolatedGit runs one git command like git, but with none of the
// process's GIT_* variables, no system or global configuration and no
// hooks: inherited from a git-spawned caller (a hook, rebase --exec),
// GIT_DIR, GIT_WORK_TREE or GIT_INDEX_FILE would aim the command at the
// caller's repository instead of the given directory.
func isolatedGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=" + os.DevNull}, args...)...) // #nosec G204 -- fixed verbs; paths and revisions come from Maestro
	cmd.Dir = dir
	var env []string
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); !strings.HasPrefix(name, "GIT_") {
			env = append(env, entry)
		}
	}
	cmd.Env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
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
