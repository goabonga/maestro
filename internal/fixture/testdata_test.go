// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package fixture

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommittedFixturesAreSafeAndReplayable guards every fixture in
// testdata: sanitized, free of detectable secrets, labelled after its
// path, ending with the recorded exit, and replayable.
func TestCommittedFixturesAreSafeAndReplayable(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no committed fixtures found")
	}
	for _, path := range paths {
		loaded, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		parts := strings.Split(filepath.ToSlash(path), "/")
		agent, version, name := parts[1], parts[2], strings.TrimSuffix(parts[3], ".jsonl")
		header := loaded.Header
		if header.Agent != agent || header.Version != version || header.Case != name {
			t.Fatalf("%s: header %s/%s/%s does not match its path", path, header.Agent, header.Version, header.Case)
		}
		if !header.Sanitized {
			t.Fatalf("%s: not sanitized", path)
		}
		if findings := Verify(loaded); len(findings) > 0 {
			t.Fatalf("%s: detectable secrets: %v", path, findings)
		}
		if len(loaded.Events) == 0 || loaded.Events[len(loaded.Events)-1].Kind != Exit {
			t.Fatalf("%s: does not end with the recorded exit", path)
		}
		if err := Replay(context.Background(), loaded, &collector{}, 0); err != nil {
			t.Fatalf("%s: replay: %v", path, err)
		}
	}
}
