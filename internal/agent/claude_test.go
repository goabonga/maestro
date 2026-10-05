// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/session"
)

func nativeOptions(t *testing.T) NativeOptions {
	t.Helper()
	options := NativeOptions{WorkerID: "worker-1", ConfigID: "config-1", WorkDir: t.TempDir(), Home: t.TempDir(), StateDir: t.TempDir(), Version: Version{2, 1, 289}}
	for _, path := range []string{options.Home, options.StateDir} {
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return options
}

func writeClaudeMetadata(t *testing.T, options NativeOptions, id string) {
	t.Helper()
	dir := filepath.Join(options.Home, ".claude", "projects", "fixture")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"sessionId": id, "cwd": options.WorkDir, "version": options.Version.String(), "isSidechain": false})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeChosenIdentitySurvivesReopenAndRequiresConfirmation(t *testing.T) {
	opts := nativeOptions(t)
	s, err := OpenClaude(opts)
	if err != nil {
		t.Fatal(err)
	}
	id := s.Identity().ID
	if !nativeUUID.MatchString(id) || s.Identity().Confirmed {
		t.Fatal(s.Identity())
	}
	if _, err := s.ClaudeResume(id); err == nil {
		t.Fatal("unconfirmed resume accepted")
	}
	if err := s.ConfirmClaude(); err == nil {
		t.Fatal("missing transcript accepted")
	}
	if _, err := OpenClaude(opts); err == nil {
		t.Fatal("concurrent owner accepted")
	}
	writeClaudeMetadata(t, opts, id)
	if err := s.ConfirmClaude(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenClaude(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Identity().ID != id || !s.Identity().Confirmed {
		t.Fatal("identity lost on restart")
	}
	argv, err := s.ClaudeResume(id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv, " "), "--continue") || !strings.Contains(strings.Join(argv, " "), "--resume "+id) {
		t.Fatal(argv)
	}
	if _, err := s.ClaudeResume(chosenUUID()); err == nil {
		t.Fatal("different conversation accepted")
	}
	if _, err := s.ClaudeStart(launcher.Spec{Dir: opts.WorkDir}); err == nil {
		t.Fatal("confirmed conversation relaunched as new")
	}
}

func TestClaudeRefusesChangedWorkerAndUnsafeMetadata(t *testing.T) {
	opts := nativeOptions(t)
	s, err := OpenClaude(opts)
	if err != nil {
		t.Fatal(err)
	}
	id := s.Identity().ID
	_ = s.Close()
	changed := opts
	changed.ConfigID = "different"
	if _, err := OpenClaude(changed); err == nil {
		t.Fatal("changed configuration accepted")
	}
	s, err = OpenClaude(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	writeClaudeMetadata(t, opts, id)
	path := filepath.Join(opts.Home, ".claude", "projects", "fixture", id+".jsonl")
	if err := os.WriteFile(path, []byte(`{"sessionId":"`+id+`","cwd":"/another","version":"2.1.289"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmClaude(); err == nil {
		t.Fatal("wrong worktree accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), id+".jsonl")
	if err := os.WriteFile(out, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, path); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmClaude(); err == nil {
		t.Fatal("outside metadata symlink accepted")
	}
	if s.Identity().Confirmed {
		t.Fatal("failed confirmation changed identity")
	}
}

func TestNativeCommandProtectsSupervisorStateAndPromptControls(t *testing.T) {
	opts := nativeOptions(t)
	s, err := OpenClaude(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	base := launcher.Spec{Dir: opts.WorkDir, Env: map[string]string{"HOME": "original"}}
	configured, err := s.ClaudeStart(base)
	if err != nil {
		t.Fatal(err)
	}
	if configured.Env["HOME"] != opts.Home || base.Env["HOME"] != "original" {
		t.Fatal("private environment is not independent")
	}
	base.Writable = []string{opts.StateDir}
	if _, err := s.ClaudeStart(base); err == nil {
		t.Fatal("supervisor identity mounted writable")
	}
	for _, prompt := range []string{"", "bad\x1b[201~\r/exit", "bad\x03", "bad\x7f"} {
		if _, err := PromptInput(prompt); err == nil {
			t.Fatal("terminal control accepted")
		}
	}
	if _, err := PromptInput("first line\nsecond line"); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeFixtureConfirmsInsideAConfinedPTY(t *testing.T) {
	l, err := launcher.New()
	if errors.Is(err, launcher.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	opts := nativeOptions(t)
	opts.Binary = filepath.Join(opts.WorkDir, "fixture-cli")
	script := `#!/bin/sh
id="$2"
mkdir -p "$CLAUDE_CONFIG_DIR/projects/fixture"
printf '{"sessionId":"%s","cwd":"%s","version":"2.1.289"}\n' "$id" "$PWD" > "$CLAUDE_CONFIG_DIR/projects/fixture/$id.jsonl"
echo READY
cat
`
	if err := os.WriteFile(opts.Binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := OpenClaude(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	spec, err := s.ClaudeStart(launcher.Spec{Dir: opts.WorkDir, Env: map[string]string{"PATH": "/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	live, err := session.Start(l, session.Config{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Stop(100 * time.Millisecond) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := live.Output()
		if strings.Contains(string(data), "READY") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.ConfirmClaude(); err != nil {
		t.Fatal(err)
	}
	if !s.Identity().Confirmed {
		t.Fatal("native ID not persisted before assignment")
	}
}
