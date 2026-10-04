// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package fixture records PTY sessions as replayable fixtures, strips
// secrets from them, and replays them with their expected outcomes, so
// driver behavior can be tested without the real agent CLIs.
package fixture

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Format names the fixture file format.
const Format = "maestro-pty-fixture/1"

// Event kinds.
const (
	// Output is terminal output produced by the recorded process.
	Output = "output"
	// Input is a keystroke chunk written to the terminal.
	Input = "input"
	// Resize is a terminal size change.
	Resize = "resize"
	// Exit is the end of the recorded process.
	Exit = "exit"
)

// Event is one timed step of a recorded session.
type Event struct {
	// AtMillis is the offset from the start of the recording.
	AtMillis int64  `json:"at_ms"`
	Kind     string `json:"kind"`
	// Data carries output or input bytes.
	Data []byte `json:"data,omitempty"`
	// Rows and Cols carry a resize.
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	// ExitCode and Phase carry the end of the process.
	ExitCode int    `json:"exit_code,omitempty"`
	Phase    string `json:"phase,omitempty"`
}

// At returns the event offset as a duration.
func (e Event) At() time.Duration {
	return time.Duration(e.AtMillis) * time.Millisecond
}

// Checkpoint is an expected driver state at a point of the replay.
type Checkpoint struct {
	AtMillis int64  `json:"at_ms"`
	State    string `json:"state"`
}

// Header describes a fixture: what was recorded, and what a driver
// replaying it must conclude.
type Header struct {
	Format  string `json:"format"`
	Agent   string `json:"agent"`
	Version string `json:"version"`
	Case    string `json:"case"`
	Rows    uint16 `json:"rows"`
	Cols    uint16 `json:"cols"`
	// Sanitized is set once secrets were stripped; Save refuses a
	// fixture without it.
	Sanitized bool         `json:"sanitized"`
	Expected  []Checkpoint `json:"expected,omitempty"`
}

// Fixture is one recorded session.
type Fixture struct {
	Header Header
	Events []Event
}

// Save writes the fixture as JSON Lines: the header, then one event per
// line. A fixture that is not sanitized, or that still contains a
// detectable secret, is refused.
func Save(path string, fixture Fixture) error {
	if !fixture.Header.Sanitized {
		return errors.New("refusing to save an unsanitized fixture")
	}
	if findings := Verify(fixture); len(findings) > 0 {
		return fmt.Errorf("refusing to save a fixture with %d detectable secrets: %v", len(findings), findings)
	}
	fixture.Header.Format = Format
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- the operator names the fixture file
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	if err := encoder.Encode(fixture.Header); err != nil {
		_ = file.Close()
		return err
	}
	for _, event := range fixture.Events {
		if err := encoder.Encode(event); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// Load reads a fixture, checking its format and the ordering of its
// events.
func Load(path string) (Fixture, error) {
	file, err := os.Open(path) // #nosec G304 -- the operator names the fixture file
	if err != nil {
		return Fixture{}, err
	}
	defer func() { _ = file.Close() }()
	return Read(file)
}

// Read decodes a fixture from a JSON Lines stream.
func Read(r io.Reader) (Fixture, error) {
	decoder := json.NewDecoder(r)
	var fixture Fixture
	if err := decoder.Decode(&fixture.Header); err != nil {
		return Fixture{}, fmt.Errorf("fixture header: %w", err)
	}
	if fixture.Header.Format != Format {
		return Fixture{}, fmt.Errorf("unsupported fixture format %q", fixture.Header.Format)
	}
	var previous int64
	for {
		var event Event
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Fixture{}, fmt.Errorf("fixture event %d: %w", len(fixture.Events), err)
		}
		switch event.Kind {
		case Output, Input, Resize, Exit:
		default:
			return Fixture{}, fmt.Errorf("fixture event %d: unknown kind %q", len(fixture.Events), event.Kind)
		}
		if event.AtMillis < previous {
			return Fixture{}, fmt.Errorf("fixture event %d goes back in time", len(fixture.Events))
		}
		previous = event.AtMillis
		fixture.Events = append(fixture.Events, event)
	}
	return fixture, nil
}
