// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package handoff

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goabonga/maestro/internal/state"
)

// ErrDuplicate reports an artifact id already accepted: artifacts are
// immutable once persisted.
var ErrDuplicate = errors.New("artifact already accepted")

// ErrNone reports a task without any accepted artifact of a kind.
var ErrNone = errors.New("no accepted artifact")

// Stored is an accepted artifact.
type Stored struct {
	Envelope   Envelope
	Digest     string
	ReceivedAt time.Time
	Document   []byte
}

// Accept persists a validated document with its receipt time and the
// SHA-256 of its content. An artifact id is accepted once; a second
// document under the same id is refused, never overwritten.
func Accept(db *state.DB, envelope Envelope, document []byte) (Stored, error) {
	sum := sha256.Sum256(document)
	stored := Stored{
		Envelope:   envelope,
		Digest:     hex.EncodeToString(sum[:]),
		ReceivedAt: time.Now().UTC(),
		Document:   append([]byte(nil), document...),
	}
	var existing string
	err := db.QueryRow("SELECT digest FROM artifacts WHERE artifact_id = ?", envelope.ArtifactID).Scan(&existing)
	if err == nil {
		return Stored{}, fmt.Errorf("%w: %s", ErrDuplicate, envelope.ArtifactID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Stored{}, err
	}
	_, err = db.Exec(`INSERT INTO artifacts
		(artifact_id, kind, task_id, turn_id, attempt_id, worker_id, config_id, digest, received_at, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		envelope.ArtifactID, string(envelope.Kind), envelope.TaskID, envelope.TurnID, envelope.AttemptID,
		envelope.WorkerID, envelope.ConfigID, stored.Digest, stored.ReceivedAt.Format(time.RFC3339Nano), stored.Document)
	if err != nil {
		// A concurrent accept of the same id loses on the primary key.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Stored{}, fmt.Errorf("%w: %s", ErrDuplicate, envelope.ArtifactID)
		}
		return Stored{}, fmt.Errorf("persist artifact %s: %w", envelope.ArtifactID, err)
	}
	return stored, nil
}

// Load returns an accepted artifact by id.
func Load(db *state.DB, artifactID string) (Stored, error) {
	var stored Stored
	var received string
	err := db.QueryRow("SELECT digest, received_at, document FROM artifacts WHERE artifact_id = ?", artifactID).
		Scan(&stored.Digest, &received, &stored.Document)
	if err != nil {
		return Stored{}, err
	}
	stored.ReceivedAt, err = time.Parse(time.RFC3339Nano, received)
	if err != nil {
		return Stored{}, err
	}
	stored.Envelope, _, err = Decode(stored.Document)
	if err != nil {
		return Stored{}, err
	}
	return stored, nil
}

// Latest returns the artifact of a kind last accepted for a task. A
// task without one fails with ErrNone.
func Latest(db *state.DB, taskID string, kind Kind) (Stored, error) {
	var artifactID string
	err := db.QueryRow(`SELECT artifact_id FROM artifacts WHERE task_id = ? AND kind = ?
		ORDER BY rowid DESC LIMIT 1`, taskID, string(kind)).Scan(&artifactID)
	if errors.Is(err, sql.ErrNoRows) {
		return Stored{}, fmt.Errorf("%w: %s for task %s", ErrNone, kind, taskID)
	}
	if err != nil {
		return Stored{}, err
	}
	return Load(db, artifactID)
}
