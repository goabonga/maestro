// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/launcher"
)

func profileFixture(t *testing.T) ProfileSpec {
	t.Helper()
	work := t.TempDir()
	git := filepath.Join(work, ".git")
	handoff := filepath.Join(work, "handoff")
	for _, path := range []string{git, handoff} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return ProfileSpec{Epoch: 1, Role: Coding, Revision: "fixture-sha", GitDir: git, Handoff: handoff,
		Config: Config{Spec: launcher.Spec{Dir: work, Argv: []string{"/bin/sh"}, Env: map[string]string{"LABEL": "original"}}}}
}

func mustProfile(t *testing.T, spec ProfileSpec) Profile {
	t.Helper()
	profile, err := NewProfile(spec)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestProfileIsImmutable(t *testing.T) {
	spec := profileFixture(t)
	profile := mustProfile(t, spec)
	spec.Config.Spec.Argv[0] = "/bin/false"
	spec.Config.Spec.Env["LABEL"] = "mutated"
	exported := profile.Spec()
	exported.Config.Spec.Argv[0] = "/bin/true"
	exported.Config.Spec.Env["LABEL"] = "changed again"
	config := profile.confinedConfig(profile.Spec().Config.Spec.Argv)
	if config.Spec.Argv[0] != "/bin/sh" || config.Spec.Env["LABEL"] != "original" {
		t.Fatalf("profile changed: %+v", config)
	}
}

func TestProfileRejectsWritableAliasesAndProtectedMounts(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ProfileSpec)
	}{
		{"zero epoch", func(s *ProfileSpec) { s.Epoch = 0 }},
		{"no revision", func(s *ProfileSpec) { s.Revision = "" }},
		{"unknown role", func(s *ProfileSpec) { s.Role = "human" }},
		{"no command", func(s *ProfileSpec) { s.Config.Spec.Argv = nil }},
		{"relative dir", func(s *ProfileSpec) { s.Config.Spec.Dir = "relative" }},
		{"repair without handoff", func(s *ProfileSpec) { s.Role = Repair; s.Handoff = "" }},
		{"handoff is source", func(s *ProfileSpec) { s.Handoff = s.Config.Spec.Dir }},
		{"handoff is git", func(s *ProfileSpec) { s.Handoff = s.GitDir }},
		{"writable source parent", func(s *ProfileSpec) { s.Config.Spec.Writable = []string{filepath.Dir(s.Config.Spec.Dir)} }},
		{"repair extra writable source", func(s *ProfileSpec) { s.Role = Repair; s.Config.Spec.Writable = []string{s.Handoff} }},
		{"writable instruction", func(s *ProfileSpec) { s.Config.Spec.ReadOnly = []string{s.Handoff} }},
		{"readonly git overwritten", func(s *ProfileSpec) { s.Role = Review; s.Config.Spec.Writable = []string{s.GitDir} }},
		{"symlink alias", func(s *ProfileSpec) {
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(s.Config.Spec.Dir, alias); err != nil {
				t.Fatal(err)
			}
			s.Role = Review
			s.Config.Spec.Writable = []string{alias}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := profileFixture(t)
			test.change(&spec)
			if _, err := NewProfile(spec); err == nil {
				t.Fatal("unsafe profile accepted")
			}
		})
	}
}

func startEpoch(t *testing.T, spec ProfileSpec) *Epoch {
	t.Helper()
	epoch, err := StartEpoch(sandbox(t), mustProfile(t, spec))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = epoch.Session().Stop(100 * time.Millisecond) })
	return epoch
}

func TestBoundaryStopsDescendantsBeforeReconciliation(t *testing.T) {
	for _, reason := range []string{"turn_complete", "interrupted", "pause", "detach", "role_change"} {
		t.Run(reason, func(t *testing.T) {
			spec := profileFixture(t)
			spec.Config.Spec.Argv = []string{"/bin/sh", "-c", "(echo child >> writes; echo READY; while :; do echo child >> writes; sleep 0.01; done) & wait"}
			epoch := startEpoch(t, spec)
			old := epoch.Session()
			waitOutput(t, old, "READY")
			called := false
			if err := epoch.End(reason, 100*time.Millisecond, func(boundary Boundary) error {
				called = true
				if epoch.State().Phase != EpochBlocked {
					t.Fatal("boundary not exposed as blocked")
				}
				if boundary.Epoch != 1 || boundary.Revision != spec.Revision || boundary.Reason != reason {
					t.Fatalf("boundary %+v", boundary)
				}
				if _, err := old.Write([]byte("echo late\r")); err == nil {
					t.Fatal("input admitted during reconciliation")
				}
				// An open descriptor in a descendant must be gone, too.
				before, err := os.ReadFile(filepath.Join(spec.Config.Spec.Dir, "writes"))
				if err != nil {
					return err
				}
				time.Sleep(60 * time.Millisecond)
				after, err := os.ReadFile(filepath.Join(spec.Config.Spec.Dir, "writes"))
				if err != nil {
					return err
				}
				if string(before) != string(after) {
					t.Fatal("descendant wrote after the barrier")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !called || epoch.State().Phase != EpochRevoked {
				t.Fatalf("barrier %+v", epoch.State())
			}
			if err := epoch.End(reason, 0, func(Boundary) error { t.Fatal("successful boundary repeated"); return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestartUsesExactIDAndNewPermissions(t *testing.T) {
	spec := profileFixture(t)
	epoch := startEpoch(t, spec)
	if err := epoch.ConfirmNativeID("exact-native-id"); err != nil {
		t.Fatal(err)
	}
	if err := epoch.ConfirmNativeID("other"); err == nil {
		t.Fatal("conversation identity changed")
	}
	old := epoch.Session()
	if err := epoch.Restart(mustProfile(t, spec), nil); err == nil {
		t.Fatal("restart before barrier succeeded")
	}
	if err := epoch.End("repair", 100*time.Millisecond, func(Boundary) error { return nil }); err != nil {
		t.Fatal(err)
	}
	spec.Epoch = 2
	spec.Role = Repair
	resumed := false
	err := epoch.Restart(mustProfile(t, spec), func(id string) ([]string, error) {
		resumed = true
		if id != "exact-native-id" {
			t.Fatalf("wrong native ID %q", id)
		}
		return []string{"/bin/sh", "-c", `printf 'ID:%s\n' "$1"; if echo forbidden > source; then echo SOURCE_WRITABLE; else echo SOURCE_PROTECTED; fi; if echo forbidden > .git/metadata; then echo GIT_WRITABLE; else echo GIT_PROTECTED; fi; echo artifact > handoff/repair; echo CHECKED; exec /bin/cat`, "resume", id}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	live := epoch.Session()
	output := waitOutput(t, live, "CHECKED")
	if !resumed || live == old || !strings.Contains(output, "ID:exact-native-id") || !strings.Contains(output, "SOURCE_PROTECTED") || !strings.Contains(output, "GIT_PROTECTED") || strings.Contains(output, "SOURCE_WRITABLE") || strings.Contains(output, "GIT_WRITABLE") {
		t.Fatalf("restart output %q", output)
	}
	if _, err := old.Write([]byte("late\r")); err == nil {
		t.Fatal("old PTY accepted input")
	}
	artifact, err := os.ReadFile(filepath.Join(spec.Handoff, "repair"))
	if err != nil || string(artifact) != "artifact\n" {
		t.Fatalf("artifact %q: %v", artifact, err)
	}
	if epoch.State().Epoch != 2 || epoch.State().Phase != EpochActive {
		t.Fatalf("state %+v", epoch.State())
	}
}

func TestFailedBoundaryOrMissingIDCannotRestart(t *testing.T) {
	for _, failure := range []string{"reconcile", "missing reconciler", "missing ID", "resume command"} {
		t.Run(failure, func(t *testing.T) {
			spec := profileFixture(t)
			epoch := startEpoch(t, spec)
			reconcile := Reconcile(func(Boundary) error { return nil })
			if failure == "reconcile" {
				reconcile = func(Boundary) error { return errors.New("Git is inconsistent") }
			}
			if failure == "missing reconciler" {
				reconcile = nil
			}
			if failure == "resume command" {
				if err := epoch.ConfirmNativeID("native"); err != nil {
					t.Fatal(err)
				}
			}
			endErr := epoch.End("pause", 100*time.Millisecond, reconcile)
			spec.Epoch = 2
			called := false
			err := epoch.Restart(mustProfile(t, spec), func(string) ([]string, error) { called = true; return nil, errors.New("driver refused") })
			if err == nil {
				t.Fatal("unsafe restart accepted")
			}
			if failure != "resume command" && called {
				t.Fatal("driver ran without reconciliation or ID")
			}
			if failure == "reconcile" && endErr == nil {
				t.Fatal("reconciliation failure ignored")
			}
			if _, err := epoch.Session().Write([]byte("late\r")); err == nil {
				t.Fatal("blocked session admits input")
			}
			if epoch.State().Phase != EpochBlocked {
				t.Fatalf("state %+v", epoch.State())
			}
		})
	}
}

func TestConcurrentStopWaitsForGroupDeath(t *testing.T) {
	// A session already marked stopped but not yet reaped must not report
	// success to another caller. No process or sandbox is needed for this race.
	s := &Session{phase: Stopped, pid: 2147483647, done: make(chan struct{})}
	var wg sync.WaitGroup
	returned := make(chan struct{})
	wg.Go(func() {
		if err := s.Stop(time.Second); err != nil {
			t.Error(err)
		}
		close(returned)
	})
	select {
	case <-returned:
		t.Fatal("stop returned before the group was reaped")
	case <-time.After(30 * time.Millisecond):
	}
	close(s.done)
	wg.Wait()
}

func TestReviewCannotWriteSourcesOrGitButKeepsPrivateState(t *testing.T) {
	spec := profileFixture(t)
	spec.Role = Review
	home := t.TempDir()
	spec.Config.Spec.Writable = []string{home}
	spec.Config.Spec.Env["HOME"] = home
	spec.Config.Spec.Argv = []string{"/bin/sh", "-c", `if echo x > source; then exit 11; fi; if echo x > .git/metadata; then exit 12; fi; if echo x > handoff/review; then exit 13; fi; echo state > "$HOME/state"; echo REVIEWED`}
	epoch := startEpoch(t, spec)
	waitOutput(t, epoch.Session(), "REVIEWED")
	epoch.Session().Wait()
	if epoch.Session().State().ExitCode != 0 {
		t.Fatal("review had unexpected permissions")
	}
	if _, err := os.Stat(filepath.Join(home, "state")); err != nil {
		t.Fatal(err)
	}
}

func TestCodingProtectsInstructionsNestedUnderSources(t *testing.T) {
	spec := profileFixture(t)
	instructions := filepath.Join(spec.Config.Spec.Dir, "AGENTS.md")
	if err := os.WriteFile(instructions, []byte("immutable"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.Config.Spec.ReadOnly = []string{instructions}
	spec.Config.Spec.Argv = []string{"/bin/sh", "-c", `if echo changed > AGENTS.md; then exit 11; fi; echo code > source; echo CODED`}
	epoch := startEpoch(t, spec)
	waitOutput(t, epoch.Session(), "CODED")
	epoch.Session().Wait()
	contents, err := os.ReadFile(instructions)
	if err != nil || string(contents) != "immutable" {
		t.Fatalf("instructions changed: %q, %v", contents, err)
	}
	if epoch.Session().State().ExitCode != 0 {
		t.Fatal("nested protection was masked")
	}
}

func TestAdmissionRejectsChangedMountPaths(t *testing.T) {
	spec := profileFixture(t)
	profile := mustProfile(t, spec)
	oldDir := spec.Config.Spec.Dir
	if err := os.RemoveAll(oldDir); err != nil {
		t.Fatal(err)
	}
	replacement := t.TempDir()
	for _, name := range []string{".git", "handoff"} {
		if err := os.Mkdir(filepath.Join(replacement, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(replacement, oldDir); err != nil {
		t.Fatal(err)
	}
	// No launcher or process is needed to detect a changed canonical path.
	if _, err := profile.validateAdmission(); err == nil {
		t.Fatal("changed profile mount admitted")
	}
}

func TestSessionRefusesAnUnconfirmedSandbox(t *testing.T) {
	l := sandbox(t)
	if live, err := Start(l, Config{Spec: launcher.Spec{Dir: filepath.Join(t.TempDir(), "missing"), Argv: []string{"/bin/cat"}}}); err == nil {
		_ = live.Stop(100 * time.Millisecond)
		t.Fatal("failed mount treated as a confirmed session")
	}
}

func TestRestartRefusesReusedEpochAndConfirmsReplacement(t *testing.T) {
	spec := profileFixture(t)
	epoch := startEpoch(t, spec)
	if err := epoch.ConfirmNativeID("native"); err != nil {
		t.Fatal(err)
	}
	if err := epoch.End("end", 100*time.Millisecond, func(Boundary) error { return nil }); err != nil {
		t.Fatal(err)
	}
	called := false
	resume := func(string) ([]string, error) { called = true; return []string{"/bin/cat"}, nil }
	if err := epoch.Restart(mustProfile(t, spec), resume); err == nil || called {
		t.Fatal("epoch reused")
	}
	spec.Epoch = 2
	failed := mustProfile(t, spec)
	if err := os.RemoveAll(spec.Handoff); err != nil {
		t.Fatal(err)
	}
	if err := epoch.Restart(failed, resume); err == nil {
		t.Fatal("missing mount admitted")
	}
	if epoch.State().Phase != EpochBlocked {
		t.Fatal("failed replacement not blocked")
	}
}

func TestBoundaryCanStopAnAgentWithBlockedInput(t *testing.T) {
	spec := profileFixture(t)
	spec.Config.Spec.Argv = []string{"/bin/sh", "-c", "stty raw -echo; echo READY; sleep 1000"}
	epoch := startEpoch(t, spec)
	live := epoch.Session()
	waitOutput(t, live, "READY")
	// Resizing must not accidentally restore blocking mode on the master.
	if err := live.Resize(30, 90); err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() { _, err := live.Write(bytes.Repeat([]byte("x"), 1<<20)); writeDone <- err }()
	time.Sleep(30 * time.Millisecond)
	ended := make(chan error, 1)
	go func() { ended <- epoch.End("interrupted", 100*time.Millisecond, func(Boundary) error { return nil }) }()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked input prevented the permissions boundary")
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("blocked write unexpectedly completed")
		}
	case <-time.After(time.Second):
		t.Fatal("old PTY write did not unblock on stop")
	}
}
