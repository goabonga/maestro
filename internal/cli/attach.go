// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"

	"github.com/goabonga/maestro/internal/attach"
	"github.com/goabonga/maestro/internal/transport"
)

// attachUsage is the usage of `maestro attach`.
const attachUsage = "usage: maestro attach <worker> [--project <id>] [--socket <path>]"

// attachCommand parses `maestro attach <worker>` and attaches the
// process's own terminal as the worker's pilot, on the project of the
// current repository or the one named by --project.
func attachCommand(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("maestro attach", flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", transport.DefaultSocket(), "daemon Unix socket path")
	projectID := flags.String("project", "", "project id (default: the project of the current repository)")
	positionals, err := parseInterleaved(flags, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(positionals) != 1 {
		return errors.New(attachUsage)
	}
	project, err := selectProject(*projectID)
	if err != nil {
		return err
	}
	winch, stop := attach.Resizes()
	defer stop()
	return attach.Worker(ctx, *socket, project, positionals[0], attach.Terminal{In: os.Stdin, Out: output, Winch: winch})
}
