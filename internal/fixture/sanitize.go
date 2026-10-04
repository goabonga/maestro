// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package fixture

import (
	"bytes"
	"regexp"
	"sort"
)

// Redacted replaces every stripped secret.
const Redacted = "[REDACTED]"

// coalesceWindow merges output chunks that arrive this close together,
// so a secret split across PTY reads is scanned as one string.
const coalesceWindow = 100

// patterns are the secret shapes stripped from every fixture and
// checked again before saving.
var patterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`),
}

// Sanitizer strips secrets from fixtures.
type Sanitizer struct {
	// Values lists exact secret values to strip (API keys from the
	// recording environment, tokens), whatever their shape.
	Values []string
	// Paths maps host-specific strings to stable placeholders, such as
	// the home directory to /home/user.
	Paths map[string]string
}

// Sanitize returns a copy of the fixture with secrets stripped from its
// output and input, adjacent output coalesced so no secret straddles
// two events, and the header marked sanitized.
func (s Sanitizer) Sanitize(fixture Fixture) Fixture {
	clean := Fixture{Header: fixture.Header}
	for _, event := range coalesce(fixture.Events) {
		if event.Kind == Output || event.Kind == Input {
			event.Data = s.scrub(event.Data)
		}
		clean.Events = append(clean.Events, event)
	}
	clean.Header.Sanitized = true
	return clean
}

// scrub strips exact values first, then host paths, then shapes.
func (s Sanitizer) scrub(data []byte) []byte {
	out := append([]byte(nil), data...)
	values := append([]string(nil), s.Values...)
	// Longest first, so a value containing another is stripped whole.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		if value != "" {
			out = bytes.ReplaceAll(out, []byte(value), []byte(Redacted))
		}
	}
	paths := make([]string, 0, len(s.Paths))
	for path := range s.Paths {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
	for _, path := range paths {
		if path != "" {
			out = bytes.ReplaceAll(out, []byte(path), []byte(s.Paths[path]))
		}
	}
	for _, pattern := range patterns {
		out = pattern.ReplaceAll(out, []byte(Redacted))
	}
	return out
}

// coalesce merges output events closer than the coalesce window into
// the first one of their burst.
func coalesce(events []Event) []Event {
	var merged []Event
	for _, event := range events {
		last := len(merged) - 1
		if event.Kind == Output && last >= 0 && merged[last].Kind == Output &&
			event.AtMillis-merged[last].AtMillis < coalesceWindow {
			merged[last].Data = append(append([]byte(nil), merged[last].Data...), event.Data...)
			continue
		}
		event.Data = append([]byte(nil), event.Data...)
		merged = append(merged, event)
	}
	return merged
}

// Verify scans the whole output and input streams of a fixture, across
// event boundaries, and returns every detectable secret shape left.
func Verify(fixture Fixture) []string {
	var output, input bytes.Buffer
	for _, event := range fixture.Events {
		switch event.Kind {
		case Output:
			output.Write(event.Data)
		case Input:
			input.Write(event.Data)
		}
	}
	var findings []string
	for _, stream := range [][]byte{output.Bytes(), input.Bytes()} {
		for _, pattern := range patterns {
			for _, match := range pattern.FindAll(stream, -1) {
				findings = append(findings, pattern.String()+" at "+shorten(match))
			}
		}
	}
	return findings
}

// shorten keeps a finding readable without echoing the secret whole.
func shorten(match []byte) string {
	if len(match) <= 8 {
		return "…"
	}
	return string(match[:4]) + "…"
}
