// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package handoff

import (
	"errors"
	"testing"
)

func TestForTaskRefusesAnotherTaskOrConfiguration(t *testing.T) {
	envelope := Envelope{ArtifactID: "a1", TaskID: "task-1", ConfigID: "config-1"}
	if err := envelope.ForTask("task-1", "config-1"); err != nil {
		t.Fatal(err)
	}
	if err := envelope.ForTask("task-1", "config-2"); !errors.Is(err, ErrConfig) || errors.Is(err, ErrInvalid) {
		t.Fatalf("old configuration: %v", err)
	}
	if err := envelope.ForTask("task-2", "config-1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("other task: %v", err)
	}
}
