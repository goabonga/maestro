// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package handoff

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/goabonga/maestro/internal/state"
)

func TestAcceptPersistsImmutably(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	data := planDocument(t, nil)
	envelope, _, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := Accept(db, envelope, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Digest) != 64 || stored.ReceivedAt.IsZero() {
		t.Fatalf("stored %+v", stored)
	}
	loaded, err := Load(db, "art-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Digest != stored.Digest || !bytes.Equal(loaded.Document, data) || loaded.Envelope.TurnID != "turn-1" {
		t.Fatalf("loaded %+v", loaded)
	}
	other := planDocument(t, func(e map[string]any) { e["turn_id"] = "turn-2" })
	envelope, _, err = Decode(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Accept(db, envelope, other); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a second document under the same id: %v", err)
	}
	if reloaded, err := Load(db, "art-1"); err != nil || reloaded.Envelope.TurnID != "turn-1" {
		t.Fatalf("the accepted artifact changed: %+v %v", reloaded, err)
	}
}

func TestLatestReturnsTheLastAcceptedArtifactOfAKind(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := Latest(db, "task-1", Plan); !errors.Is(err, ErrNone) {
		t.Fatalf("no plan yet: %v", err)
	}
	for _, id := range []string{"art-1", "art-2"} {
		data := planDocument(t, func(e map[string]any) { e["artifact_id"] = id })
		envelope, _, err := Decode(data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Accept(db, envelope, data); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := Latest(db, "task-1", Plan)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Envelope.ArtifactID != "art-2" {
		t.Fatalf("latest plan %s", latest.Envelope.ArtifactID)
	}
	if _, err := Latest(db, "task-1", Review); !errors.Is(err, ErrNone) {
		t.Fatalf("no review: %v", err)
	}
	if _, err := Latest(db, "task-2", Plan); !errors.Is(err, ErrNone) {
		t.Fatalf("another task: %v", err)
	}
}
