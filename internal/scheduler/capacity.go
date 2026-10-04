// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package scheduler owns the daemon's global capacity: machine-wide
// ceilings on concurrent sessions, test runs and commands, reserved
// atomically and never overallocated across projects.
package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Kind is one bounded resource.
type Kind string

// The bounded resources.
const (
	Sessions Kind = "sessions"
	Tests    Kind = "tests"
	Commands Kind = "commands"
)

// Default ceilings of the daemon.
const (
	DefaultSessions = 3
	DefaultTests    = 1
	DefaultCommands = 3
)

// ErrFull reports an exhausted ceiling; the caller queues, it never
// overallocates.
var ErrFull = errors.New("capacity exhausted")

// Capacity holds the ceilings and the live reservations.
type Capacity struct {
	mu     sync.Mutex
	limits map[Kind]int
	used   map[Kind]map[string]int
}

// NewCapacity validates the ceilings and returns an empty capacity.
func NewCapacity(sessions, tests, commands int) (*Capacity, error) {
	limits := map[Kind]int{Sessions: sessions, Tests: tests, Commands: commands}
	for kind, limit := range limits {
		if limit < 1 {
			return nil, fmt.Errorf("the %s ceiling must be at least 1, got %d", kind, limit)
		}
	}
	return &Capacity{
		limits: limits,
		used:   map[Kind]map[string]int{Sessions: {}, Tests: {}, Commands: {}},
	}, nil
}

// Slot is one held reservation. Releasing it twice is harmless.
type Slot struct {
	capacity *Capacity
	kind     Kind
	project  string
	once     sync.Once
}

// Reserve atomically takes one slot of a kind for a project, or
// reports ErrFull without taking anything.
func (c *Capacity) Reserve(kind Kind, project string) (*Slot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	used, ok := c.used[kind]
	if !ok {
		return nil, fmt.Errorf("unknown capacity kind: %s", kind)
	}
	total := 0
	for _, count := range used {
		total += count
	}
	if total >= c.limits[kind] {
		return nil, fmt.Errorf("%s: %w (%d of %d in use)", kind, ErrFull, total, c.limits[kind])
	}
	used[project]++
	return &Slot{capacity: c, kind: kind, project: project}, nil
}

// Release frees the slot.
func (s *Slot) Release() {
	s.once.Do(func() {
		s.capacity.mu.Lock()
		defer s.capacity.mu.Unlock()
		used := s.capacity.used[s.kind]
		used[s.project]--
		if used[s.project] <= 0 {
			delete(used, s.project)
		}
	})
}

// KindUsage is the live consumption of one resource.
type KindUsage struct {
	Used      int            `json:"used"`
	Limit     int            `json:"limit"`
	ByProject map[string]int `json:"by_project,omitempty"`
}

// Usage returns consumption and ceilings, with the per-project detail.
func (c *Capacity) Usage() map[Kind]KindUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	usage := make(map[Kind]KindUsage, len(c.limits))
	for kind, limit := range c.limits {
		detail := make(map[string]int, len(c.used[kind]))
		total := 0
		for project, count := range c.used[kind] {
			detail[project] = count
			total += count
		}
		usage[kind] = KindUsage{Used: total, Limit: limit, ByProject: detail}
	}
	return usage
}

// Kinds returns the bounded resources in a stable order.
func Kinds() []Kind {
	kinds := []Kind{Sessions, Tests, Commands}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}
