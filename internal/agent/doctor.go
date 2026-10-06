// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Check is the doctor's verdict on one requirement.
type Check struct {
	// Name is the requirement, such as "sandbox" or an agent kind.
	Name string
	// OK is true when Maestro may rely on it.
	OK bool
	// Detail explains the verdict: the version and driver, or why not.
	Detail string
}

// Doctor checks a host: the confinement Maestro needs, then each agent
// kind of the registry — binary found, version read, validated driver
// selected.
type Doctor struct {
	Registry *Registry
	// Sandbox probes the confinement; a nil error means available.
	Sandbox func() error
	// LookPath finds a binary; exec.LookPath when nil.
	LookPath func(string) (string, error)
}

// Run returns one check for the sandbox, then one per agent kind.
func (d Doctor) Run(ctx context.Context) []Check {
	checks := []Check{{Name: "sandbox", OK: true, Detail: "user, mount, PID and network namespaces available"}}
	if err := d.Sandbox(); err != nil {
		checks[0] = Check{Name: "sandbox", Detail: err.Error()}
	}
	for _, kind := range d.Registry.Kinds() {
		checks = append(checks, d.checkAgent(ctx, kind))
	}
	return checks
}

// checkAgent reads one agent's version and selects its driver.
func (d Doctor) checkAgent(ctx context.Context, kind Driver) Check {
	installed, err := d.Registry.Installed(ctx, kind.Kind, d.LookPath)
	if err != nil {
		return Check{Name: kind.Kind, Detail: err.Error()}
	}
	return Check{Name: kind.Kind, OK: true, Detail: installed.Version.String() + " → driver " + installed.Driver.Name}
}

// Installation is the installed binary of an agent kind with the driver
// validated for its version.
type Installation struct {
	// Path is the binary found on PATH.
	Path    string
	Version Version
	Driver  Driver
}

// Installed finds the binary of an agent kind, reads its version and
// selects the driver validated for it; lookPath is exec.LookPath when
// nil. A kind the registry does not know, a missing binary, an
// unreadable version or a version outside every validated range is
// refused with the reason.
func (r *Registry) Installed(ctx context.Context, kind string, lookPath func(string) (string, error)) (Installation, error) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	binary := ""
	for _, known := range r.Kinds() {
		if known.Kind == kind {
			binary = known.Binary
		}
	}
	if binary == "" {
		return Installation{}, fmt.Errorf("%w for agent %q", ErrNoDriver, kind)
	}
	path, err := lookPath(binary)
	if err != nil {
		return Installation{}, errors.New(binary + " not found on PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").Output() // #nosec G204 -- the binary comes from the registry, never from task text
	if err != nil {
		return Installation{}, errors.New(path + " --version failed: " + err.Error())
	}
	version, err := ParseVersion(string(output))
	if err != nil {
		return Installation{}, errors.New(path + ": " + err.Error())
	}
	driver, err := r.Select(kind, version)
	if err != nil {
		return Installation{}, err
	}
	return Installation{Path: path, Version: version, Driver: driver}, nil
}
