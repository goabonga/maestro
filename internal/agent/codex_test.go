// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package agent

import (
	"encoding/json"
	"github.com/goabonga/maestro/internal/launcher"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func codexOptions(t *testing.T) NativeOptions {
	t.Helper()
	opts := nativeOptions(t)
	opts.Version = Version{0, 160, 0}
	return opts
}

func testNativeSpec(opts NativeOptions) launcher.Spec {
	return launcher.Spec{Dir: opts.WorkDir, Env: map[string]string{"PATH": "/usr/bin:/bin"}}
}

func writeCodexMetadata(t *testing.T, opts NativeOptions, id string) string {
	t.Helper()
	dir := filepath.Join(opts.Home, ".codex", "sessions", "2026", "10", "05")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "session_id": id, "cwd": opts.WorkDir, "cli_version": opts.Version.String()}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+id+".jsonl")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexDiscoversOnePrivateIDAndPersistsBeforeResume(t *testing.T) {
	opts := codexOptions(t)
	s, err := OpenCodex(opts)
	if err != nil {
		t.Fatal(err)
	}
	if s.Identity().ID != "" || s.Identity().Confirmed {
		t.Fatal("Codex ID chosen by caller")
	}
	if err := s.ConfirmCodex(); err == nil {
		t.Fatal("missing metadata confirmed")
	}
	spec, err := s.CodexStart(testNativeSpec(opts))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Env["CODEX_HOME"] != filepath.Join(opts.Home, ".codex") || !strings.Contains(strings.Join(spec.Argv, " "), "--no-daemon") {
		t.Fatal("private runtime not configured")
	}
	id := chosenUUID()
	path := writeCodexMetadata(t, opts, id)
	if _, err := s.CodexResume(id); err == nil {
		t.Fatal("unpersisted ID resumed")
	}
	if err := s.ConfirmCodex(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenCodex(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Identity().ID != id || !s.Identity().Confirmed {
		t.Fatal("identity lost across crash")
	}
	resumed, err := s.CodexResumeSpec(testNativeSpec(opts))
	if err != nil {
		t.Fatal(err)
	}
	command := strings.Join(resumed.Argv, " ")
	if !strings.Contains(command, "resume "+id) || strings.Contains(command, "--last") || strings.Contains(command, "dangerously") {
		t.Fatal(command)
	}
	if _, err := s.CodexResume(chosenUUID()); err == nil {
		t.Fatal("different ID resumed")
	}
	if _, err := s.CodexStart(testNativeSpec(opts)); err == nil {
		t.Fatal("existing conversation restarted as new")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CodexResume(id); err == nil {
		t.Fatal("deleted native session resumed")
	}
}

func TestCodexBlocksAmbiguousForeignAndMalformedMetadata(t *testing.T) {
	for _, name := range []string{"ambiguous", "wrong cwd", "wrong version", "conflicting IDs", "malformed", "empty", "symlink", "changed ID"} {
		t.Run(name, func(t *testing.T) {
			opts := codexOptions(t)
			s, err := OpenCodex(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			id := chosenUUID()
			path := writeCodexMetadata(t, opts, id)
			switch name {
			case "ambiguous":
				writeCodexMetadata(t, opts, chosenUUID())
			case "wrong cwd":
				opts.WorkDir = t.TempDir()
				writeCodexMetadata(t, opts, id)
			case "wrong version":
				opts.Version.Patch++
				writeCodexMetadata(t, opts, id)
			case "conflicting IDs":
				data := `{"type":"session_meta","payload":{"id":"` + id + `","session_id":"` + chosenUUID() + `","cwd":"` + opts.WorkDir + `","cli_version":"0.160.0"}}`
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "empty":
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				other := filepath.Join(t.TempDir(), "metadata")
				if err := os.WriteFile(other, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			case "changed ID":
				if err := s.ConfirmCodex(); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				writeCodexMetadata(t, opts, chosenUUID())
			}
			if err := s.ConfirmCodex(); err == nil {
				t.Fatal("unsafe metadata accepted")
			}
			if _, err := s.CodexResume(id); err == nil {
				t.Fatal("worker resumed despite invalid metadata")
			}
		})
	}
}

func TestCodexMetadataIsConfinedToItsPrivateHome(t *testing.T) {
	opts := codexOptions(t)
	s, err := OpenCodex(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	foreign := codexOptions(t)
	writeCodexMetadata(t, foreign, chosenUUID())
	if err := os.Mkdir(filepath.Join(opts.Home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(foreign.Home, ".codex", "sessions"), filepath.Join(opts.Home, ".codex", "sessions")); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmCodex(); err == nil {
		t.Fatal("another worker's session selected")
	}
}
