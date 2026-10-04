// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package cli provides the bootstrap command-line interface.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"

	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/worktree"
)

// Run parses the public CLI flags, runs a subcommand, or writes its version or help.
func Run(ctx context.Context, args []string, output io.Writer, version string) error {
	flags := flag.NewFlagSet("maestro", flag.ContinueOnError)
	flags.SetOutput(output)
	showVersion := flags.Bool("version", false, "print version")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *showVersion {
		_, err := fmt.Fprintf(output, "maestro %s\n", version)
		return err
	}
	if flags.NArg() == 0 {
		flags.Usage()
		return nil
	}
	switch flags.Arg(0) {
	case "init":
		return initProject(flags.Args()[1:], output)
	case "status":
		return status(ctx, flags.Args()[1:], output)
	case "project":
		return projectCommand(flags.Args()[1:], output)
	case "worktree":
		return worktreeCommand(flags.Args()[1:], output)
	case "diff":
		return diff(flags.Args()[1:], output)
	default:
		return fmt.Errorf("unknown command: %s", flags.Arg(0))
	}
}

// initProject registers the repository containing the given path (the
// working directory by default) and imports it into Maestro's data
// directory. Running it again on the same repository is harmless.
func initProject(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("maestro init", flag.ContinueOnError)
	flags.SetOutput(output)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("unexpected argument: %s", flags.Arg(1))
	}
	path := "."
	if flags.NArg() == 1 {
		path = flags.Arg(0)
	}
	store, err := worktree.DefaultStore()
	if err != nil {
		return err
	}
	project, created, err := store.Init(path)
	if err != nil {
		return err
	}
	if !created {
		_, err = fmt.Fprintf(output, "project already registered: %s\n", project.ID)
		return err
	}
	_, err = fmt.Fprintf(output, "project registered: %s\n", project.ID)
	return err
}

// status asks the running daemon for its health over its Unix socket.
func status(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("maestro status", flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", transport.DefaultSocket(), "daemon Unix socket path")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument: %s", flags.Arg(0))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://maestro/healthz", nil)
	if err != nil {
		return err
	}
	response, err := transport.Client(*socket).Do(request)
	if err != nil {
		return fmt.Errorf("daemon not reachable at %s: %w", *socket, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %s", response.Status)
	}
	_, err = io.Copy(output, io.LimitReader(response.Body, 1<<16))
	return err
}
