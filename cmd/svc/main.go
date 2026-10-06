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

	"github.com/goabonga/maestro/internal/integration"
	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/scheduler"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/testrun"
	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/worker"
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

// run parses the daemon flags, then starts it: migrate the store and
// reconcile the recorded workers under the user lock, bind the socket,
// serve and drive the tasks on the started workers until the context
// ends.
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
	// The runtimes of a previous daemon are lost: the recorded workers
	// are reconciled before any request can reach them.
	if err := reconcileWorkers(store, db, output); err != nil {
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
	server := &ipc.Server{
		DB: db, Store: store, Service: "maestro-svc", Version: Version, Shutdown: stop, Capacity: capacity,
		Tasks: &task.Store{DB: db}, Sync: &integration.Syncer{Store: integration.Store{DB: db}},
		Operations: &integration.Store{DB: db}, Workers: &worker.Store{DB: db},
	}
	// Workers start in confined sessions bounded by the session ceiling.
	server.Supervisor = &worker.Supervisor{Store: worker.Store{DB: db}, Projects: store, Capacity: capacity}
	// A sync runs its tests confined; a host that cannot confine has no
	// test runner, and every sync is refused rather than left untested.
	// Without confinement, every worker start is refused as well.
	// The task engine drives the tasks of a project on its IDLE workers
	// whenever a task is created or changed, a worker becomes IDLE, and
	// at every sweep.
	engine := &worker.Engine{DB: db, Projects: store, Sessions: server.Supervisor}
	if confined, err := launcher.New(); err == nil {
		runner := &testrun.Runner{Launcher: confined}
		server.Sync.Tester = runner
		server.Supervisor.Launcher = confined
		engine.Tester = runner
	}
	drives := &driver{ctx: ctx, engine: engine, store: store, output: &lockedWriter{w: output}}
	server.Supervisor.Ready = drives.project
	drives.sweep(sweepInterval)
	err = transport.Serve(ctx, listener, advancing(server.Handler(), drives))
	// The drives in progress stop their turns, a sync running in the
	// background finishes its journal, and every live worker is stopped,
	// before the store closes.
	stop()
	drives.close()
	server.Wait()
	server.Supervisor.Close()
	return err
}

// reconcileWorkers reconciles the recorded workers of every registered
// project with this freshly started daemon, searching each worker's
// repository for surviving processes, and reports every worker it moved
// or found survivors for.
func reconcileWorkers(store worktree.Store, db *state.DB, output io.Writer) error {
	projects, err := store.Projects()
	if err != nil {
		return err
	}
	workers := worker.Store{DB: db}
	for _, project := range projects {
		outcomes, err := workers.Reconcile(project.ID, launcher.Survivors)
		if err != nil {
			return fmt.Errorf("reconcile the workers of project %s: %w", project.ID, err)
		}
		for _, outcome := range outcomes {
			if outcome.Reason == "" {
				continue
			}
			if _, err := fmt.Fprintf(output, "worker %s/%s: %s -> %s: %s\n",
				project.ID, outcome.Name, outcome.From, outcome.To, outcome.Reason); err != nil {
				return err
			}
		}
	}
	return nil
}
