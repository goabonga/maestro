// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package testrun

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/launcher"
)

// sandbox returns a runner on a probed launcher, or skips when the host
// cannot confine: the production rule stays refusal.
func sandbox(t *testing.T) *Runner {
	t.Helper()
	l, err := launcher.New()
	if errors.Is(err, launcher.ErrUnsupported) {
		t.Skipf("host cannot confine: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{Launcher: l, Grace: time.Second}
}

// sh is a fixture command: an explicit argv whose script is test text,
// never task text.
func sh(name, script string) Command {
	return Command{Name: name, Argv: []string{"/bin/sh", "-c", script}}
}

func TestRunRecordsEachCommandOnTheExactRevision(t *testing.T) {
	runner := sandbox(t)
	repo, first, _ := repository(t)
	ticks := []time.Time{time.Unix(100, 0), time.Unix(102, 0), time.Unix(200, 0), time.Unix(200, 500e6)}
	runner.Now = func() time.Time {
		now := ticks[0]
		ticks = ticks[1:]
		return now
	}
	clone := filepath.Join(t.TempDir(), "clone")
	run, err := runner.Run(repo, first, clone, []Command{
		sh("read", "cat README.md; echo to-stderr >&2"),
		sh("fail", "exit 3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.TestedSHA != first || run.Clone != clone || len(run.Results) != 2 || run.Passed() {
		t.Fatalf("run %+v", run)
	}
	read, fail := run.Results[0], run.Results[1]
	if read.ExitCode != 0 || read.Output != "first\nto-stderr\n" || read.Duration != 2*time.Second ||
		read.TestedSHA != first || !read.Passed() || read.Argv[0] != "/bin/sh" {
		t.Fatalf("read %+v", read)
	}
	if fail.ExitCode != 3 || fail.Passed() || fail.Duration != 500*time.Millisecond {
		t.Fatalf("fail %+v", fail)
	}
	// The repository itself was never touched.
	if head := runGit(t, repo, "status", "--porcelain"); head != "" {
		t.Fatalf("repository changed: %s", head)
	}
}

func TestRunConfinesTheCommands(t *testing.T) {
	runner := sandbox(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-FAKE0123456789abcdefFAKE")
	repo, _, second := repository(t)
	outside := filepath.Join(t.TempDir(), "outside")
	run, err := runner.Run(repo, second, filepath.Join(t.TempDir(), "clone"), []Command{
		sh("env", "env | sort"),
		sh("network", "awk -F: 'NR > 2 { gsub(/ /, \"\", $1); print $1 }' /proc/net/dev"),
		sh("git-metadata", "echo x > .git/config.lock || echo refused"),
		sh("outside", "echo x > "+outside+" || echo refused"),
		sh("repository", "ls "+repo+" || echo refused"),
	})
	if err != nil {
		t.Fatal(err)
	}
	env := run.Results[0].Output
	if strings.Contains(env, "ANTHROPIC") || !strings.Contains(env, "HOME=/tmp") || !strings.Contains(env, "PATH=") {
		t.Fatalf("environment %q", env)
	}
	if network := strings.TrimSpace(run.Results[1].Output); network != "lo" {
		t.Fatalf("network interfaces %q, want only the loopback", network)
	}
	for _, result := range run.Results[2:] {
		if !strings.Contains(result.Output, "refused") || len(result.Changed) != 0 {
			t.Fatalf("%s escaped: %+v", result.Name, result)
		}
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a command wrote outside its clone: %v", err)
	}
}

func TestRunReportsSourceChangesAndBuildOutputs(t *testing.T) {
	runner := sandbox(t)
	repo, _, second := repository(t)
	run, err := runner.Run(repo, second, filepath.Join(t.TempDir(), "clone"), []Command{
		sh("build", "mkdir -p bin && echo app > bin/app && echo out > out.txt"),
		sh("rewrite", "echo changed > README.md"),
	})
	if err != nil {
		t.Fatal(err)
	}
	build, rewrite := run.Results[0], run.Results[1]
	if !build.Passed() || strings.Join(build.Untracked, ",") != "bin/,out.txt" {
		t.Fatalf("build %+v", build)
	}
	if rewrite.Passed() || rewrite.ExitCode != 0 || strings.Join(rewrite.Changed, ",") != "README.md" {
		t.Fatalf("rewrite %+v", rewrite)
	}
	if run.Passed() {
		t.Fatal("a run that changed the sources passed")
	}
}

func TestRunStopsACommandAtItsTimeout(t *testing.T) {
	runner := sandbox(t)
	runner.MaxOutput = 6
	armed := make(chan time.Duration, 1)
	runner.After = func(d time.Duration) <-chan time.Time {
		armed <- d
		fired := make(chan time.Time, 1)
		fired <- time.Time{}
		return fired
	}
	repo, _, second := repository(t)
	slow := sh("slow", "printf 'started-'; exec sleep 60")
	slow.Timeout = 7 * time.Minute
	run, err := runner.Run(repo, second, filepath.Join(t.TempDir(), "clone"), []Command{slow})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-armed; got != 7*time.Minute {
		t.Fatalf("timeout armed for %s", got)
	}
	result := run.Results[0]
	if !result.TimedOut || result.Passed() || result.ExitCode == 0 {
		t.Fatalf("slow %+v", result)
	}
	if result.Truncated && len(result.Output) > 6 {
		t.Fatalf("output exceeds its bound: %q", result.Output)
	}
}

func TestRunBoundsTheOutput(t *testing.T) {
	runner := sandbox(t)
	runner.MaxOutput = 10
	repo, _, second := repository(t)
	run, err := runner.Run(repo, second, filepath.Join(t.TempDir(), "clone"), []Command{sh("noisy", "printf 0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	if result := run.Results[0]; result.Output != "6789abcdef" || !result.Truncated {
		t.Fatalf("noisy %+v", result)
	}
}

func TestRunRefusesWhatCannotBeTested(t *testing.T) {
	repo, first, _ := repository(t)
	if _, err := (&Runner{}).Run(repo, first, filepath.Join(t.TempDir(), "clone"), []Command{sh("x", "true")}); err == nil {
		t.Fatal("a runner without launcher ran")
	}
	runner := sandbox(t)
	if _, err := runner.Run(repo, first, filepath.Join(t.TempDir(), "clone"), nil); !errors.Is(err, ErrNoCommands) {
		t.Fatalf("no commands: %v", err)
	}
	if _, err := runner.Run(repo, first, filepath.Join(t.TempDir(), "clone"), []Command{{Name: "empty"}}); err == nil {
		t.Fatal("an empty argv ran")
	}
	if _, err := runner.Run(repo, strings.Repeat("d", 40), filepath.Join(t.TempDir(), "clone"), []Command{sh("x", "true")}); !errors.Is(err, ErrCheckout) {
		t.Fatalf("unknown revision: %v", err)
	}
}
