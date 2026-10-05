// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package agent

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goabonga/maestro/internal/session"
)

// Detection describes native CLI readiness, never artifact or Git acceptance.
type Detection string

const (
	DetectRunning      Detection = "RUNNING"
	DetectWaitingInput Detection = "WAITING_INPUT"
	DetectCompleted    Detection = "COMPLETED"
	DetectInterrupted  Detection = "INTERRUPTED"
	DetectFailed       Detection = "FAILED"
	DetectUnknown      Detection = "UNKNOWN"
)

// DetectorConfig freezes the bounds for one detector. Poll uses an injected
// clock. Unknown activity expires as FAILED, never as COMPLETED.
type DetectorConfig struct {
	Rows, Cols                          int
	Idle, TurnTimeout, InputWaitTimeout time.Duration
}

// TurnDetector combines a version-specific rendered ready marker, inactivity
// and process liveness. Call Begin after submitting each prompt, Feed for
// every output chunk and Poll even while the PTY is silent. Calls serialize.
type TurnDetector struct {
	mu                                sync.Mutex
	kind                              string
	config                            DetectorConfig
	screen                            terminal
	started, lastOutput, waitingSince time.Time
	state                             Detection
	active                            bool
}

// NewTurnDetector refuses versions outside the real-session validation matrix.
func NewTurnDetector(kind string, version Version, cfg DetectorConfig) (*TurnDetector, error) {
	if _, err := Builtin().Select(kind, version); err != nil {
		return nil, err
	}
	if cfg.Idle == 0 {
		cfg.Idle = 300 * time.Millisecond
	}
	if cfg.TurnTimeout == 0 {
		cfg.TurnTimeout = 20 * time.Minute
	}
	if cfg.InputWaitTimeout == 0 {
		cfg.InputWaitTimeout = 5 * time.Minute
	}
	if cfg.Idle < 0 || cfg.TurnTimeout <= 0 || cfg.InputWaitTimeout <= 0 || cfg.Idle >= cfg.TurnTimeout {
		return nil, errors.New("invalid detector timeouts")
	}
	return &TurnDetector{kind: kind, config: cfg, screen: newTerminal(cfg.Rows, cfg.Cols), state: DetectUnknown}, nil
}

// Begin discards all previous turn evidence, including a stale ready footer.
func (d *TurnDetector) Begin(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.screen = newTerminal(d.config.Rows, d.config.Cols)
	d.started, d.lastOutput, d.waitingSince = now, now, time.Time{}
	d.state, d.active = DetectRunning, true
}

// Feed consumes raw terminal bytes, including escapes split across chunks.
func (d *TurnDetector) Feed(data []byte, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(data) == 0 {
		return
	}
	d.screen.feed(data)
	if !now.Before(d.lastOutput) {
		d.lastOutput = now
	}
}

// Resize tracks the PTY's dimensions; bounded visible text is retained until
// the CLI redraws it. The caller must also resize the actual Session.
func (d *TurnDetector) Resize(rows, cols int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	next := newTerminal(rows, cols)
	for row := 0; row < min(len(next.lines), len(d.screen.lines)); row++ {
		copy(next.lines[row], d.screen.lines[row])
	}
	next.row = min(d.screen.row, len(next.lines)-1)
	next.col = min(d.screen.col, len(next.lines[0])-1)
	d.screen = next
	d.config.Rows, d.config.Cols = len(next.lines), len(next.lines[0])
}

// Interrupt records a supervisor-requested interruption; it cannot be
// mistaken for completion when the CLI redraws a ready prompt afterwards.
func (d *TurnDetector) Interrupt() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state = DetectInterrupted
	d.active = false
}

var claudeDone = regexp.MustCompile(`(?m)^\s*✻\s+[\p{L}]+ for [0-9][^\n]*· done `)
var codexDone = regexp.MustCompile(`(?m)^\s*Worked for [0-9][^\n]*• `)

// Poll returns a conservative observation. COMPLETED means native readiness
// for validation, not SUCCEEDED. A dead process, even with exit code zero,
// never supplies readiness evidence on its own.
func (d *TurnDetector) Poll(now time.Time, process session.State) Detection {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active {
		return d.state
	}
	if process.Phase != session.Running {
		if process.Phase == session.Stopped {
			d.state = DetectInterrupted
		} else {
			d.state = DetectFailed
		}
		d.active = false
		return d.state
	}
	if !now.Before(d.started.Add(d.config.TurnTimeout)) {
		d.state = DetectFailed
		d.active = false
		return d.state
	}
	text := d.screen.text()
	if hasAny(text, "API Error:", "invalid_request_error", "Can't reach the API server", "Unable to connect to API") {
		d.state = DetectFailed
		d.active = false
		return d.state
	}
	waiting := hasAny(text, "Quick safety check", "Trust this folder?", "Do you want to create", "Do you want to proceed?", "Would you like to run the following command?", "Enter to confirm")
	answer := assistantAnswer(text, d.kind)
	if strings.Contains(answer, "?") {
		waiting = true
	}
	if waiting {
		if d.waitingSince.IsZero() {
			d.waitingSince = now
		}
		if !now.Before(d.waitingSince.Add(d.config.InputWaitTimeout)) {
			d.state = DetectFailed
			d.active = false
		} else {
			d.state = DetectWaitingInput
		}
		return d.state
	}
	d.waitingSince = time.Time{}
	prompt, done := "❯", claudeDone.MatchString(text)
	if d.kind == "codex" {
		prompt, done = "›", codexDone.MatchString(text)
	}
	ready := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prompt) {
			ready = true
		}
	}
	if done && ready && !now.Before(d.lastOutput.Add(d.config.Idle)) {
		d.state = DetectCompleted
		d.active = false
		return d.state
	}
	if now.Before(d.lastOutput.Add(d.config.Idle)) {
		d.state = DetectRunning
	} else {
		d.state = DetectUnknown
	}
	return d.state
}

func hasAny(text string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// Questions in assistant text are conservative waits; composer hints and
// echoed user prompts are excluded. Ordinary text never supplies success.
func assistantAnswer(text, kind string) string {
	bullet := "●"
	if kind == "codex" {
		bullet = "•"
	}
	var answer strings.Builder
	collecting := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, bullet+" ") && !hasAny(line, "Working (", "Running ") {
			answer.Reset()
			collecting = true
		}
		if hasAny(line, "Worked for ", "· done ") || strings.HasPrefix(line, "❯") || strings.HasPrefix(line, "›") {
			collecting = false
		}
		if collecting {
			answer.WriteString(line)
			answer.WriteByte(' ')
		}
	}
	return answer.String()
}
