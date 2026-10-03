// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package transport provides HTTP process lifecycle and health endpoints.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Health returns a liveness endpoint; it does not certify resource readiness.
func Health(service, version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"service": service, "version": version, "status": "ok"}); err != nil {
			return
		}
	})
	return mux
}

// Serve drains active requests when its process context is cancelled.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		err := <-finished
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Run configures and starts a daemon listening on a Unix socket, or prints its help/version.
func Run(ctx context.Context, args []string, output io.Writer, service, version, defaultSocket string, handler http.Handler) error {
	flags := flag.NewFlagSet(service, flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", defaultSocket, "Unix socket path")
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
		_, err := fmt.Fprintf(output, "%s %s\n", service, version)
		return err
	}
	listener, err := Listen(*socket)
	if err != nil {
		return err
	}
	return Serve(ctx, listener, handler)
}
