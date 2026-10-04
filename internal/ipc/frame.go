// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"encoding/binary"
	"fmt"
	"io"
)

// FrameType tags one streaming frame.
type FrameType byte

// The streaming frame types. Terminal bytes travel only in these
// frames, on a dedicated upgraded connection, never in RPC responses.
const (
	// FrameOutput carries terminal output to the client.
	FrameOutput FrameType = 'o'
	// FrameInput carries client keystrokes to the terminal.
	FrameInput FrameType = 'i'
	// FrameResize carries rows and cols, two big-endian uint16.
	FrameResize FrameType = 'r'
	// FrameDetach ends the stream without touching the session.
	FrameDetach FrameType = 'd'
	// FrameError carries a terminal diagnostic before the stream ends.
	FrameError FrameType = 'e'
)

// MaxFramePayload bounds every frame.
const MaxFramePayload = 64 << 10

// WriteFrame writes one typed, bounded frame.
func WriteFrame(w io.Writer, kind FrameType, payload []byte) error {
	if len(payload) > MaxFramePayload {
		return fmt.Errorf("frame payload of %d bytes exceeds the %d bound", len(payload), MaxFramePayload)
	}
	header := [5]byte{byte(kind)}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload))) // #nosec G115 -- bounded by MaxFramePayload above
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one frame, refusing unknown types and oversized
// payloads.
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	kind := FrameType(header[0])
	switch kind {
	case FrameOutput, FrameInput, FrameResize, FrameDetach, FrameError:
	default:
		return 0, nil, fmt.Errorf("unknown frame type %q", header[0])
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > MaxFramePayload {
		return 0, nil, fmt.Errorf("frame payload of %d bytes exceeds the %d bound", length, MaxFramePayload)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return kind, payload, nil
}

// ResizePayload encodes a terminal size.
func ResizePayload(rows, cols uint16) []byte {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint16(payload, rows)
	binary.BigEndian.PutUint16(payload[2:], cols)
	return payload
}

// ParseResize decodes a resize payload.
func ParseResize(payload []byte) (uint16, uint16, error) {
	if len(payload) != 4 {
		return 0, 0, fmt.Errorf("a resize payload has 4 bytes, got %d", len(payload))
	}
	return binary.BigEndian.Uint16(payload), binary.BigEndian.Uint16(payload[2:]), nil
}
