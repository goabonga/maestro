// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Command maestro-svc runs the maestro daemon, serving its versioned
// JSON API and health endpoint on a Unix socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/scheduler"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/worktree"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run parses the daemon flags, then starts it: migrate the store under
// the user lock, bind the socket, serve until the context ends.
func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("maestro-svc", flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", transport.DefaultSocket(), "Unix socket path")
	maxSessions := flags.Int("max-sessions", scheduler.DefaultSessions, "global ceiling of concurrent sessions")
	maxTests := flags.Int("max-test-jobs", scheduler.DefaultTests, "global ceiling of concurrent test runs")
	maxCommands := flags.Int("max-command-jobs", scheduler.DefaultCommands, "global ceiling of concurrent commands")
	showVersion := flags.Bool("version", false, "print version")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument: %s", flags.Arg(0))
	}
	if *showVersion {
		_, err := fmt.Fprintf(output, "maestro-svc %s\n", Version)
		return err
	}

	// The ceilings come from the daemon's own configuration, never
	// from a project's versioned file; they are validated at startup.
	capacity, err := scheduler.NewCapacity(*maxSessions, *maxTests, *maxCommands)
	if err != nil {
		return err
	}
	store, err := worktree.DefaultStore()
	if err != nil {
		return err
	}
	// Exactly one daemon per user, whatever socket it was given: the
	// kernel drops this lock when the process dies, so no PID file.
	daemonLock, err := state.Acquire(filepath.Join(store.Base, "daemon.lock"))
	if err != nil {
		return fmt.Errorf("another maestro-svc is already running for this user: %w", err)
	}
	defer func() { _ = daemonLock.Release() }()
	lock, err := state.Acquire(filepath.Join(store.Base, "lock"))
	if err != nil {
		return err
	}
	db, err := state.Open(filepath.Join(store.Base, "maestro.db"))
	if err != nil {
		_ = lock.Release()
		return err
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(state.Migrations); err != nil {
		_ = lock.Release()
		return err
	}
	listener, err := transport.Listen(*socket)
	if err != nil {
		_ = lock.Release()
		return err
	}
	if err := lock.Release(); err != nil {
		_ = listener.Close()
		return err
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	server := &ipc.Server{DB: db, Store: store, Service: "maestro-svc", Version: Version, Shutdown: stop, Capacity: capacity}
	return transport.Serve(ctx, listener, server.Handler())
}
