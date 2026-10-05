// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/goabonga/maestro/internal/launcher"
)

// OpenCodex opens one worker's exclusively private session space. Its native
// ID stays absent until Codex creates metadata and ConfirmCodex persists it.
func OpenCodex(options NativeOptions) (*NativeSession, error) { return openNative("codex", options) }

// CodexStart uses workspace-write with approvals explicitly pre-granted.
// --no-daemon keeps the runtime inside this supervised PTY's namespace,
// preventing a connection to a host-wide background server.
func (s *NativeSession) CodexStart(base launcher.Spec) (launcher.Spec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.identity.Kind != "codex" || s.identity.ID != "" {
		return launcher.Spec{}, errors.New("Codex start requires a worker without a recorded native ID")
	}
	result, err := s.configure(base, append([]string{s.binary}, codexFlags...))
	if err == nil {
		result.Env["CODEX_HOME"] = filepath.Join(s.identity.Home, ".codex")
	}
	return result, err
}

var codexFlags = []string{"--no-alt-screen", "--no-daemon", "--sandbox", "workspace-write", "--ask-for-approval", "never"}

// CodexResume constructs only an exact-ID interactive resume. Missing or
// ambiguous metadata, a changed ID and an unconfirmed record all block it.
func (s *NativeSession) CodexResume(id string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil || s.identity.Kind != "codex" || !s.identity.Confirmed || id != s.identity.ID {
		return nil, errors.New("Codex resume requires its exact confirmed native ID")
	}
	observed, err := s.codexID()
	if err != nil {
		return nil, err
	}
	if observed != id {
		return nil, errors.New("Codex native identity changed")
	}
	return append([]string{s.binary, "resume", id}, codexFlags...), nil
}

// CodexResumeSpec reconstructs the private HOME, CODEX_HOME and mounts when
// a daemon recreates the permissions profile from its persisted snapshot.
func (s *NativeSession) CodexResumeSpec(base launcher.Spec) (launcher.Spec, error) {
	argv, err := s.CodexResume(s.Identity().ID)
	if err != nil {
		return launcher.Spec{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.configure(base, argv)
	if err == nil {
		result.Env["CODEX_HOME"] = filepath.Join(s.identity.Home, ".codex")
	}
	return result, err
}

// ConfirmCodex reads local session_meta records, never PTY text or mtime,
// then fsyncs the exact ID before an assignment can be admitted.
func (s *NativeSession) ConfirmCodex() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil || s.identity.Kind != "codex" {
		return errors.New("not an open Codex session")
	}
	id, err := s.codexID()
	if err != nil {
		return err
	}
	return s.confirm(id)
}

func (s *NativeSession) codexID() (string, error) {
	root, err := s.metadataRoot(".codex/sessions")
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	ids := map[string]bool{}
	files := 0
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in Codex session metadata")
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		files++
		if files > 1024 {
			return errors.New("too many Codex session metadata files")
		}
		file, err := root.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		if !scanner.Scan() {
			return errors.Join(scanner.Err(), errors.New("empty Codex metadata"))
		}
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				ID        string `json:"id"`
				SessionID string `json:"session_id"`
				CWD       string `json:"cwd"`
				Version   string `json:"cli_version"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return fmt.Errorf("invalid Codex session metadata: %w", err)
		}
		p := row.Payload
		if row.Type != "session_meta" || !nativeUUID.MatchString(p.ID) || (p.SessionID != "" && p.SessionID != p.ID) || p.CWD != s.identity.WorkDir || p.Version != s.identity.Version {
			return errors.New("Codex session metadata identity mismatch")
		}
		ids[p.ID] = true
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("Codex native ID missing or ambiguous: %d candidates", len(ids))
	}
	for id := range ids {
		return id, nil
	}
	return "", errors.New("missing Codex native ID")
}
