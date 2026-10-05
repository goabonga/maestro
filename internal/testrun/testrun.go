// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package testrun runs a project's configured test commands against one
// exact revision. The revision is checked out into a private clone; each
// command runs as an explicit argv in its own confined group, without
// network, with an allow-listed environment, a timeout and a bounded
// output capture. The clone's Git metadata is read-only to the commands,
// and the tracked files and the index are verified after each command.
// Results become TEST_REPORT handoff documents bound to the tested SHA.
package testrun

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/launcher"
)

// Defaults of a Runner left at its zero value.
const (
	// DefaultTimeout bounds a command whose configuration sets none.
	DefaultTimeout = 30 * time.Minute
	// DefaultMaxOutput bounds the captured output of one command.
	DefaultMaxOutput = 256 << 10
	// DefaultGrace is the delay between SIGTERM and SIGKILL on timeout.
	DefaultGrace = 5 * time.Second
)

var (
	// ErrNoCommands reports a run without any test command.
	ErrNoCommands = errors.New("no test command configured")
	// ErrCheckout reports a revision that cannot be checked out exactly.
	ErrCheckout = errors.New("cannot check out the tested revision")
)

// Command is one test command, taken from configuration.
type Command struct {
	Name string
	// Argv is the explicit command line; it is never given to a shell.
	Argv []string
	// Timeout bounds the run; zero means the runner's default.
	Timeout time.Duration
}

// Commands lists the test commands of a configuration, sorted by name.
func Commands(c config.Config) []Command {
	commands := make([]Command, 0, len(c.Tests))
	for name, test := range c.Tests {
		command := Command{Name: name, Argv: append([]string(nil), test.Argv...)}
		if test.Timeout != nil {
			command.Timeout = test.Timeout.Duration
		}
		commands = append(commands, command)
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].Name < commands[j].Name })
	return commands
}

// Environment returns the allow-listed environment of every test
// command. Nothing is inherited from the daemon, so no credential or API
// key reaches a test.
func Environment() map[string]string {
	return map[string]string{
		"PATH":   "/usr/local/bin:/usr/bin:/bin",
		"HOME":   "/tmp",
		"TMPDIR": "/tmp",
		"LANG":   "C.UTF-8",
		"TERM":   "dumb",
	}
}

// Result is the outcome of one command on the tested revision.
type Result struct {
	Name      string
	Argv      []string
	TestedSHA string
	ExitCode  int
	Duration  time.Duration
	// Output is the tail of the combined stdout and stderr, bounded.
	Output    string
	Truncated bool
	TimedOut  bool
	// Changed lists tracked paths or index entries changed by the run.
	Changed []string
	// Untracked lists files left outside version control (build
	// outputs), ignored ones included.
	Untracked []string
}

// Passed reports a command that exited 0 in time without changing the
// tracked files or the index.
func (r Result) Passed() bool {
	return r.ExitCode == 0 && !r.TimedOut && len(r.Changed) == 0
}

// Run is the outcome of every command on one revision.
type Run struct {
	TestedSHA string
	// Clone is the private clone the commands ran in; the caller keeps
	// it for diagnosis or removes it.
	Clone   string
	Results []Result
}

// Passed reports a run whose every command passed.
func (r Run) Passed() bool {
	if len(r.Results) == 0 {
		return false
	}
	for _, result := range r.Results {
		if !result.Passed() {
			return false
		}
	}
	return true
}

// Runner runs test commands in confined groups.
type Runner struct {
	Launcher *launcher.Launcher
	// Limits bounds each group; the zero value means the launcher's
	// defaults.
	Limits launcher.Limits
	// MaxOutput bounds the captured output of one command.
	MaxOutput int
	// DefaultTimeout applies to commands without their own timeout.
	DefaultTimeout time.Duration
	// Grace is the delay between SIGTERM and SIGKILL on timeout.
	Grace time.Duration
	// Now is the clock measuring durations; nil means time.Now.
	Now func() time.Time
	// After arms a command's timeout; nil means time.After.
	After func(time.Duration) <-chan time.Time
}

// Run checks out sha from repository into a new private clone at clone
// (which must not exist yet), then runs the commands there in order.
// A command failure is a result, not an error; errors report a run that
// could not happen or could not be verified.
func (r *Runner) Run(repository, sha, clone string, commands []Command) (Run, error) {
	if r.Launcher == nil {
		return Run{}, errors.New("a test runner needs a launcher")
	}
	if len(commands) == 0 {
		return Run{}, ErrNoCommands
	}
	for _, command := range commands {
		if len(command.Argv) == 0 || command.Argv[0] == "" {
			return Run{}, fmt.Errorf("test command %q has no program", command.Name)
		}
	}
	if err := checkout(repository, sha, clone); err != nil {
		return Run{}, err
	}
	baseline, err := indexEntries(clone)
	if err != nil {
		return Run{}, err
	}
	run := Run{TestedSHA: sha, Clone: clone}
	for _, command := range commands {
		result, err := r.runOne(clone, sha, command)
		if err != nil {
			return run, fmt.Errorf("test %s: %w", command.Name, err)
		}
		result.Changed, result.Untracked, err = inspect(clone, sha, baseline)
		if err != nil {
			return run, fmt.Errorf("test %s: %w", command.Name, err)
		}
		run.Results = append(run.Results, result)
	}
	return run, nil
}

// runOne runs one command in its own confined group.
func (r *Runner) runOne(clone, sha string, command Command) (Result, error) {
	now, after := r.Now, r.After
	if now == nil {
		now = time.Now
	}
	if after == nil {
		after = time.After
	}
	timeout := command.Timeout
	if timeout <= 0 {
		timeout = r.DefaultTimeout
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	grace := r.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}
	limit := r.MaxOutput
	if limit <= 0 {
		limit = DefaultMaxOutput
	}
	output := &tail{max: limit}
	spec := launcher.Spec{
		Argv: command.Argv,
		Dir:  clone,
		// The commands may build in the sources, never rewrite history,
		// the index or the clone's configuration.
		ReadOnly: []string{filepath.Join(clone, ".git")},
		Env:      Environment(),
		Limits:   r.Limits,
	}
	result := Result{Name: command.Name, Argv: append([]string(nil), command.Argv...), TestedSHA: sha}
	started := now()
	group, err := r.Launcher.Start(spec, nil, output, output)
	if err != nil {
		return Result{}, err
	}
	done := make(chan error, 1)
	go func() { done <- group.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-after(timeout):
		result.TimedOut = true
		if err := group.Stop(grace); err != nil {
			return Result{}, err
		}
		waitErr = <-done
	}
	result.Duration = now().Sub(started)
	result.Output, result.Truncated = output.String()
	var exit *exec.ExitError
	switch {
	case waitErr == nil:
		result.ExitCode = 0
	case errors.As(waitErr, &exit):
		result.ExitCode = exit.ExitCode()
	default:
		return Result{}, waitErr
	}
	return result, nil
}
