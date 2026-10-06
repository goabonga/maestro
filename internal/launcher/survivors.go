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

// Survivors lists the processes visible to the daemon whose working
// directory is dir or lies below it, by PID in ascending order. A
// confined group binds its workspace at the same path and works in it,
// so its processes show that path from the host. The list only
// observes: it proves no membership in any supervised group, and a
// caller never signals a process found here. The daemon's own process
// is left out; a directory that does not exist has no survivor.
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
	resolved, err := filepath.EvalSymlinks(absolute)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
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
		if within(cwd, resolved) {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids, nil
}

// within reports whether path is dir or lies below it.
func within(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}
