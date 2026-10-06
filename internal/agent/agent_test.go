// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVersionReadsCLIOutputs(t *testing.T) {
	for output, want := range map[string]Version{
		"2.1.289 (Claude Code)\n": {2, 1, 289},
		"codex-cli 0.160.0\n":     {0, 160, 0},
	} {
		got, err := ParseVersion(output)
		if err != nil || got != want {
			t.Fatalf("%q: got %v, %v", output, got, err)
		}
	}
	if _, err := ParseVersion("no version here"); err == nil {
		t.Fatal("a version was invented")
	}
}

func TestSelectRefusesUnvalidatedVersions(t *testing.T) {
	registry := Builtin()
	driver, err := registry.Select("claude-code", Version{2, 1, 289})
	if err != nil || driver.Name != "claude-code-2.1" {
		t.Fatalf("driver %+v, %v", driver, err)
	}
	for _, version := range []Version{{2, 1, 288}, {2, 1, 290}, {3, 0, 0}} {
		_, err := registry.Select("claude-code", version)
		if !errors.Is(err, ErrNoDriver) || !strings.Contains(err.Error(), "2.1.289–2.1.289") {
			t.Fatalf("%s: %v", version, err)
		}
	}
	if _, err := registry.Select("gemini", Version{1, 0, 0}); !errors.Is(err, ErrNoDriver) {
		t.Fatalf("unknown kind: %v", err)
	}
}

func TestNewRegistryRejectsInconsistentDrivers(t *testing.T) {
	base := Driver{Kind: "k", Name: "a", Binary: "b", Min: Version{1, 0, 0}, Max: Version{1, 5, 0}}
	overlapping := Driver{Kind: "k", Name: "c", Binary: "b", Min: Version{1, 5, 0}, Max: Version{2, 0, 0}}
	if _, err := NewRegistry(base, overlapping); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("error %v", err)
	}
	reversed := Driver{Kind: "k", Name: "r", Binary: "b", Min: Version{2, 0, 0}, Max: Version{1, 0, 0}}
	if _, err := NewRegistry(reversed); err == nil {
		t.Fatal("reversed bounds accepted")
	}
	if _, err := NewRegistry(Driver{Kind: "k"}); err == nil {
		t.Fatal("an incomplete driver was accepted")
	}
	adjacent := Driver{Kind: "k", Name: "d", Binary: "b", Min: Version{1, 5, 1}, Max: Version{2, 0, 0}}
	if _, err := NewRegistry(base, adjacent); err != nil {
		t.Fatalf("adjacent ranges refused: %v", err)
	}
}

// fakeBinary writes a script printing a version line.
func fakeBinary(t *testing.T, dir, name, output string) {
	t.Helper()
	script := "#!/bin/sh\necho '" + output + "'\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorReportsEachRequirement(t *testing.T) {
	bin := t.TempDir()
	fakeBinary(t, bin, "claude", "2.1.289 (Claude Code)")
	fakeBinary(t, bin, "codex", "codex-cli 0.161.0")
	doctor := Doctor{
		Registry: Builtin(),
		Sandbox:  func() error { return nil },
		LookPath: func(name string) (string, error) {
			path := filepath.Join(bin, name)
			if _, err := os.Stat(path); err != nil {
				return "", err
			}
			return path, nil
		},
	}
	checks := doctor.Run(context.Background())
	if len(checks) != 3 {
		t.Fatalf("checks %+v", checks)
	}
	if !checks[0].OK || checks[0].Name != "sandbox" {
		t.Fatalf("sandbox %+v", checks[0])
	}
	if !checks[1].OK || !strings.Contains(checks[1].Detail, "claude-code-2.1") {
		t.Fatalf("claude %+v", checks[1])
	}
	if checks[2].OK || !strings.Contains(checks[2].Detail, "0.161.0") {
		t.Fatalf("an unvalidated codex was accepted: %+v", checks[2])
	}

	os.Remove(filepath.Join(bin, "codex"))
	doctor.Sandbox = func() error { return errors.New("bwrap not found") }
	checks = doctor.Run(context.Background())
	if checks[0].OK || !strings.Contains(checks[0].Detail, "bwrap") {
		t.Fatalf("sandbox %+v", checks[0])
	}
	if checks[2].OK || !strings.Contains(checks[2].Detail, "not found") {
		t.Fatalf("missing codex %+v", checks[2])
	}
}

func TestDriversListsEveryRegisteredDriver(t *testing.T) {
	drivers := Builtin().Drivers()
	if len(drivers) != 2 || drivers[0].Name != "claude-code-2.1" || drivers[1].Name != "codex-0.160" {
		t.Fatalf("drivers %+v", drivers)
	}
	drivers[0].Name = "changed"
	if Builtin().Drivers()[0].Name != "claude-code-2.1" {
		t.Fatal("the registry handed out its own slice")
	}
}

func TestInstalledSelectsTheDriverOfTheInstalledVersion(t *testing.T) {
	bin := t.TempDir()
	fakeBinary(t, bin, "claude", "2.1.289 (Claude Code)")
	fakeBinary(t, bin, "codex", "codex-cli 0.161.0")
	lookPath := func(name string) (string, error) {
		path := filepath.Join(bin, name)
		if _, err := os.Stat(path); err != nil {
			return "", err
		}
		return path, nil
	}
	installed, err := Builtin().Installed(context.Background(), "claude-code", lookPath)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Path != filepath.Join(bin, "claude") || installed.Version != (Version{2, 1, 289}) ||
		installed.Driver.Name != "claude-code-2.1" {
		t.Fatalf("installed %+v", installed)
	}
	if _, err := Builtin().Installed(context.Background(), "codex", lookPath); !errors.Is(err, ErrNoDriver) {
		t.Fatalf("an unvalidated version was accepted: %v", err)
	}
	if _, err := Builtin().Installed(context.Background(), "openai-compatible", lookPath); !errors.Is(err, ErrNoDriver) {
		t.Fatalf("an unknown kind was accepted: %v", err)
	}
}
