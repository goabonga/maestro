// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package fixture

import (
	"context"
	"time"
)

// Sink receives a replayed session, as a driver's terminal reader
// would.
type Sink interface {
	// Output delivers terminal output.
	Output(data []byte)
	// Input reports a keystroke chunk the recording sent.
	Input(data []byte)
	// Resize reports a terminal size change.
	Resize(rows, cols uint16)
	// Exit reports the end of the process.
	Exit(code int, phase string)
	// Tick reports the replay clock before each event, so a detector
	// can measure idle periods against recorded time.
	Tick(at time.Duration)
}

// Replay plays the fixture into a sink. Speed scales the recorded
// delays: 1 is real time, 0 replays instantly on the recorded clock.
func Replay(ctx context.Context, fixture Fixture, sink Sink, speed float64) error {
	start := time.Now()
	for _, event := range fixture.Events {
		if speed > 0 {
			due := start.Add(time.Duration(float64(event.At()) / speed))
			if wait := time.Until(due); wait > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
			}
		} else if err := ctx.Err(); err != nil {
			return err
		}
		sink.Tick(event.At())
		switch event.Kind {
		case Output:
			sink.Output(event.Data)
		case Input:
			sink.Input(event.Data)
		case Resize:
			sink.Resize(event.Rows, event.Cols)
		case Exit:
			sink.Exit(event.ExitCode, event.Phase)
		}
	}
	return nil
}
