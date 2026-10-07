// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/goabonga/maestro/internal/transport"
)

// Usages of the worker controls.
const (
	pauseUsage  = `usage: maestro pause <worker> [--project <id>] [--socket <path>]`
	resumeUsage = `usage: maestro resume <worker> [--project <id>] [--socket <path>]`
)

// pauseCommand pauses a worker: the turn it runs is interrupted and its
// task blocked, its session stopped and kept for its resume.
func pauseCommand(ctx context.Context, args []string, output io.Writer) error {
	return controlWorker(ctx, "pause", pauseUsage, args, output)
}

// resumeCommand resumes a paused worker, then follows it until it is
// IDLE or FAILED.
func resumeCommand(ctx context.Context, args []string, output io.Writer) error {
	return controlWorker(ctx, "resume", resumeUsage, args, output)
}

// controlWorker applies a control to the worker named on the command
// line, in the project of the current repository or the one named by
// --project, and prints the worker's state once the control is over.
func controlWorker(ctx context.Context, verb, usage string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("maestro "+verb, flag.ContinueOnError)
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
		return errors.New(usage)
	}
	project, err := selectProject(*projectID)
	if err != nil {
		return err
	}
	client := daemonClient{socket: *socket}
	name := positionals[0]
	var w workerDocument
	path := "/v1/workers/" + url.PathEscape(name) + "/" + verb
	if err := client.call(ctx, http.MethodPost, path, map[string]string{"project_id": project}, &w); err != nil {
		return err
	}
	show := "/v1/workers/" + url.PathEscape(w.Name) + "?project_id=" + url.QueryEscape(project)
	for w.State == "STARTING" {
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped following worker %s, still %s: %w", w.Name, w.State, ctx.Err())
		case <-time.After(workerPoll):
		}
		if err := client.call(ctx, http.MethodGet, show, nil, &w); err != nil {
			return err
		}
	}
	if w.State == "FAILED" {
		fmt.Fprintf(output, "%s: %s: %s\n", w.Name, w.State, w.Reason)
		return fmt.Errorf("worker %s failed to %s", w.Name, verb)
	}
	_, err = fmt.Fprintf(output, "%s: %s\n", w.Name, w.State)
	return err
}
