// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package session

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
)

// Role selects source and Git permissions for one assignment.
type Role string

const (
	Coding Role = "coding"
	Review Role = "review"
	Repair Role = "repair"
)

// ProfileSpec describes an immutable permissions epoch. All mount paths must
// exist. Extra writable paths are private session state or caches; Repair
// permits only Handoff inside the source tree.
type ProfileSpec struct {
	Epoch    uint64
	Role     Role
	Revision string
	Config   Config
	GitDir   string
	Handoff  string
}

// Profile owns a deep copy of its configuration. No caller can mutate a live
// epoch by retaining or receiving a slice or environment map.
type Profile struct {
	spec   ProfileSpec
	config Config
}

// NewProfile validates and freezes an assignment's mount policy.
func NewProfile(spec ProfileSpec) (Profile, error) {
	if spec.Epoch == 0 || spec.Revision == "" {
		return Profile{}, errors.New("permissions epoch and revision are required")
	}
	if spec.Role != Coding && spec.Role != Review && spec.Role != Repair {
		return Profile{}, errors.New("unknown permissions role")
	}
	if len(spec.Config.Spec.Argv) == 0 {
		return Profile{}, errors.New("profile command is required")
	}
	spec = cloneProfile(spec)
	command := &spec.Config.Spec
	var err error
	command.Dir, err = mountPath(command.Dir)
	if err != nil {
		return Profile{}, err
	}
	spec.GitDir, err = mountPath(spec.GitDir)
	if err != nil {
		return Profile{}, err
	}
	if contains(spec.GitDir, command.Dir) {
		return Profile{}, errors.New("Git metadata cannot contain the source tree")
	}
	for i, path := range command.ReadOnly {
		command.ReadOnly[i], err = mountPath(path)
		if err != nil {
			return Profile{}, err
		}
	}
	for i, path := range command.Writable {
		command.Writable[i], err = mountPath(path)
		if err != nil {
			return Profile{}, err
		}
	}
	if spec.Handoff != "" {
		spec.Handoff, err = mountPath(spec.Handoff)
		if err != nil {
			return Profile{}, err
		}
		if !contains(command.Dir, spec.Handoff) || spec.Handoff == command.Dir || overlaps(spec.GitDir, spec.Handoff) {
			return Profile{}, errors.New("handoff must be a separate directory inside the source tree")
		}
	}
	if spec.Role == Repair && spec.Handoff == "" {
		return Profile{}, errors.New("repair requires its own handoff directory")
	}
	command.DirReadOnly = spec.Role != Coding
	original := cloneProfile(spec)
	if spec.Role == Coding {
		command.Writable = append(command.Writable, spec.GitDir)
	} else {
		command.ReadOnly = append(command.ReadOnly, spec.GitDir)
	}
	for _, writable := range command.Writable {
		for _, readonly := range command.ReadOnly {
			if overlaps(writable, readonly) {
				return Profile{}, fmt.Errorf("writable mount %s overlaps protected mount %s", writable, readonly)
			}
		}
		if spec.Role != Coding && overlaps(writable, command.Dir) {
			return Profile{}, errors.New("extra writable mount overlaps read-only sources")
		}
		if contains(writable, command.Dir) {
			return Profile{}, errors.New("writable mount cannot contain sources")
		}
	}
	if spec.Handoff != "" && spec.Role != Review {
		for _, readonly := range command.ReadOnly {
			if overlaps(spec.Handoff, readonly) {
				return Profile{}, errors.New("handoff overlaps a protected mount")
			}
		}
		command.Writable = append(command.Writable, spec.Handoff)
	}
	return Profile{spec: original, config: spec.Config}, nil
}

// Spec returns an independent copy for diagnostics or serialization.
func (p Profile) Spec() ProfileSpec { return cloneProfile(p.spec) }

func cloneProfile(spec ProfileSpec) ProfileSpec {
	spec.Config.Spec.Argv = slices.Clone(spec.Config.Spec.Argv)
	spec.Config.Spec.ReadOnly = slices.Clone(spec.Config.Spec.ReadOnly)
	spec.Config.Spec.Writable = slices.Clone(spec.Config.Spec.Writable)
	spec.Config.Spec.Env = maps.Clone(spec.Config.Spec.Env)
	return spec
}

func mountPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("mount paths must be absolute")
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve mount: %w", err)
	}
	return filepath.Clean(path), nil
}

func contains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && !hasParentPrefix(relative)
}

func hasParentPrefix(path string) bool {
	return len(path) >= 3 && path[:3] == ".."+string(filepath.Separator)
}

func overlaps(a, b string) bool { return contains(a, b) || contains(b, a) }

// confinedConfig returns another copy; the original command is replaced only
// by a driver constructing an exact native-session resume command.
func (p Profile) confinedConfig(argv []string) Config {
	config := cloneProfile(ProfileSpec{Config: p.config}).Config
	config.Spec.Argv = slices.Clone(argv)
	return config
}

func (p Profile) validateAdmission() (Profile, error) {
	validated, err := NewProfile(p.Spec())
	if err != nil {
		return Profile{}, err
	}
	if !reflect.DeepEqual(validated.spec, p.spec) {
		return Profile{}, errors.New("profile paths changed since construction")
	}
	return validated, nil
}
