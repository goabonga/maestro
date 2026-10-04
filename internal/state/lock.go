// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package state

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock is an exclusive advisory lock on a file. The kernel releases it
// when the holding process dies, so a stale lock cannot survive a
// crash; no PID file is ever consulted.
type Lock struct {
	file *os.File
}

// Acquire takes the exclusive lock at path without blocking. A lock
// already held by another process is refused.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- path is built from Maestro's own data directory, never from task text
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("another maestro process holds the lock at %s", path)
		}
		return nil, err
	}
	return &Lock{file: file}, nil
}

// Release drops the lock. The file stays in place: its existence means
// nothing, only the kernel lock does.
func (l *Lock) Release() error {
	if err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN); err != nil {
		_ = l.file.Close()
		return err
	}
	return l.file.Close()
}
