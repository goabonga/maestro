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
	"text/tabwriter"
	"time"

	"github.com/goabonga/maestro/internal/transport"
)

// workerUsage lists the worker subcommands.
const workerUsage = `usage: maestro worker list | show <name> [--project <id>] [--socket <path>]`

// workerDocument is a worker as the daemon reports it.
type workerDocument struct {
	ProjectID  string `json:"project_id"`
	Name       string `json:"name"`
	Agent      string `json:"agent"`
	AgentKind  string `json:"agent_kind"`
	Driver     string `json:"driver"`
	Repository string `json:"repository"`
	State      string `json:"state"`
	Assignment *struct {
		TaskID string `json:"task_id"`
		Role   string `json:"role"`
		TurnID string `json:"turn_id"`
	} `json:"assignment"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Events    []struct {
		Event  string    `json:"event"`
		From   string    `json:"from"`
		To     string    `json:"to"`
		TaskID string    `json:"task_id"`
		Reason string    `json:"reason"`
		At     time.Time `json:"at"`
	} `json:"events"`
}

// assignment renders the worker's current assignment, or "-".
func (w workerDocument) assignment() string {
	if w.Assignment == nil {
		return "-"
	}
	return w.Assignment.TaskID + " (" + w.Assignment.Role + ")"
}

// workerCommand dispatches the worker subcommands. Both read the worker
// registry from the daemon over its socket, on the project of the
// current repository or the one named by --project.
func workerCommand(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New(workerUsage)
	}
	command, rest := args[0], args[1:]
	flags := flag.NewFlagSet("maestro worker "+command, flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", transport.DefaultSocket(), "daemon Unix socket path")
	projectID := flags.String("project", "", "project id (default: the project of the current repository)")
	positionals, err := parseInterleaved(flags, rest)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	arity := map[string]int{"list": 0, "show": 1}
	want, known := arity[command]
	if !known {
		return fmt.Errorf("unknown worker command: %s", command)
	}
	if len(positionals) != want {
		return errors.New(workerUsage)
	}
	project, err := selectProject(*projectID)
	if err != nil {
		return err
	}
	client := daemonClient{socket: *socket}
	if command == "list" {
		return workerList(ctx, client, project, output)
	}
	return workerShow(ctx, client, project, positionals[0], output)
}

// workerList prints the workers of the project.
func workerList(ctx context.Context, client daemonClient, project string, output io.Writer) error {
	var workers []workerDocument
	if err := client.call(ctx, http.MethodGet, "/v1/workers?project_id="+url.QueryEscape(project), nil, &workers); err != nil {
		return err
	}
	if len(workers) == 0 {
		_, err := fmt.Fprintln(output, "no workers")
		return err
	}
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "NAME\tAGENT\tDRIVER\tSTATE\tASSIGNMENT")
	for _, w := range workers {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", w.Name, w.Agent, w.Driver, w.State, w.assignment())
	}
	return table.Flush()
}

// workerShow prints one worker with its recent events.
func workerShow(ctx context.Context, client daemonClient, project, name string, output io.Writer) error {
	var w workerDocument
	path := "/v1/workers/" + url.PathEscape(name) + "?project_id=" + url.QueryEscape(project)
	if err := client.call(ctx, http.MethodGet, path, nil, &w); err != nil {
		return err
	}
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintf(table, "worker:\t%s\n", w.Name)
	fmt.Fprintf(table, "project:\t%s\n", w.ProjectID)
	fmt.Fprintf(table, "agent:\t%s (%s)\n", w.Agent, w.AgentKind)
	fmt.Fprintf(table, "driver:\t%s\n", w.Driver)
	fmt.Fprintf(table, "state:\t%s\n", w.State)
	if w.Reason != "" {
		fmt.Fprintf(table, "reason:\t%s\n", w.Reason)
	}
	if w.Assignment != nil {
		fmt.Fprintf(table, "task:\t%s\n", w.Assignment.TaskID)
		fmt.Fprintf(table, "role:\t%s\n", w.Assignment.Role)
		fmt.Fprintf(table, "turn:\t%s\n", w.Assignment.TurnID)
	} else {
		fmt.Fprintf(table, "assignment:\t-\n")
	}
	fmt.Fprintf(table, "repository:\t%s\n", w.Repository)
	fmt.Fprintf(table, "created:\t%s\n", w.CreatedAt.Local().Format(time.DateTime))
	fmt.Fprintf(table, "updated:\t%s\n", w.UpdatedAt.Local().Format(time.DateTime))
	if err := table.Flush(); err != nil {
		return err
	}
	if len(w.Events) == 0 {
		return nil
	}
	fmt.Fprintln(output)
	table = tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "AT\tEVENT\tFROM\tTO\tTASK\tREASON")
	for _, event := range w.Events {
		from, taskID := event.From, event.TaskID
		if from == "" {
			from = "-"
		}
		if taskID == "" {
			taskID = "-"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			event.At.Local().Format(time.DateTime), event.Event, from, event.To, taskID, event.Reason)
	}
	return table.Flush()
}
