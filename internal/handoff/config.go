// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package handoff

import (
	"errors"
	"fmt"
)

// ErrConfig reports a result produced under another configuration
// snapshot than the one its task runs on now.
var ErrConfig = errors.New("result bound to another configuration")

// ForTask refuses a document that does not belong to the task taskID
// as it runs now, on the snapshot configID: once a task adopts a new
// snapshot, every plan, implementation, review, test report or
// resolution produced under the old one is refused for it.
func (e Envelope) ForTask(taskID, configID string) error {
	if e.TaskID != taskID {
		return invalid("%s belongs to task %s, not %s", e.ArtifactID, e.TaskID, taskID)
	}
	if e.ConfigID != configID {
		return fmt.Errorf("%w: %s was produced under %s, task %s runs on %s",
			ErrConfig, e.ArtifactID, e.ConfigID, taskID, configID)
	}
	return nil
}
