// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package launcher

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// procRoot is the process table Survivors reads.
const procRoot = "/proc"

// deletedSuffix is what the kernel appends to the working directory of
// a process whose directory was removed.
const deletedSuffix = " (deleted)"

// Survivors lists the processes visible to the daemon whose working
// directory is dir or lies below it, by PID in ascending order. A
// confined group binds its workspace at the same path and works in it,
// so its processes show that path from the host. The list only
// observes: it proves no membership in any supervised group, and a
// caller never signals a process found here. The daemon's own process
// is left out. A directory that was removed, or removed and created
// again, still has the processes that kept working in it.
func Survivors(dir string) ([]int, error) {
	return survivorsIn(procRoot, dir)
}

// survivorsIn lists the survivors of dir from the process table at
// root.
func survivorsIn(root, dir string) ([]int, error) {
	if dir == "" {
		return nil, errors.New("a survivor search needs a directory")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	resolved, err := resolve(absolute)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		// A process that ended meanwhile, or one the daemon may not
		// inspect, has no readable working directory.
		cwd, err := os.Readlink(filepath.Join(root, entry.Name(), "cwd"))
		if err != nil {
			continue
		}
		if within(strings.TrimSuffix(cwd, deletedSuffix), resolved) {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids, nil
}

// resolve resolves the symbolic links of an absolute path. A path that
// does not exist is resolved up to its deepest existing ancestor, the
// way the kernel still names a removed working directory.
func resolve(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return resolved, err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	base, err := resolve(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, filepath.Base(path)), nil
}

// within reports whether path is dir or lies below it.
func within(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}
