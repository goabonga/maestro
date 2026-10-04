// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/worktree"
)

// startTimeout bounds how long start and stop wait on the socket.
const startTimeout = 10 * time.Second

// daemonCommand dispatches the daemon lifecycle subcommands.
func daemonCommand(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: maestro daemon start | stop | status")
	}
	flags := flag.NewFlagSet("maestro daemon "+args[0], flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", transport.DefaultSocket(), "daemon Unix socket path")
	binary := flags.String("binary", "", "maestro-svc binary (default: next to maestro, then $PATH)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument: %s", flags.Arg(0))
	}
	switch args[0] {
	case "start":
		return daemonStart(ctx, *socket, *binary, output)
	case "stop":
		return daemonStop(ctx, *socket, output)
	case "status":
		return daemonStatus(ctx, *socket, output)
	default:
		return fmt.Errorf("unknown daemon command: %s", args[0])
	}
}

// health asks the daemon for its health document.
func health(ctx context.Context, socket string) (map[string]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://maestro/healthz", nil)
	if err != nil {
		return nil, err
	}
	response, err := transport.Client(socket).Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon returned %s", response.Status)
	}
	var document map[string]string
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&document); err != nil {
		return nil, err
	}
	return document, nil
}

// daemonStatus reports liveness by connecting to the socket; nothing
// else, in particular no PID file, is consulted.
func daemonStatus(ctx context.Context, socket string, output io.Writer) error {
	document, err := health(ctx, socket)
	if err != nil {
		_, err = fmt.Fprintf(output, "daemon not running at %s\n", socket)
		return err
	}
	_, err = fmt.Fprintf(output, "daemon running: %s %s\n", document["service"], document["version"])
	return err
}

// daemonStart spawns maestro-svc detached and waits until it answers.
func daemonStart(ctx context.Context, socket, binary string, output io.Writer) error {
	if _, err := health(ctx, socket); err == nil {
		_, err = fmt.Fprintf(output, "daemon already running at %s\n", socket)
		return err
	}
	path, err := findDaemon(binary)
	if err != nil {
		return err
	}
	store, err := worktree.DefaultStore()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(store.Base, 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(store.Base, "svc.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- path is inside Maestro's own data directory
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	command := exec.Command(path, "--socket", socket) // #nosec G204 -- the binary is maestro's own daemon, resolved locally, never from task text
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	if err := command.Process.Release(); err != nil {
		return err
	}

	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		if document, err := health(ctx, socket); err == nil {
			_, err = fmt.Fprintf(output, "daemon running: %s %s\n", document["service"], document["version"])
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("the daemon did not answer on %s; see %s", socket, logPath)
}

// daemonStop asks a running daemon to shut down and waits until its
// socket stops answering. Stopping a stopped daemon is a no-op.
func daemonStop(ctx context.Context, socket string, output io.Writer) error {
	if _, err := health(ctx, socket); err != nil {
		_, err = fmt.Fprintf(output, "daemon not running at %s\n", socket)
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://maestro/v1/daemon/stop", nil)
	if err != nil {
		return err
	}
	response, err := transport.Client(socket).Do(request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon refused to stop: %s", response.Status)
	}
	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", socket); err != nil {
			_, err = fmt.Fprintln(output, "daemon stopped")
			return err
		} else {
			_ = conn.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("the daemon acknowledged the stop but its socket still answers")
}

// findDaemon resolves the maestro-svc binary: an explicit path, the
// directory of the running maestro, then $PATH.
func findDaemon(binary string) (string, error) {
	if binary != "" {
		return binary, nil
	}
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "maestro-svc")
		if _, err := os.Stat(sibling); err == nil {
			return sibling, nil
		}
	}
	path, err := exec.LookPath("maestro-svc")
	if err != nil {
		return "", errors.New("maestro-svc not found next to maestro or in $PATH; pass --binary")
	}
	return path, nil
}
