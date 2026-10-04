// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package fixture

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/session"
)

// fakeKey has the shape of an Anthropic key without being one.
const fakeKey = "sk-ant-FAKE0123456789abcdefFAKE"

// collector is a sink that keeps what a replay delivered.
type collector struct {
	output bytes.Buffer
	inputs []string
	sizes  [][2]uint16
	exit   int
	phase  string
	ticks  []time.Duration
}

func (c *collector) Output(data []byte)       { c.output.Write(data) }
func (c *collector) Input(data []byte)        { c.inputs = append(c.inputs, string(data)) }
func (c *collector) Resize(rows, cols uint16) { c.sizes = append(c.sizes, [2]uint16{rows, cols}) }
func (c *collector) Exit(code int, phase string) {
	c.exit, c.phase = code, phase
}
func (c *collector) Tick(at time.Duration) { c.ticks = append(c.ticks, at) }

// synthetic builds a small fixture by hand.
func synthetic() Fixture {
	return Fixture{
		Header: Header{Agent: "fake", Version: "1.0.0", Case: "prompt", Rows: 24, Cols: 80,
			Expected: []Checkpoint{{AtMillis: 300, State: "COMPLETED"}}},
		Events: []Event{
			{AtMillis: 0, Kind: Input, Data: []byte("hello\r")},
			{AtMillis: 10, Kind: Output, Data: []byte("token " + fakeKey[:12])},
			{AtMillis: 20, Kind: Output, Data: []byte(fakeKey[12:] + " for user@example.com in /home/alice/work\r\n")},
			{AtMillis: 250, Kind: Resize, Rows: 40, Cols: 100},
			{AtMillis: 300, Kind: Exit, ExitCode: 0, Phase: "exited"},
		},
	}
}

func TestSanitizeStripsSecretsAcrossChunks(t *testing.T) {
	raw := synthetic()
	if findings := Verify(raw); len(findings) == 0 {
		t.Fatal("the raw fixture should expose its secrets to Verify")
	}
	clean := Sanitizer{Paths: map[string]string{"/home/alice": "/home/user"}}.Sanitize(raw)
	if !clean.Header.Sanitized {
		t.Fatal("the header is not marked sanitized")
	}
	if findings := Verify(clean); len(findings) != 0 {
		t.Fatalf("secrets survived: %v", findings)
	}
	var output bytes.Buffer
	for _, event := range clean.Events {
		if event.Kind == Output {
			output.Write(event.Data)
		}
	}
	text := output.String()
	if strings.Contains(text, "FAKE") || strings.Contains(text, "alice") || strings.Contains(text, "example.com") {
		t.Fatalf("sanitized output still leaks: %q", text)
	}
	if !strings.Contains(text, "/home/user/work") || !strings.Contains(text, Redacted) {
		t.Fatalf("placeholders missing: %q", text)
	}
	// The raw fixture is left untouched.
	if !bytes.Contains(raw.Events[1].Data, []byte("sk-ant")) {
		t.Fatal("Sanitize modified its input")
	}
}

func TestSanitizeStripsExactValues(t *testing.T) {
	raw := Fixture{Events: []Event{{Kind: Output, Data: []byte("token=opaque-value-42 done")}}}
	clean := Sanitizer{Values: []string{"opaque-value-42"}}.Sanitize(raw)
	if bytes.Contains(clean.Events[0].Data, []byte("opaque")) {
		t.Fatalf("an exact value survived: %q", clean.Events[0].Data)
	}
}

func TestSaveRefusesUnsanitizedOrLeakingFixtures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "case.jsonl")
	if err := Save(path, synthetic()); err == nil || !strings.Contains(err.Error(), "unsanitized") {
		t.Fatalf("error %v", err)
	}
	leaking := synthetic()
	leaking.Header.Sanitized = true
	if err := Save(path, leaking); err == nil || !strings.Contains(err.Error(), "detectable secrets") {
		t.Fatalf("error %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	clean := Sanitizer{}.Sanitize(synthetic())
	path := filepath.Join(t.TempDir(), "fixtures", "case.jsonl")
	if err := Save(path, clean); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Header.Format != Format || loaded.Header.Case != "prompt" || len(loaded.Header.Expected) != 1 {
		t.Fatalf("header %+v", loaded.Header)
	}
	if len(loaded.Events) != len(clean.Events) {
		t.Fatalf("events %d, want %d", len(loaded.Events), len(clean.Events))
	}
	for i := range clean.Events {
		if !bytes.Equal(loaded.Events[i].Data, clean.Events[i].Data) || loaded.Events[i].Kind != clean.Events[i].Kind {
			t.Fatalf("event %d differs: %+v vs %+v", i, loaded.Events[i], clean.Events[i])
		}
	}
}

func TestReadRejectsMalformedFixtures(t *testing.T) {
	for name, content := range map[string]string{
		"format":   `{"format":"other/1"}`,
		"kind":     `{"format":"` + Format + `"}` + "\n" + `{"at_ms":0,"kind":"teleport"}`,
		"ordering": `{"format":"` + Format + `"}` + "\n" + `{"at_ms":5,"kind":"output"}` + "\n" + `{"at_ms":1,"kind":"output"}`,
		"header":   `not json`,
	} {
		if _, err := Read(strings.NewReader(content)); err == nil {
			t.Fatalf("%s: a malformed fixture was accepted", name)
		}
	}
}

func TestReplayDeliversEventsOnTheRecordedClock(t *testing.T) {
	clean := Sanitizer{}.Sanitize(synthetic())
	sink := &collector{}
	if err := Replay(context.Background(), clean, sink, 0); err != nil {
		t.Fatal(err)
	}
	if len(sink.inputs) != 1 || sink.inputs[0] != "hello\r" {
		t.Fatalf("inputs %q", sink.inputs)
	}
	if len(sink.sizes) != 1 || sink.sizes[0] != [2]uint16{40, 100} {
		t.Fatalf("sizes %v", sink.sizes)
	}
	if sink.phase != "exited" || sink.exit != 0 {
		t.Fatalf("exit %d %s", sink.exit, sink.phase)
	}
	if last := sink.ticks[len(sink.ticks)-1]; last != 300*time.Millisecond {
		t.Fatalf("last tick %v", last)
	}

	// Real-time replay honors the recorded delays.
	started := time.Now()
	if err := Replay(context.Background(), clean, &collector{}, 1); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Fatalf("real-time replay took only %v", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Replay(ctx, clean, &collector{}, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled replay returned %v", err)
	}
}

func TestRecordCapturesAConfinedSession(t *testing.T) {
	probed, err := launcher.New()
	if errors.Is(err, launcher.ErrUnsupported) {
		t.Skipf("host cannot confine: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	// A non-interactive shell: an interactive dash restores the
	// terminal's process group on exit, which fails from inside the
	// sandbox's PID namespace and masks the exit code.
	live, err := session.Start(probed, session.Config{Spec: launcher.Spec{
		Argv: []string{"/bin/sh", "-c", "read line; echo got:$line; stty size; exit 4"},
		Dir:  t.TempDir(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Stop(200 * time.Millisecond) })

	recorder := Record(live, Header{Agent: "sh", Version: "fixture", Case: "prompt", Rows: 24, Cols: 80})
	if err := recorder.Resize(30, 90); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Input([]byte(fakeKey + "\r")); err != nil {
		t.Fatal(err)
	}
	raw := recorder.Finish()

	last := raw.Events[len(raw.Events)-1]
	if last.Kind != Exit || last.ExitCode != 4 || last.Phase != session.Exited {
		var seen bytes.Buffer
		for _, event := range raw.Events {
			seen.Write(event.Data)
		}
		t.Fatalf("last event %+v, stream %q", last, seen.String())
	}
	var output bytes.Buffer
	for _, event := range raw.Events {
		if event.Kind == Output {
			output.Write(event.Data)
		}
	}
	if !strings.Contains(output.String(), "30 90") {
		t.Fatalf("the recorded output misses the resize: %q", output.String())
	}

	clean := Sanitizer{}.Sanitize(raw)
	path := filepath.Join(t.TempDir(), "sh-prompt.jsonl")
	if err := Save(path, clean); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	sink := &collector{}
	if err := Replay(context.Background(), loaded, sink, 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sink.output.String(), "FAKE") || !strings.Contains(sink.output.String(), "30 90") {
		t.Fatalf("replayed output %q", sink.output.String())
	}
	if sink.exit != 4 {
		t.Fatalf("replayed exit %d", sink.exit)
	}
}
