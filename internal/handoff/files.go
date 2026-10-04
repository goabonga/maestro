// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package handoff

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
)

// MaxDocument bounds a handoff document.
const MaxDocument = 1 << 20

// ErrMissing reports a turn that left no handoff document.
var ErrMissing = errors.New("no handoff document")

// Path returns where the document of an attempt lives, relative to the
// worktree. The agent writes Path+".tmp" and renames it to Path, so a
// partial file never has the final name.
func Path(turnID, attemptID string) (string, error) {
	if !identifier.MatchString(turnID) || !identifier.MatchString(attemptID) {
		return "", invalid("turn %q or attempt %q is not a valid identifier", turnID, attemptID)
	}
	return path.Join(".maestro", "handoff", turnID, attemptID+".json"), nil
}

// Write publishes a document atomically under a worktree: a temporary
// file, synced, then renamed to its final name. HTTP adapters and
// tests use it; agents on a PTY follow the same protocol themselves.
func Write(worktree, turnID, attemptID string, document []byte) error {
	if len(document) > MaxDocument {
		return invalid("document of %d bytes exceeds the %d bound", len(document), MaxDocument)
	}
	relative, err := Path(turnID, attemptID)
	if err != nil {
		return err
	}
	target := filepath.Join(worktree, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	temporary := target + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- built from validated identifiers under the worktree
	if err != nil {
		return err
	}
	if _, err := file.Write(document); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, target)
}

// Read loads the document of an assignment's attempt from a worktree,
// then decodes and checks it against the assignment. It refuses any
// symbolic link on the way, anything but a regular file, and anything
// larger than MaxDocument. A temporary file never satisfies a turn,
// and the document of another attempt has another path and another
// attempt id. A missing document is ErrMissing.
func Read(worktree string, assignment Assignment) (Envelope, any, []byte, error) {
	relative, err := Path(assignment.TurnID, assignment.AttemptID)
	if err != nil {
		return Envelope{}, nil, nil, err
	}
	root, err := os.OpenRoot(worktree)
	if err != nil {
		return Envelope{}, nil, nil, err
	}
	defer func() { _ = root.Close() }()

	// Every component, final file included, must be a real entry, not a
	// symbolic link: the agent controls this tree.
	current := ""
	for _, component := range splitPath(relative) {
		current = path.Join(current, component)
		info, err := root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return Envelope{}, nil, nil, fmt.Errorf("%w for attempt %s", ErrMissing, assignment.AttemptID)
		}
		if err != nil {
			return Envelope{}, nil, nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return Envelope{}, nil, nil, invalid("%s is a symbolic link", current)
		}
	}
	// O_NOFOLLOW closes the window between the check and the open;
	// O_NONBLOCK keeps a named pipe planted there from blocking the
	// open until a writer appears.
	file, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Envelope{}, nil, nil, invalid("open %s: %v", relative, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Envelope{}, nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return Envelope{}, nil, nil, invalid("%s is not a regular file", relative)
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxDocument+1))
	if err != nil {
		return Envelope{}, nil, nil, err
	}
	if len(data) > MaxDocument {
		return Envelope{}, nil, nil, invalid("%s exceeds the %d byte bound", relative, MaxDocument)
	}
	envelope, payload, err := Decode(data)
	if err != nil {
		return Envelope{}, nil, nil, err
	}
	if err := envelope.Check(assignment); err != nil {
		return Envelope{}, nil, nil, err
	}
	return envelope, payload, data, nil
}

// splitPath splits a slash-separated relative path.
func splitPath(relative string) []string {
	var parts []string
	for relative != "" && relative != "." {
		dir, file := path.Split(relative)
		parts = append([]string{file}, parts...)
		relative = path.Clean(dir)
		if relative == "/" {
			break
		}
	}
	return parts
}
