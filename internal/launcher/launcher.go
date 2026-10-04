// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package launcher confines agent, test and command processes. Every
// start goes through user, mount, PID and network namespaces
// (Bubblewrap) with inherited resource limits, and each group dies as
// a unit: its processes live in their own PID namespace, so killing
// the group's init kills every descendant — a kernel guarantee, not a
// best effort. No confinement, no start; there is no permissive
// fallback mode.
package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
)

// ErrUnsupported reports a host that cannot confine: Maestro refuses
// to start work on it.
var ErrUnsupported = errors.New("sandboxing is not available on this host")

// Limits bounds a group. The limits are set before the sandbox starts
// and inherited by every descendant.
type Limits struct {
	// CPUSeconds bounds the CPU time of each process.
	CPUSeconds uint64
	// MemoryBytes bounds the address space of each process.
	MemoryBytes uint64
	// Processes bounds the number of processes of the sandboxed user.
	Processes uint64
}

// DefaultLimits are deliberately generous bounds against runaways.
var DefaultLimits = Limits{CPUSeconds: 3600, MemoryBytes: 4 << 30, Processes: 512}

// Spec describes one confined group.
type Spec struct {
	// Argv is the explicit command line; never a shell string built
	// from task text.
	Argv []string
	// Dir is the group's project path, bound at the same path and used
	// as the working directory; writable unless DirReadOnly is set.
	Dir string
	// DirReadOnly binds Dir read-only: a group that may read the sources
	// but must not change them (end of turn, review, repair).
	DirReadOnly bool
	// ReadOnly lists extra paths bound read-only.
	ReadOnly []string
	// Writable lists extra paths bound writable (private HOME, tmp).
	Writable []string
	// Env is the full environment, an allow-list: nothing is
	// inherited from the daemon.
	Env map[string]string
	// Network keeps the network namespace shared; the default is an
	// isolated namespace with only a loopback.
	Network bool
	// Limits bounds the group; the zero value means DefaultLimits.
	Limits Limits
}

// Launcher starts confined groups once the host proved it can.
type Launcher struct {
	bwrap   string
	prlimit string
}

// New probes the host: the sandbox and limit tools must exist and a
// canary must run inside the full namespace set. Any failure is
// ErrUnsupported — never a degraded mode.
func New() (*Launcher, error) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("%w: bwrap not found", ErrUnsupported)
	}
	prlimit, err := exec.LookPath("prlimit")
	if err != nil {
		return nil, fmt.Errorf("%w: prlimit not found", ErrUnsupported)
	}
	launcher := &Launcher{bwrap: bwrap, prlimit: prlimit}
	canary := Spec{Argv: []string{"/bin/true"}, Dir: os.TempDir()}
	command, err := launcher.command(canary)
	if err != nil {
		return nil, err
	}
	if output, err := command.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w: the confinement canary failed: %v: %s", ErrUnsupported, err, output)
	}
	return launcher, nil
}

// arguments renders the full argv of a confined group.
func (l *Launcher) arguments(spec Spec) ([]string, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("empty command")
	}
	if spec.Dir == "" {
		return nil, errors.New("a confined group needs a working directory")
	}
	limits := spec.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits
	}
	args := []string{
		l.bwrap,
		"--die-with-parent",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
	}
	if !spec.Network {
		args = append(args, "--unshare-net")
	}
	// A read-only system, a namespace-local /proc, a private /dev and
	// /tmp: no host socket or device is exposed.
	for _, path := range []string{"/usr", "/bin", "/lib", "/lib64", "/etc/alternatives"} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	for _, path := range spec.ReadOnly {
		args = append(args, "--ro-bind", path, path)
	}
	if spec.DirReadOnly {
		args = append(args, "--ro-bind", spec.Dir, spec.Dir)
	} else {
		args = append(args, "--bind", spec.Dir, spec.Dir)
	}
	for _, path := range spec.Writable {
		args = append(args, "--bind", path, path)
	}
	args = append(args, "--chdir", spec.Dir, "--clearenv")
	keys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--setenv", key, spec.Env[key])
	}
	// The limits are set inside the user namespace: there the process
	// count starts from the namespace, not from the host user's total,
	// and every descendant inherits all three bounds.
	args = append(args, "--",
		l.prlimit,
		"--cpu="+strconv.FormatUint(limits.CPUSeconds, 10),
		"--as="+strconv.FormatUint(limits.MemoryBytes, 10),
		"--nproc="+strconv.FormatUint(limits.Processes, 10),
		"--")
	args = append(args, spec.Argv...)
	return args, nil
}

// Command builds the exec.Cmd of a confined group, for callers that
// attach their own terminal or pipes before starting it. The returned
// command is fully confined; its environment lives inside the sandbox
// specification, never in the process environment.
func (l *Launcher) Command(spec Spec) (*exec.Cmd, error) {
	return l.command(spec)
}

// command builds the exec.Cmd of a confined group.
func (l *Launcher) command(spec Spec) (*exec.Cmd, error) {
	args, err := l.arguments(spec)
	if err != nil {
		return nil, err
	}
	command := exec.Command(args[0], args[1:]...) // #nosec G204 -- argv is built by Maestro from its own configuration, never from task text
	command.Env = []string{}
	return command, nil
}
