// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package cli provides the bootstrap command-line interface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"

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
	case "backup":
		return backup(flags.Args()[1:], output)
	case "daemon":
		return daemonCommand(ctx, flags.Args()[1:], output)
	case "gc":
		return gc(flags.Args()[1:], output)
	case "restore":
		return restore(flags.Args()[1:], output)
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://maestro/v1/status", nil)
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
	var envelope struct {
		Data struct {
			Service  string `json:"service"`
			Version  string `json:"version"`
			Capacity map[string]struct {
				Used      int            `json:"used"`
				Limit     int            `json:"limit"`
				ByProject map[string]int `json:"by_project"`
			} `json:"capacity"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&envelope); err != nil {
		return err
	}
	fmt.Fprintf(output, "daemon: %s %s\n", envelope.Data.Service, envelope.Data.Version)
	if len(envelope.Data.Capacity) == 0 {
		return nil
	}
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "CAPACITY\tUSED\tLIMIT\tBY PROJECT")
	kinds := make([]string, 0, len(envelope.Data.Capacity))
	for kind := range envelope.Data.Capacity {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		usage := envelope.Data.Capacity[kind]
		projects := make([]string, 0, len(usage.ByProject))
		for project, count := range usage.ByProject {
			projects = append(projects, fmt.Sprintf("%s: %d", project, count))
		}
		sort.Strings(projects)
		fmt.Fprintf(table, "%s\t%d\t%d\t%s\n", kind, usage.Used, usage.Limit, strings.Join(projects, ", "))
	}
	return table.Flush()
}
