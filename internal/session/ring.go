// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package session

import "sync"

// ring is a bounded output buffer: it keeps the last capacity bytes
// and silently drops the oldest, so a session can never grow the
// daemon's memory unboundedly.
type ring struct {
	mu     sync.Mutex
	buf    []byte
	start  int
	length int
	total  uint64
}

func newRing(capacity int) *ring {
	return &ring{buf: make([]byte, capacity)}
}

// Write keeps the newest bytes, dropping the oldest on overflow.
func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total += uint64(len(p))
	capacity := len(r.buf)
	if len(p) >= capacity {
		copy(r.buf, p[len(p)-capacity:])
		r.start = 0
		r.length = capacity
		return len(p), nil
	}
	for _, b := range p {
		index := (r.start + r.length) % capacity
		r.buf[index] = b
		if r.length < capacity {
			r.length++
		} else {
			r.start = (r.start + 1) % capacity
		}
	}
	return len(p), nil
}

// Snapshot returns a copy of the kept bytes and the total ever written.
func (r *ring) Snapshot() ([]byte, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, r.length)
	for i := 0; i < r.length; i++ {
		out[i] = r.buf[(r.start+i)%len(r.buf)]
	}
	return out, r.total
}
