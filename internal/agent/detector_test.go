// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/fixture"
	"github.com/goabonga/maestro/internal/session"
)

func detector(t *testing.T, kind string) *TurnDetector {
	t.Helper()
	v := Version{2, 1, 289}
	if kind == "codex" {
		v = Version{0, 160, 0}
	}
	d, err := NewTurnDetector(kind, v, DetectorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDetectorRequiresReadinessIdleAndLiveProcess(t *testing.T) {
	for _, kind := range []string{"claude-code", "codex"} {
		t.Run(kind, func(t *testing.T) {
			d := detector(t, kind)
			now := time.Now()
			d.Begin(now)
			live := session.State{Phase: session.Running}
			if got := d.Poll(now.Add(time.Second), live); got != DetectUnknown {
				t.Fatal(got)
			}
			footer := "✻ Cogitated for 1s · done 12:21 AM\r\n❯ "
			if kind == "codex" {
				footer = "Worked for 1s • 12:21 AM\r\n› "
			}
			d.Feed([]byte(footer), now.Add(time.Second))
			if got := d.Poll(now.Add(time.Second), live); got != DetectRunning {
				t.Fatal(got)
			}
			if got := d.Poll(now.Add(2*time.Second), live); got != DetectCompleted {
				t.Fatal(got)
			}
			d.Begin(now.Add(3 * time.Second))
			if got := d.Poll(now.Add(4*time.Second), live); got != DetectUnknown {
				t.Fatalf("stale footer: %s", got)
			}
			d.Feed([]byte(footer), now.Add(4*time.Second))
			if got := d.Poll(now.Add(5*time.Second), session.State{Phase: session.Exited}); got != DetectFailed {
				t.Fatalf("exit used as success: %s", got)
			}
		})
	}
}

func TestDetectorTimeoutQuestionsErrorsAndInterruption(t *testing.T) {
	now := time.Now()
	live := session.State{Phase: session.Running}
	for _, kind := range []string{"claude-code", "codex"} {
		d := detector(t, kind)
		d.Begin(now)
		bullet := "●"
		if kind == "codex" {
			bullet = "•"
		}
		d.Feed([]byte(bullet+" What color do you prefer?\r\n"), now)
		if got := d.Poll(now.Add(time.Second), live); got != DetectWaitingInput {
			t.Fatal(got)
		}
		if got := d.Poll(now.Add(6*time.Minute), live); got != DetectFailed {
			t.Fatal(got)
		}
		d.Begin(now)
		d.Feed([]byte("Would you like to run the following command?"), now)
		if got := d.Poll(now, live); got != DetectWaitingInput {
			t.Fatal(got)
		}
		d.Interrupt()
		if got := d.Poll(now, live); got != DetectInterrupted {
			t.Fatal(got)
		}
		d.Begin(now)
		d.Feed([]byte("invalid_request_error"), now)
		if got := d.Poll(now, live); got != DetectFailed {
			t.Fatal(got)
		}
		d.Begin(now)
		if got := d.Poll(now.Add(21*time.Minute), live); got != DetectFailed {
			t.Fatalf("silence became success: %s", got)
		}
	}
	if _, err := NewTurnDetector("codex", Version{0, 160, 1}, DetectorConfig{}); err == nil {
		t.Fatal("unvalidated version accepted")
	}
}

func TestTerminalReconstructsChunkedRedraws(t *testing.T) {
	s := newTerminal(3, 30)
	for _, b := range []byte("\x1b]0;ignored\x07Working\x1b[2Gorked\x1b[1;1H\x1b[2KWorked\x1b[8Gfor 2s\r\n› ") {
		s.feed([]byte{b})
	}
	if !strings.Contains(s.text(), "Worked for 2s") || strings.Contains(s.text(), "Working") {
		t.Fatal(s.text())
	}
}

// Real sanitized captures are replayed without a native binary, terminal or
// network. These assertions check driver conclusions, not just decoding.
func TestDetectorReplaysReferenceOutcomes(t *testing.T) {
	for _, agent := range []struct{ path, kind, version string }{{"claude", "claude-code", "2.1.289"}, {"codex", "codex", "0.160.0"}} {
		for _, name := range []string{"prompt", "multiline", "resize", "resume"} {
			t.Run(agent.path+"/"+name, func(t *testing.T) {
				f, err := fixture.Load(filepath.Join("..", "fixture", "testdata", agent.path, agent.version, name+".jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				d := detector(t, agent.kind)
				d.screen = newTerminal(int(f.Header.Rows), int(f.Header.Cols))
				start := time.Now()
				d.Begin(start)
				want := DetectCompleted
				// The resumed Claude capture answers the memory check, then
				// asks whether to write the unfinished story. It awaits input.
				if agent.path == "claude" && name == "resume" {
					want = DetectWaitingInput
				}
				matched := false
				for _, e := range f.Events {
					if e.Kind == fixture.Resize {
						d.Resize(int(e.Rows), int(e.Cols))
						continue
					}
					if e.Kind != fixture.Output {
						continue
					}
					at := start.Add(e.At())
					for offset := 0; offset < len(e.Data); offset += 7 {
						d.Feed(e.Data[offset:min(offset+7, len(e.Data))], at)
					}
					if d.Poll(at.Add(time.Second), session.State{Phase: session.Running}) == want && (want != DetectWaitingInput || strings.Contains(assistantAnswer(d.screen.text(), agent.kind), "ORANGE-17")) {
						matched = true
						break
					}
				}
				if !matched {
					t.Fatalf("expected %s; screen: %s", want, strings.TrimSpace(d.screen.text()))
				}
			})
		}
	}
}
