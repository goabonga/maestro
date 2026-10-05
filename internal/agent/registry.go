// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package agent knows which agent CLIs Maestro may drive: a registry
// maps each kind of agent and a validated version range to a driver,
// and the doctor checks a host against it. An unknown version is
// refused, never driven blindly.
package agent

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// ErrNoDriver reports a kind or version no driver is validated for.
var ErrNoDriver = errors.New("no validated driver")

// Version is a semantic version.
type Version struct {
	Major, Minor, Patch int
}

// versionPattern finds the first semantic version in a version line.
var versionPattern = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// ParseVersion extracts the first semantic version from a CLI's
// version output, such as "2.1.289 (Claude Code)" or "codex-cli 0.160.0".
func ParseVersion(output string) (Version, error) {
	match := versionPattern.FindStringSubmatch(output)
	if match == nil {
		return Version{}, fmt.Errorf("no version in %q", output)
	}
	var parts [3]int
	for i := range parts {
		value, err := strconv.Atoi(match[i+1])
		if err != nil {
			return Version{}, fmt.Errorf("bad version %q: %w", match[0], err)
		}
		parts[i] = value
	}
	return Version{Major: parts[0], Minor: parts[1], Patch: parts[2]}, nil
}

// String renders the version.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare returns -1, 0 or 1 as v is lower than, equal to or higher
// than other.
func (v Version) Compare(other Version) int {
	for _, pair := range [][2]int{{v.Major, other.Major}, {v.Minor, other.Minor}, {v.Patch, other.Patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

// Driver is one validated way of driving a kind of agent, bounded to
// the versions its validation matrix covered.
type Driver struct {
	// Kind names the agent, such as "claude-code".
	Kind string
	// Name identifies the driver.
	Name string
	// Binary is the executable looked up on PATH.
	Binary string
	// Min and Max bound the validated versions, both included.
	Min, Max Version
}

// Covers reports whether the driver is validated for a version.
func (d Driver) Covers(version Version) bool {
	return version.Compare(d.Min) >= 0 && version.Compare(d.Max) <= 0
}

// Registry maps kinds and version ranges to drivers.
type Registry struct {
	drivers []Driver
}

// NewRegistry validates and holds a set of drivers: bounds must be
// ordered and the ranges of one kind must not overlap, so a version
// selects at most one driver.
func NewRegistry(drivers ...Driver) (*Registry, error) {
	for i, driver := range drivers {
		if driver.Kind == "" || driver.Name == "" || driver.Binary == "" {
			return nil, fmt.Errorf("driver %d: kind, name and binary are required", i)
		}
		if driver.Min.Compare(driver.Max) > 0 {
			return nil, fmt.Errorf("driver %s: minimum %s above maximum %s", driver.Name, driver.Min, driver.Max)
		}
		for _, other := range drivers[:i] {
			if other.Kind == driver.Kind && driver.Min.Compare(other.Max) <= 0 && other.Min.Compare(driver.Max) <= 0 {
				return nil, fmt.Errorf("drivers %s and %s overlap for %s", other.Name, driver.Name, driver.Kind)
			}
		}
	}
	return &Registry{drivers: append([]Driver(nil), drivers...)}, nil
}

// Drivers returns every registered driver, in registration order.
func (r *Registry) Drivers() []Driver {
	return append([]Driver(nil), r.drivers...)
}

// Kinds returns the kinds the registry knows, in registration order.
func (r *Registry) Kinds() []Driver {
	seen := map[string]bool{}
	var kinds []Driver
	for _, driver := range r.drivers {
		if !seen[driver.Kind] {
			seen[driver.Kind] = true
			kinds = append(kinds, driver)
		}
	}
	return kinds
}

// Select returns the driver validated for a kind and version, or an
// explicit refusal naming the validated ranges.
func (r *Registry) Select(kind string, version Version) (Driver, error) {
	var ranges []string
	for _, driver := range r.drivers {
		if driver.Kind != kind {
			continue
		}
		if driver.Covers(version) {
			return driver, nil
		}
		ranges = append(ranges, driver.Min.String()+"–"+driver.Max.String())
	}
	if len(ranges) == 0 {
		return Driver{}, fmt.Errorf("%w for agent %q", ErrNoDriver, kind)
	}
	return Driver{}, fmt.Errorf("%w for %s %s (validated: %v)", ErrNoDriver, kind, version, ranges)
}

// Builtin is the registry of drivers validated by the driver
// validation matrix: each range covers only the versions recorded
// and passed there.
func Builtin() *Registry {
	registry, err := NewRegistry(
		Driver{Kind: "claude-code", Name: "claude-code-2.1", Binary: "claude",
			Min: Version{2, 1, 289}, Max: Version{2, 1, 289}},
		Driver{Kind: "codex", Name: "codex-0.160", Binary: "codex",
			Min: Version{0, 160, 0}, Max: Version{0, 160, 0}},
	)
	if err != nil {
		panic(err)
	}
	return registry
}
