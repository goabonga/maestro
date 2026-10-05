// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/goabonga/maestro/internal/launcher"
)

// OpenClaude chooses and durably records an ID before starting Claude. An
// existing worker record is recovered after a crash without allocating an ID.
func OpenClaude(options NativeOptions) (*NativeSession, error) {
	return openNative("claude-code", options)
}

// ClaudeStart constructs the interactive chosen-ID command with an explicit
// native permission mode. System confinement remains the launcher's job.
func (s *NativeSession) ClaudeStart(base launcher.Spec) (launcher.Spec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.identity.Kind != "claude-code" || s.identity.Confirmed {
		return launcher.Spec{}, errors.New("Claude start requires an unconfirmed chosen session")
	}
	result, err := s.configure(base, []string{s.binary, "--session-id", s.identity.ID, "--permission-mode", "acceptEdits"})
	if err == nil {
		result.Env["CLAUDE_CONFIG_DIR"] = filepath.Join(s.identity.Home, ".claude")
	}
	return result, err
}

// ClaudeResume matches session.Epoch's exact-ID callback. Confirmation must
// come from native metadata before any assignment or automatic restart.
func (s *NativeSession) ClaudeResume(id string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil || s.identity.Kind != "claude-code" || !s.identity.Confirmed || id != s.identity.ID {
		return nil, errors.New("Claude resume requires its exact confirmed ID")
	}
	if err := s.verifyClaude(); err != nil {
		return nil, err
	}
	return []string{s.binary, "--resume", id, "--permission-mode", "acceptEdits"}, nil
}

// ClaudeResumeSpec restores the private environment and mounts as well as
// the exact resume command when reconstructing a profile after daemon crash.
func (s *NativeSession) ClaudeResumeSpec(base launcher.Spec) (launcher.Spec, error) {
	argv, err := s.ClaudeResume(s.Identity().ID)
	if err != nil {
		return launcher.Spec{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.configure(base, argv)
	if err == nil {
		result.Env["CLAUDE_CONFIG_DIR"] = filepath.Join(s.identity.Home, ".claude")
	}
	return result, err
}

// ConfirmClaude verifies Claude's chosen ID in its private project transcript
// and persists confirmation before returning. No ID is extracted from PTY.
func (s *NativeSession) ConfirmClaude() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil || s.identity.Kind != "claude-code" {
		return errors.New("not an open Claude session")
	}
	if err := s.verifyClaude(); err != nil {
		return err
	}
	return s.confirm(s.identity.ID)
}

func (s *NativeSession) verifyClaude() error {
	root, err := s.metadataRoot(".claude/projects")
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	found := false
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in native session metadata")
		}
		if entry.IsDir() || filepath.Base(path) != s.identity.ID+".jsonl" {
			return nil
		}
		file, err := root.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		scanner := bufio.NewScanner(io.LimitReader(file, 8<<20))
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			var row struct {
				Type      string `json:"type"`
				SessionID string `json:"sessionId"`
				CWD       string `json:"cwd"`
				Version   string `json:"version"`
				Sidechain bool   `json:"isSidechain"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
				return fmt.Errorf("invalid Claude metadata: %w", err)
			}
			if row.SessionID == "" {
				continue
			}
			if row.SessionID != s.identity.ID || row.Sidechain {
				return errors.New("Claude metadata identity mismatch")
			}
			// Native mode records precede the first conversation message and
			// carry an ID but no worktree/version provenance. They are never
			// sufficient confirmation; continue to a context-bearing record.
			if row.CWD == "" && row.Version == "" && hasAny(row.Type, "mode", "permission-mode", "atis-latch") {
				continue
			}
			if row.CWD != s.identity.WorkDir || row.Version != s.identity.Version {
				return errors.New("Claude metadata provenance mismatch")
			}
			found = true
			return nil
		}
		return scanner.Err()
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("chosen Claude session is not confirmed by native metadata")
	}
	return nil
}

func (s *NativeSession) confirm(id string) error {
	if !nativeUUID.MatchString(id) || (s.identity.ID != "" && s.identity.ID != id) {
		return errors.New("native session ID changed or invalid")
	}
	previous := s.identity
	s.identity.ID = id
	s.identity.Confirmed = true
	if err := s.persist(); err != nil {
		s.identity = previous
		return fmt.Errorf("persist native session ID: %w", err)
	}
	return nil
}

// PromptInput uses bracketed paste for multiline input, then submits it.
// Terminal controls are refused rather than letting a task inject CLI keys.
func PromptInput(prompt string) ([]byte, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("empty native prompt")
	}
	for _, r := range prompt {
		if r < 32 && r != '\n' && r != '\t' {
			return nil, errors.New("terminal control in native prompt")
		}
		if r == 127 {
			return nil, errors.New("terminal control in native prompt")
		}
	}
	return []byte("\x1b[200~" + prompt + "\x1b[201~\r"), nil
}
