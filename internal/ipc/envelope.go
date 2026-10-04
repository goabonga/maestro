// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package ipc is the daemon's versioned JSON API over the Unix socket:
// bounded bodies, a stable envelope with request_id, and persisted
// idempotency keys so a client retry never repeats a mutation.
package ipc

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
)

// maxBody bounds every request body.
const maxBody = 1 << 20

// Error codes of the stable error envelope.
const (
	CodeInvalidRequest = "invalid_request"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeInProgress     = "in_progress"
	CodeInternal       = "internal"
)

// Envelope is the body of every response: data on success, error
// otherwise, always with the request identifier.
type Envelope struct {
	RequestID string      `json:"request_id"`
	Data      interface{} `json:"data,omitempty"`
	Error     *Problem    `json:"error,omitempty"`
}

// Problem is the stable error envelope.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// requestID returns the client's X-Request-Id or generates one.
func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); id != "" && len(id) <= 128 {
		return id
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}

// reply writes the success envelope.
func reply(w http.ResponseWriter, r *http.Request, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Envelope{RequestID: requestID(r), Data: data})
}

// fail writes the error envelope.
func fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Envelope{RequestID: requestID(r), Error: &Problem{Code: code, Message: message}})
}
