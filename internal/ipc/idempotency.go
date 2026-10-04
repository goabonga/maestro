// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/goabonga/maestro/internal/state"
)

// recorder captures a handler's response so it can be persisted.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body.Write(p)
	return r.ResponseWriter.Write(p)
}

// idempotent guards a mutating handler with a persisted key. The first
// request claims the key and stores its response; a retry with the same
// key and body replays that response; the same key with another body is
// a conflict, and a claim still running is reported as in progress.
func idempotent(db *state.DB, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" || len(key) > 128 {
			fail(w, r, http.StatusBadRequest, CodeInvalidRequest, "mutations require an Idempotency-Key header")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			fail(w, r, http.StatusRequestEntityTooLarge, CodeInvalidRequest, "request body too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
		hash := hex.EncodeToString(sum[:])

		// Claim the key atomically; losing the claim means replaying.
		_, err = db.Exec("INSERT INTO idempotency_keys (key, request_hash, created_at) VALUES (?, ?, ?)",
			key, hash, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			var storedHash string
			var status sql.NullInt64
			var stored []byte
			row := db.QueryRow("SELECT request_hash, response_status, response_body FROM idempotency_keys WHERE key = ?", key)
			if scanErr := row.Scan(&storedHash, &status, &stored); scanErr != nil {
				if errors.Is(scanErr, sql.ErrNoRows) {
					fail(w, r, http.StatusInternalServerError, CodeInternal, "idempotency claim failed")
					return
				}
				fail(w, r, http.StatusInternalServerError, CodeInternal, scanErr.Error())
				return
			}
			if storedHash != hash {
				fail(w, r, http.StatusConflict, CodeConflict, "Idempotency-Key already used with a different request")
				return
			}
			if !status.Valid {
				fail(w, r, http.StatusConflict, CodeInProgress, "the request with this Idempotency-Key is still running")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(status.Int64))
			_, _ = w.Write(stored)
			return
		}

		captured := &recorder{ResponseWriter: w}
		next(captured, r)
		if _, err := db.Exec("UPDATE idempotency_keys SET response_status = ?, response_body = ? WHERE key = ?",
			captured.status, captured.body.Bytes(), key); err != nil {
			// The response already reached the client; the claim stays
			// in progress and a later retry reports it.
			return
		}
	}
}
