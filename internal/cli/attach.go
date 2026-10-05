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

// attachCommand parses `maestro attach <session>` and attaches the
// process's own terminal.
func attachCommand(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("maestro attach", flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", transport.DefaultSocket(), "daemon Unix socket path")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// Accept flags after the session too.
	if flags.NArg() == 0 {
		return errors.New("usage: maestro attach <session> [--socket <path>]")
	}
	target := flags.Arg(0)
	if err := flags.Parse(flags.Args()[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: maestro attach <session> [--socket <path>]")
	}
	winch, stop := attach.Resizes()
	defer stop()
	return attach.Run(ctx, *socket, target, attach.Terminal{In: os.Stdin, Out: output, Winch: winch})
}
