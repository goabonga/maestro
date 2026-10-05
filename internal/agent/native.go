// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package agent

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/goabonga/maestro/internal/launcher"
	"golang.org/x/sys/unix"
)

// NativeOptions identifies one worker's conversation. StateDir is private
// supervisor storage, outside both Home and WorkDir, and must not be mounted
// writable in the agent. Home is the worker's exclusively owned private HOME.
// Credentials and native instruction/MCP configuration are provisioned by the
// caller; none are inherited or copied by a driver.
type NativeOptions struct {
	WorkerID, ConfigID, WorkDir, Home, StateDir string
	Version                                     Version
	// Binary may name a resolved executable; empty uses the registered name.
	Binary string
}

// NativeIdentity is durable supervisor evidence, not terminal text.
type NativeIdentity struct {
	Schema    int    `json:"schema"`
	WorkerID  string `json:"worker_id"`
	ConfigID  string `json:"config_id"`
	Kind      string `json:"kind"`
	Version   string `json:"version"`
	WorkDir   string `json:"workdir"`
	Home      string `json:"home"`
	ID        string `json:"id"`
	Confirmed bool   `json:"confirmed"`
}

// NativeSession owns a worker's durable identity and private session space.
// Close releases the exclusive supervisor lock; a daemon crash releases it
// through the kernel. Restart opens the same record, never chooses a latest ID.
type NativeSession struct {
	mu       sync.Mutex
	root     *os.Root
	lock     *os.File
	identity NativeIdentity
	binary   string
}

var nativeUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func chosenUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func openNative(kind string, options NativeOptions) (*NativeSession, error) {
	driver, err := Builtin().Select(kind, options.Version)
	if err != nil {
		return nil, err
	}
	if options.WorkerID == "" || options.ConfigID == "" {
		return nil, errors.New("worker and configuration IDs are required")
	}
	paths := []*string{&options.WorkDir, &options.Home, &options.StateDir}
	for _, path := range paths {
		if !filepath.IsAbs(*path) {
			return nil, errors.New("native session paths must be absolute existing directories")
		}
		*path, err = filepath.EvalSymlinks(*path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(*path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, errors.New("native session path is not a directory")
		}
	}
	if pathOverlap(options.Home, options.WorkDir) || pathOverlap(options.StateDir, options.Home) || pathOverlap(options.StateDir, options.WorkDir) {
		return nil, errors.New("native HOME, sources and supervisor state must be separate")
	}
	for _, path := range []string{options.Home, options.StateDir} {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("native HOME and supervisor state must be private")
		}
	}
	root, err := os.OpenRoot(options.StateDir)
	if err != nil {
		return nil, err
	}
	lock, err := root.OpenFile("native.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		_ = root.Close()
		return nil, fmt.Errorf("native worker already owned: %w", err)
	}
	s := &NativeSession{root: root, lock: lock, binary: options.Binary}
	if s.binary == "" {
		s.binary = driver.Binary
	}
	want := NativeIdentity{Schema: 1, WorkerID: options.WorkerID, ConfigID: options.ConfigID, Kind: kind, Version: options.Version.String(), WorkDir: options.WorkDir, Home: options.Home}
	file, err := root.Open("native.json")
	if errors.Is(err, os.ErrNotExist) {
		s.identity = want
		if kind == "claude-code" {
			s.identity.ID = chosenUUID()
		}
		err = s.persist()
	} else if err == nil {
		decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&s.identity)
		if err == nil {
			var extra any
			if decoder.Decode(&extra) != io.EOF {
				err = errors.New("trailing native identity data")
			}
		}
		_ = file.Close()
		if err == nil {
			actual := s.identity
			actual.ID = ""
			actual.Confirmed = false
			if actual != want || (s.identity.ID != "" && !nativeUUID.MatchString(s.identity.ID)) || (s.identity.Confirmed && s.identity.ID == "") || (kind == "claude-code" && s.identity.ID == "") {
				err = errors.New("native identity does not match worker, configuration or version")
			}
		}
	}
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func pathOverlap(a, b string) bool {
	contains := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != ".." && !filepath.IsAbs(rel) && (len(rel) < 3 || rel[:3] != ".."+string(filepath.Separator))
	}
	return contains(a, b) || contains(b, a)
}

// Identity returns a value copy. Confirmed alone authorizes assignment;
// callers must also confirm their permissions epoch before sending a task.
func (s *NativeSession) Identity() NativeIdentity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identity
}

// Close releases supervisor ownership. It does not stop a PTY: the caller
// must first use the session epoch barrier before releasing an assignment.
func (s *NativeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return nil
	}
	err := errors.Join(s.lock.Close(), s.root.Close())
	s.root = nil
	return err
}

// persist publishes atomically and fsyncs the record and directory before
// confirmation is returned. No credentials or transcript text enter it.
func (s *NativeSession) persist() error {
	name := "native-" + chosenUUID() + ".tmp"
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = s.root.Remove(name) }()
	err = json.NewEncoder(file).Encode(s.identity)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := s.root.Rename(name, "native.json"); err != nil {
		return err
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (s *NativeSession) configure(base launcher.Spec, argv []string) (launcher.Spec, error) {
	if s.root == nil {
		return launcher.Spec{}, errors.New("native session is closed")
	}
	resolved, err := filepath.EvalSymlinks(base.Dir)
	if err != nil || resolved != s.identity.WorkDir {
		return launcher.Spec{}, errors.New("native command must use its recorded worktree")
	}
	base.Argv = argv
	base.Env = maps.Clone(base.Env)
	if base.Env == nil {
		base.Env = map[string]string{}
	}
	base.Env["HOME"] = s.identity.Home
	delete(base.Env, "CODEX_THREAD_ID")
	delete(base.Env, "CODEX_SESSION_ID")
	base.Writable = append(append([]string(nil), base.Writable...), s.identity.Home)
	for _, path := range base.Writable {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return launcher.Spec{}, err
		}
		if pathOverlap(resolved, s.root.Name()) {
			return launcher.Spec{}, errors.New("supervisor identity cannot be writable in the agent")
		}
	}
	return base, nil
}

func (s *NativeSession) metadataRoot(path string) (*os.Root, error) {
	home, err := os.OpenRoot(s.identity.Home)
	if err != nil {
		return nil, err
	}
	defer func() { _ = home.Close() }()
	return home.OpenRoot(path)
}
