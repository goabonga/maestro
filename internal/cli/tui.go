// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/tui"
)

// tuiCommand opens the terminal dashboard on the daemon's socket, reading
// keys from input and drawing on output.
func tuiCommand(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("maestro tui", flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", transport.DefaultSocket(), "daemon Unix socket path")
	project := flags.String("project", "", "open on the tasks of this project id")
	interval := flags.Duration("interval", tui.DefaultInterval, "refresh period")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument: %s", flags.Arg(0))
	}
	if *interval < 100*time.Millisecond {
		return fmt.Errorf("refresh interval too short: %s (minimum 100ms)", *interval)
	}
	return tui.Run(ctx, tui.Options{Socket: *socket, Project: *project, Interval: *interval}, input, output)
}
