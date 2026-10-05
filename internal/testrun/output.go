// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package testrun

import (
	"strings"
	"sync"
)

// tail keeps the last max bytes written to it: a failing test reports
// its failure at the end of its output.
type tail struct {
	mu        sync.Mutex
	max       int
	data      []byte
	truncated bool
}

// Write keeps the newest bytes within the bound.
func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if excess := len(t.data) - t.max; excess > 0 {
		t.data = append(t.data[:0], t.data[excess:]...)
		t.truncated = true
	}
	return len(p), nil
}

// String returns the kept output as valid UTF-8, and whether older
// output was dropped.
func (t *tail) String() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.data), "�"), t.truncated
}
