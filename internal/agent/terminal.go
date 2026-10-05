// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package agent

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// terminal reconstructs the visible text rather than searching the raw PTY:
// cursor-addressed redraws split words, and old spinners must be erased.
// Its storage and escape strings are bounded independently of CLI output.
type terminal struct {
	lines                        [][]rune
	row, col, savedRow, savedCol int
	pending                      []byte
	escape                       string
	mode                         byte
}

func newTerminal(rows, cols int) terminal {
	if rows < 1 || rows > 256 {
		rows = 40
	}
	if cols < 1 || cols > 512 {
		cols = 182
	}
	t := terminal{lines: make([][]rune, rows)}
	for i := range t.lines {
		t.lines[i] = make([]rune, cols)
	}
	return t
}

func (t *terminal) feed(data []byte) {
	data = append(t.pending, data...)
	t.pending = nil
	for len(data) > 0 {
		if t.mode != 0 {
			b := data[0]
			data = data[1:]
			switch t.mode {
			case 'e':
				switch b {
				case '[':
					t.mode = 'c'
					t.escape = ""
				case ']', 'P', '_', '^':
					t.mode = 's'
				case '7':
					t.savedRow, t.savedCol = t.row, t.col
					t.mode = 0
				case '8':
					t.row, t.col = t.savedRow, t.savedCol
					t.mode = 0
				case '(':
					t.mode = 'x'
				default:
					t.mode = 0
				}
			case 'x':
				t.mode = 0
			case 's':
				if b == 7 {
					t.mode = 0
				}
				if b == 27 {
					t.mode = 'z'
				}
			case 'z':
				if b == '\\' {
					t.mode = 0
				} else {
					t.mode = 's'
				}
			case 'c':
				if b >= 0x40 && b <= 0x7e {
					t.csi(b)
					t.mode = 0
					t.escape = ""
				} else if len(t.escape) < 128 {
					t.escape += string(b)
				}
			}
			continue
		}
		if !utf8.FullRune(data) {
			t.pending = append([]byte(nil), data...)
			break
		}
		r, n := utf8.DecodeRune(data)
		data = data[n:]
		switch r {
		case 27:
			t.mode = 'e'
		case '\r':
			t.col = 0
		case '\n':
			t.down()
		case '\b':
			if t.col > 0 {
				t.col--
			}
		case '\t':
			t.col = min((t.col/8+1)*8, len(t.lines[0])-1)
		default:
			if r < 32 || r == 127 {
				continue
			}
			if t.col >= len(t.lines[0]) {
				t.col = 0
				t.down()
			}
			t.lines[t.row][t.col] = r
			t.col++
		}
	}
}

func (t *terminal) down() {
	if t.row < len(t.lines)-1 {
		t.row++
		return
	}
	copy(t.lines, t.lines[1:])
	t.lines[len(t.lines)-1] = make([]rune, len(t.lines[0]))
}

func (t *terminal) csi(final byte) {
	private := strings.HasPrefix(t.escape, "?")
	values := strings.Split(strings.TrimLeft(t.escape, "?=>"), ";")
	arg := func(i, fallback int) int {
		if i >= len(values) {
			return fallback
		}
		v, err := strconv.Atoi(values[i])
		if err != nil || v <= 0 {
			return fallback
		}
		return min(v, 4096)
	}
	if private {
		return
	}
	switch final {
	case 'A':
		t.row = max(0, t.row-arg(0, 1))
	case 'B':
		t.row = min(len(t.lines)-1, t.row+arg(0, 1))
	case 'C':
		t.col = min(len(t.lines[0])-1, t.col+arg(0, 1))
	case 'D':
		t.col = max(0, t.col-arg(0, 1))
	case 'E':
		t.row = min(len(t.lines)-1, t.row+arg(0, 1))
		t.col = 0
	case 'F':
		t.row = max(0, t.row-arg(0, 1))
		t.col = 0
	case 'G':
		t.col = min(len(t.lines[0])-1, arg(0, 1)-1)
	case 'H', 'f':
		t.row = min(len(t.lines)-1, arg(0, 1)-1)
		t.col = min(len(t.lines[0])-1, arg(1, 1)-1)
	case 'K':
		start, end := t.col, len(t.lines[0])
		if arg(0, 0) == 1 {
			start, end = 0, min(t.col+1, len(t.lines[0]))
		}
		if arg(0, 0) == 2 {
			start = 0
		}
		clear(t.lines[t.row][min(start, end):end])
	case 'J':
		switch arg(0, 0) {
		case 2, 3:
			for i := range t.lines {
				clear(t.lines[i])
			}
		case 0:
			clear(t.lines[t.row][min(t.col, len(t.lines[0])):])
			for i := t.row + 1; i < len(t.lines); i++ {
				clear(t.lines[i])
			}
		case 1:
			for i := 0; i < t.row; i++ {
				clear(t.lines[i])
			}
			clear(t.lines[t.row][:min(t.col+1, len(t.lines[0]))])
		}
	case 's':
		t.savedRow, t.savedCol = t.row, t.col
	case 'u':
		t.row, t.col = t.savedRow, t.savedCol
	}
}

func (t *terminal) text() string {
	var out strings.Builder
	for _, row := range t.lines {
		for _, r := range row {
			if r == 0 {
				r = ' '
			}
			out.WriteRune(r)
		}
		out.WriteByte('\n')
	}
	return out.String()
}
