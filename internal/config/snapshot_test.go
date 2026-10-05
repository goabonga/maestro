// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/state"
)

func TestTakeFreezesConfigInstructionsAndDrivers(t *testing.T) {
	repository := project(t, map[string]string{
		ProjectFile:                       "[budgets]\nmax_turns_per_task = 12\n",
		"maestro/agents/claude/CLAUDE.md": "# Rules\nBe precise.\n",
		"maestro/agents/codex/AGENTS.md":  "# Agents\n",
	})
	snapshot, err := Take(repository)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Config.Budgets.MaxTurnsPerTask != 12 || snapshot.HandoffSchema != 1 || len(snapshot.Drivers) != 2 {
		t.Fatalf("snapshot %+v", snapshot)
	}
	if snapshot.Instructions["agents/claude/CLAUDE.md"] != "# Rules\nBe precise.\n" || len(snapshot.Instructions) != 2 {
		t.Fatalf("instructions %v", snapshot.Instructions)
	}

	// The id depends on the content, and only on it.
	first, _, err := snapshot.Encode()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Take(repository)
	if err != nil {
		t.Fatal(err)
	}
	if second, _, _ := again.Encode(); second != first || !strings.HasPrefix(first, "sha256-") {
		t.Fatalf("ids %s %s", first, second)
	}
	if err := os.WriteFile(filepath.Join(repository, "maestro", "agents", "codex", "AGENTS.md"), []byte("# Changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := Take(repository)
	if err != nil {
		t.Fatal(err)
	}
	if third, _, _ := changed.Encode(); third == first {
		t.Fatal("an instruction change kept the same config_id")
	}
}

func TestTakeRefusesUnsafeInstructions(t *testing.T) {
	cases := map[string]func(t *testing.T, repository string){
		"secret": func(t *testing.T, repository string) {
			write(t, filepath.Join(repository, "maestro", "a.md"), "key: sk-ant-FAKE0123456789abcdefFAKE\n")
		},
		"binary": func(t *testing.T, repository string) {
			write(t, filepath.Join(repository, "maestro", "a.bin"), "\xff\xfe\x00")
		},
		"symlink": func(t *testing.T, repository string) {
			write(t, filepath.Join(repository, "outside.md"), "x")
			if err := os.MkdirAll(filepath.Join(repository, "maestro"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(repository, "outside.md"), filepath.Join(repository, "maestro", "link.md")); err != nil {
				t.Fatal(err)
			}
		},
		"oversized": func(t *testing.T, repository string) {
			write(t, filepath.Join(repository, "maestro", "big.md"), strings.Repeat("a", MaxInstructionFile+1))
		},
		"not a directory": func(t *testing.T, repository string) {
			write(t, filepath.Join(repository, "maestro"), "x")
		},
	}
	for name, setup := range cases {
		repository := t.TempDir()
		setup(t, repository)
		if _, err := Take(repository); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: expected ErrInvalid, got %v", name, err)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPersistIsIdempotentAndVerified(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Take(project(t, map[string]string{"maestro/a.md": "rules\n"}))
	if err != nil {
		t.Fatal(err)
	}
	id, err := Persist(db, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := Persist(db, snapshot); err != nil || again != id {
		t.Fatalf("second persist: %s %v", again, err)
	}
	loaded, err := LoadSnapshot(db, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Instructions["a.md"] != "rules\n" || loaded.Config.Budgets.TurnTimeout != snapshot.Config.Budgets.TurnTimeout {
		t.Fatalf("loaded %+v", loaded)
	}
	if _, err := db.Exec("UPDATE config_snapshots SET document = '{}' WHERE config_id = ?", id); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshot(db, id); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a tampered snapshot loaded: %v", err)
	}
	if _, err := LoadSnapshot(db, "sha256-unknown"); err == nil {
		t.Fatal("an unknown id loaded")
	}
}

func TestSnapshotFreezesTestCommands(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	repository := project(t, map[string]string{ProjectFile: "[tests.unit]\nargv = [\"go\", \"test\", \"./...\"]\ntimeout = \"5m\"\n"})
	snapshot, err := Take(repository)
	if err != nil {
		t.Fatal(err)
	}
	id, err := Persist(db, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSnapshot(db, id)
	if err != nil {
		t.Fatal(err)
	}
	unit := loaded.Config.Tests["unit"]
	if strings.Join(unit.Argv, " ") != "go test ./..." || unit.Timeout == nil || unit.Timeout.String() != "5m0s" {
		t.Fatalf("loaded test command %+v", unit)
	}
	write(t, filepath.Join(repository, ProjectFile), "[tests.unit]\nargv = [\"go\", \"test\", \"-race\", \"./...\"]\ntimeout = \"5m\"\n")
	changed, err := Take(repository)
	if err != nil {
		t.Fatal(err)
	}
	if other, _, _ := changed.Encode(); other == id {
		t.Fatal("a test command change kept the same config_id")
	}
}
