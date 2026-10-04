// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package fixture

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/goabonga/maestro/internal/session"
)

// ImportScript converts a recording made by util-linux script in its
// advanced format (-I input, -O output, -T timing) into a raw fixture.
// The result is not sanitized: it must go through a Sanitizer before
// it may be saved. The header's agent, version and case are kept;
// terminal size and exit code come from the recording.
func ImportScript(timing, input, output io.Reader, header Header) (Fixture, error) {
	in := bufio.NewReader(input)
	out := bufio.NewReader(output)
	// Both data logs open with a "Script started on ..." line.
	for name, log := range map[string]*bufio.Reader{"input": in, "output": out} {
		if _, err := log.ReadString('\n'); err != nil {
			return Fixture{}, fmt.Errorf("%s log: missing header line: %w", name, err)
		}
	}

	fixture := Fixture{Header: header}
	exitCode, hasExit := 0, false
	var elapsed float64
	lines := bufio.NewScanner(timing)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for number := 1; lines.Scan(); number++ {
		fields := strings.Fields(lines.Text())
		if len(fields) < 3 {
			return Fixture{}, fmt.Errorf("timing line %d: too short", number)
		}
		delay, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || delay < 0 || math.IsNaN(delay) || math.IsInf(delay, 0) {
			return Fixture{}, fmt.Errorf("timing line %d: bad delay %q", number, fields[1])
		}
		elapsed += delay
		at := int64(elapsed * 1000)
		switch fields[0] {
		case "O", "I":
			size, err := strconv.Atoi(fields[2])
			if err != nil || size < 0 || size > MaxEventBytes {
				return Fixture{}, fmt.Errorf("timing line %d: bad size %q", number, fields[2])
			}
			source, kind := out, Output
			if fields[0] == "I" {
				source, kind = in, Input
			}
			data := make([]byte, size)
			if _, err := io.ReadFull(source, data); err != nil {
				return Fixture{}, fmt.Errorf("timing line %d: %s log too short: %w", number, kind, err)
			}
			fixture.Events = append(fixture.Events, Event{AtMillis: at, Kind: kind, Data: data})
		case "S":
			if fields[2] != "SIGWINCH" {
				continue
			}
			rows, cols, err := parseWinch(fields[3:])
			if err != nil {
				return Fixture{}, fmt.Errorf("timing line %d: %w", number, err)
			}
			fixture.Events = append(fixture.Events, Event{AtMillis: at, Kind: Resize, Rows: rows, Cols: cols})
		case "H":
			value := strings.Join(fields[3:], " ")
			switch fields[2] {
			case "LINES":
				if rows, err := parseSize(value); err == nil && fixture.Header.Rows == 0 {
					fixture.Header.Rows = rows
				}
			case "COLUMNS":
				if cols, err := parseSize(value); err == nil && fixture.Header.Cols == 0 {
					fixture.Header.Cols = cols
				}
			case "EXIT_CODE":
				if code, err := strconv.Atoi(value); err == nil {
					exitCode, hasExit = code, true
				}
			}
		default:
			return Fixture{}, fmt.Errorf("timing line %d: unknown entry %q", number, fields[0])
		}
	}
	if err := lines.Err(); err != nil {
		return Fixture{}, err
	}
	if !hasExit {
		return Fixture{}, errors.New("the recording has no exit code: was it made with script -T in advanced format?")
	}
	phase := session.Exited
	// timeout(1) reports 124 when it had to kill the command, and a
	// SIGKILL shows as 128+9: both are a stop from outside.
	if exitCode == 124 || exitCode == 137 {
		phase = session.Stopped
	}
	fixture.Events = append(fixture.Events, Event{AtMillis: int64(elapsed * 1000), Kind: Exit, ExitCode: exitCode, Phase: phase})
	return fixture, nil
}

// MaxEventBytes bounds one imported chunk.
const MaxEventBytes = 1 << 20

// parseWinch reads "ROWS=24 COLS=80".
func parseWinch(fields []string) (uint16, uint16, error) {
	var rows, cols uint16
	for _, field := range fields {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		size, err := parseSize(value)
		if err != nil {
			return 0, 0, err
		}
		switch name {
		case "ROWS":
			rows = size
		case "COLS":
			cols = size
		}
	}
	if rows == 0 || cols == 0 {
		return 0, 0, fmt.Errorf("incomplete SIGWINCH entry %v", fields)
	}
	return rows, cols, nil
}

// parseSize reads a terminal dimension.
func parseSize(value string) (uint16, error) {
	size, err := strconv.ParseUint(value, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("bad terminal size %q", value)
	}
	return uint16(size), nil
}
