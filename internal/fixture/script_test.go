// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package fixture

import (
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/session"
)

// recording builds the three logs of a small script recording.
func recording(exitCode string) (string, string, string) {
	timing := strings.Join([]string{
		"H 0.000000 START_TIME 2026-10-05 00:07:07+02:00",
		"H 0.000000 COLUMNS 182",
		"H 0.000000 LINES 40",
		"O 0.010000 6",
		"I 0.500000 3",
		"S 0.100000 SIGWINCH ROWS=30 COLS=90",
		"O 0.200000 4",
		"H 1.000000 EXIT_CODE " + exitCode,
	}, "\n") + "\n"
	input := "Script started on 2026-10-05 [COMMAND=\"x\"]\nhi\r\nScript done on 2026-10-05\n"
	output := "Script started on 2026-10-05 [COMMAND=\"x\"]\nready>done\n\nScript done on 2026-10-05\n"
	return timing, input, output
}

func TestImportScriptRebuildsTheSession(t *testing.T) {
	timing, input, output := recording("0")
	imported, err := ImportScript(strings.NewReader(timing), strings.NewReader(input), strings.NewReader(output),
		Header{Agent: "fake", Version: "1.0.0", Case: "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Header.Rows != 40 || imported.Header.Cols != 182 || imported.Header.Agent != "fake" {
		t.Fatalf("header %+v", imported.Header)
	}
	want := []Event{
		{AtMillis: 10, Kind: Output, Data: []byte("ready>")},
		{AtMillis: 510, Kind: Input, Data: []byte("hi\r")},
		{AtMillis: 610, Kind: Resize, Rows: 30, Cols: 90},
		{AtMillis: 810, Kind: Output, Data: []byte("done")},
		{AtMillis: 1810, Kind: Exit, ExitCode: 0, Phase: session.Exited},
	}
	if len(imported.Events) != len(want) {
		t.Fatalf("events %+v", imported.Events)
	}
	for i, event := range want {
		got := imported.Events[i]
		if got.AtMillis != event.AtMillis || got.Kind != event.Kind || string(got.Data) != string(event.Data) ||
			got.Rows != event.Rows || got.Cols != event.Cols || got.ExitCode != event.ExitCode || got.Phase != event.Phase {
			t.Fatalf("event %d: got %+v, want %+v", i, got, event)
		}
	}
	if imported.Header.Sanitized {
		t.Fatal("an import must not be marked sanitized")
	}
}

func TestImportScriptMarksAKilledCommandAsStopped(t *testing.T) {
	for _, code := range []string{"124", "137"} {
		timing, input, output := recording(code)
		imported, err := ImportScript(strings.NewReader(timing), strings.NewReader(input), strings.NewReader(output), Header{})
		if err != nil {
			t.Fatal(err)
		}
		if last := imported.Events[len(imported.Events)-1]; last.Phase != session.Stopped {
			t.Fatalf("exit %s: phase %s", code, last.Phase)
		}
	}
}

func TestImportScriptRejectsBrokenRecordings(t *testing.T) {
	timing, input, output := recording("0")
	for name, logs := range map[string][3]string{
		"truncated output": {timing, input, "Script started\nre"},
		"no header line":   {timing, input, "ready>done"},
		"unknown entry":    {"X 0.1 3\n", input, output},
		"bad delay":        {"O -1 3\n", input, output},
		"no exit code":     {"O 0.010000 6\n", input, output},
		"bad winch":        {"S 0.1 SIGWINCH ROWS=0 COLS=80\nH 0 EXIT_CODE 0\n", input, output},
	} {
		if _, err := ImportScript(strings.NewReader(logs[0]), strings.NewReader(logs[1]), strings.NewReader(logs[2]), Header{}); err == nil {
			t.Fatalf("%s: a broken recording was accepted", name)
		}
	}
}
