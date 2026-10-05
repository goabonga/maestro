// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package config

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goabonga/maestro/internal/agent"
	"github.com/goabonga/maestro/internal/handoff"
	"github.com/goabonga/maestro/internal/state"
)

// InstructionsDir holds a project's versioned instruction files.
const InstructionsDir = "maestro"

// Bounds of the instruction files a snapshot carries.
const (
	MaxInstructionFile  = 1 << 20
	MaxInstructionTotal = 16 << 20
)

// DriverRange is one validated driver as the snapshot records it.
type DriverRange struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Min  string `json:"min"`
	Max  string `json:"max"`
}

// Snapshot freezes everything a task's behavior depends on: the
// effective configuration, the instruction files, the contract version
// and the validated drivers. It holds no secret: configuration only
// names the variables that carry them.
type Snapshot struct {
	Config        Config            `json:"config"`
	Instructions  map[string]string `json:"instructions"`
	HandoffSchema int               `json:"handoff_schema"`
	Drivers       []DriverRange     `json:"drivers"`
}

// Take builds the snapshot of a repository as it is now.
func Take(repository string) (Snapshot, error) {
	config, err := Load(repository)
	if err != nil {
		return Snapshot{}, err
	}
	instructions, err := readInstructions(repository)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{Config: config, Instructions: instructions, HandoffSchema: handoff.SchemaVersion}
	for _, driver := range agent.Builtin().Drivers() {
		snapshot.Drivers = append(snapshot.Drivers, DriverRange{
			Kind: driver.Kind, Name: driver.Name, Min: driver.Min.String(), Max: driver.Max.String(),
		})
	}
	return snapshot, nil
}

// Encode returns the canonical form of the snapshot and its config_id,
// the SHA-256 of that form: equal snapshots share one id.
func (s Snapshot) Encode() (string, []byte, error) {
	document, err := json.Marshal(s)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(document)
	return "sha256-" + hex.EncodeToString(sum[:]), document, nil
}

// readInstructions reads the regular files under maestro/, by path
// relative to it. Links, special files, binary or oversized content and
// anything shaped like a secret are refused.
func readInstructions(repository string) (map[string]string, error) {
	instructions := map[string]string{}
	root, err := os.OpenRoot(repository)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(InstructionsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return instructions, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, invalid("%s/ must be a directory", InstructionsDir)
	}
	total := 0
	err = fs.WalkDir(root.FS(), InstructionsDir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return invalid("%s is not a regular file", name)
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		content, err := io.ReadAll(io.LimitReader(file, MaxInstructionFile+1))
		_ = file.Close()
		if err != nil {
			return err
		}
		if len(content) > MaxInstructionFile {
			return invalid("%s exceeds %d bytes", name, MaxInstructionFile)
		}
		if total += len(content); total > MaxInstructionTotal {
			return invalid("instruction files exceed %d bytes in total", MaxInstructionTotal)
		}
		if !utf8.Valid(content) {
			return invalid("%s is not UTF-8 text", name)
		}
		for _, pattern := range secretValues {
			if pattern.Match(content) {
				return invalid("%s holds a value shaped like a secret", name)
			}
		}
		instructions[strings.TrimPrefix(name, InstructionsDir+"/")] = string(content)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return instructions, nil
}

// Persist stores a snapshot under its config_id. Storing the same
// snapshot again is a no-op; a stored snapshot never changes.
func Persist(db *state.DB, snapshot Snapshot) (string, error) {
	id, document, err := snapshot.Encode()
	if err != nil {
		return "", err
	}
	_, err = db.Exec("INSERT INTO config_snapshots (config_id, created_at, document) VALUES (?, ?, ?) ON CONFLICT (config_id) DO NOTHING",
		id, time.Now().UTC().Format(time.RFC3339Nano), document)
	if err != nil {
		return "", fmt.Errorf("persist snapshot %s: %w", id, err)
	}
	return id, nil
}

// LoadSnapshot returns a stored snapshot, checking that its content
// still hashes to its id.
func LoadSnapshot(db *state.DB, id string) (Snapshot, error) {
	var document []byte
	if err := db.QueryRow("SELECT document FROM config_snapshots WHERE config_id = ?", id).Scan(&document); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Snapshot{}, fmt.Errorf("unknown config_id %s", id)
		}
		return Snapshot{}, err
	}
	sum := sha256.Sum256(document)
	if "sha256-"+hex.EncodeToString(sum[:]) != id {
		return Snapshot{}, fmt.Errorf("snapshot %s does not match its content", id)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(document, &snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}
