// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package launcher

import (
	"errors"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Group is one running confined group. Its processes live in a
// private PID namespace whose init is the sandbox itself, so the death
// of the group's leader takes every descendant with it.
type Group struct {
	cmd      *exec.Cmd
	waitOnce sync.Once
	waitErr  error
}

// Start launches a confined group. Stdin, stdout and stderr may be
// nil.
func (l *Launcher) Start(spec Spec, stdin io.Reader, stdout, stderr io.Writer) (*Group, error) {
	command, err := l.command(spec)
	if err != nil {
		return nil, err
	}
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	// A process group of its own, so a stop signals the leader even
	// when the daemon shares its terminal.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &Group{cmd: command}, nil
}

// Wait blocks until the group's leader exits, meaning its whole PID
// namespace is gone.
func (g *Group) Wait() error {
	g.waitOnce.Do(func() { g.waitErr = g.cmd.Wait() })
	return g.waitErr
}

// Stop terminates the group: SIGTERM to the leader's process group,
// then SIGKILL after the grace period. It returns once the leader is
// reaped — at that point the kernel has torn the PID namespace down,
// so no descendant can keep modifying sources.
func (g *Group) Stop(grace time.Duration) error {
	_ = syscall.Kill(-g.cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(grace):
	}
	_ = syscall.Kill(-g.cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("the group did not die after SIGKILL")
	}
}
