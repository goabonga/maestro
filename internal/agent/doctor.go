// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package agent

import (
	"context"
	"os/exec"
	"strings"
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
	lookPath := d.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	checks := []Check{{Name: "sandbox", OK: true, Detail: "user, mount, PID and network namespaces available"}}
	if err := d.Sandbox(); err != nil {
		checks[0] = Check{Name: "sandbox", Detail: err.Error()}
	}
	for _, kind := range d.Registry.Kinds() {
		checks = append(checks, d.checkAgent(ctx, kind, lookPath))
	}
	return checks
}

// checkAgent reads one agent's version and selects its driver.
func (d Doctor) checkAgent(ctx context.Context, kind Driver, lookPath func(string) (string, error)) Check {
	path, err := lookPath(kind.Binary)
	if err != nil {
		return Check{Name: kind.Kind, Detail: kind.Binary + " not found on PATH"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").Output() // #nosec G204 -- the binary comes from the registry, never from task text
	if err != nil {
		return Check{Name: kind.Kind, Detail: path + " --version failed: " + err.Error()}
	}
	version, err := ParseVersion(string(output))
	if err != nil {
		return Check{Name: kind.Kind, Detail: path + ": " + err.Error()}
	}
	driver, err := d.Registry.Select(kind.Kind, version)
	if err != nil {
		return Check{Name: kind.Kind, Detail: strings.TrimSpace(err.Error())}
	}
	return Check{Name: kind.Kind, OK: true, Detail: version.String() + " → driver " + driver.Name}
}
