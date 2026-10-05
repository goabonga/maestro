// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/transport"
	"github.com/goabonga/maestro/internal/worktree"
)

// taskUsage lists the task subcommands.
const taskUsage = `usage: maestro task new "<description>" | show <id> | list | cancel <id> | resume <id>
       [--project <id>] [--socket <path>]`

// maxResponse bounds a daemon response read by the CLI.
const maxResponse = 4 << 20

// taskDocument is a task as the daemon reports it.
type taskDocument struct {
	ID            string    `json:"task_id"`
	ProjectID     string    `json:"project_id"`
	Description   string    `json:"description"`
	ConfigID      string    `json:"config_id"`
	BaseSHA       string    `json:"task_base_sha"`
	Branch        string    `json:"branch"`
	State         string    `json:"state"`
	Version       int64     `json:"version"`
	ResumeState   string    `json:"resume_state"`
	BlockedReason string    `json:"blocked_reason"`
	HeadSHA       string    `json:"head_sha"`
	FixCycles     int       `json:"fix_cycles"`
	MaxFixCycles  int       `json:"max_fix_cycles"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Events        []struct {
		Event    string    `json:"event"`
		From     string    `json:"from"`
		To       string    `json:"to"`
		Revision string    `json:"revision"`
		Stale    bool      `json:"stale"`
		Reason   string    `json:"reason"`
		At       time.Time `json:"at"`
	} `json:"events"`
}

// taskCommand dispatches the task subcommands. Every one talks to the
// daemon over its socket, on the project of the current repository or
// the one named by --project.
func taskCommand(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New(taskUsage)
	}
	flags := flag.NewFlagSet("maestro task "+args[0], flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", transport.DefaultSocket(), "daemon Unix socket path")
	projectID := flags.String("project", "", "project id (default: the project of the current repository)")
	positionals, err := parseInterleaved(flags, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	arity := map[string]int{"new": 1, "show": 1, "list": 0, "cancel": 1, "resume": 1}
	want, known := arity[args[0]]
	if !known {
		return fmt.Errorf("unknown task command: %s", args[0])
	}
	if len(positionals) != want {
		return errors.New(taskUsage)
	}
	project, err := selectProject(*projectID)
	if err != nil {
		return err
	}
	client := daemonClient{socket: *socket}
	switch args[0] {
	case "new":
		return taskNew(ctx, client, project, positionals[0], output)
	case "show":
		return taskShow(ctx, client, project, positionals[0], output)
	case "list":
		return taskList(ctx, client, project, output)
	case "cancel":
		return taskTransition(ctx, client, project, positionals[0], "cancel", output)
	default:
		return taskTransition(ctx, client, project, positionals[0], "resume", output)
	}
}

// parseInterleaved parses flags given before, between or after the
// positional arguments, and returns the positionals.
func parseInterleaved(flags *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		args = flags.Args()
		if len(args) == 0 {
			return positionals, nil
		}
		positionals = append(positionals, args[0])
		args = args[1:]
	}
}

// selectProject returns the explicit project, or the registered project
// of the repository holding the working directory. Outside a repository
// without --project, there is no project: the last one used is never
// picked implicitly.
func selectProject(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	store, err := worktree.DefaultStore()
	if err != nil {
		return "", err
	}
	found, ok, err := store.Find(dir)
	if err != nil {
		return "", fmt.Errorf("no project selected: %s is not inside a Git repository; pass --project <id>", dir)
	}
	if !ok {
		return "", errors.New("no project selected: the repository is not registered; run maestro init or pass --project <id>")
	}
	return found.ID, nil
}

// daemonClient calls the daemon's versioned API over its socket.
type daemonClient struct {
	socket string
}

// call sends one request and decodes the data of the envelope into
// data. A mutation carries a fresh Idempotency-Key. An error envelope
// becomes an error naming its code.
func (c daemonClient) call(ctx context.Context, method, path string, body, data any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://maestro"+path, reader)
	if err != nil {
		return err
	}
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", idempotencyKey())
	}
	response, err := transport.Client(c.socket).Do(request)
	if err != nil {
		return fmt.Errorf("daemon not reachable at %s: %w", c.socket, err)
	}
	defer func() { _ = response.Body.Close() }()
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *ipc.Problem    `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponse)).Decode(&envelope); err != nil {
		return fmt.Errorf("daemon returned %s without a valid envelope: %w", response.Status, err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("%s: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if response.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("daemon returned %s", response.Status)
	}
	if data == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, data)
}

// idempotencyKey returns a random key for one mutation.
func idempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// taskNew creates a task from its description.
func taskNew(ctx context.Context, client daemonClient, project, description string, output io.Writer) error {
	var created taskDocument
	body := map[string]string{"project_id": project, "description": description}
	if err := client.call(ctx, http.MethodPost, "/v1/tasks", body, &created); err != nil {
		return err
	}
	_, err := fmt.Fprintf(output, "task created: %s\nstate: %s\nbranch: %s\nbase: %s\n",
		created.ID, created.State, created.Branch, created.BaseSHA)
	return err
}

// taskList prints the tasks of the project.
func taskList(ctx context.Context, client daemonClient, project string, output io.Writer) error {
	var tasks []taskDocument
	if err := client.call(ctx, http.MethodGet, "/v1/tasks?project_id="+url.QueryEscape(project), nil, &tasks); err != nil {
		return err
	}
	if len(tasks) == 0 {
		_, err := fmt.Fprintln(output, "no tasks")
		return err
	}
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tSTATE\tCREATED\tDESCRIPTION")
	for _, t := range tasks {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", t.ID, t.State, t.CreatedAt.Local().Format(time.DateTime), summary(t.Description))
	}
	return table.Flush()
}

// taskShow prints one task with its recorded events.
func taskShow(ctx context.Context, client daemonClient, project, id string, output io.Writer) error {
	var t taskDocument
	path := "/v1/tasks/" + url.PathEscape(id) + "?project_id=" + url.QueryEscape(project)
	if err := client.call(ctx, http.MethodGet, path, nil, &t); err != nil {
		return err
	}
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintf(table, "task:\t%s\n", t.ID)
	fmt.Fprintf(table, "project:\t%s\n", t.ProjectID)
	fmt.Fprintf(table, "state:\t%s\n", t.State)
	if t.ResumeState != "" {
		fmt.Fprintf(table, "resume state:\t%s\n", t.ResumeState)
		fmt.Fprintf(table, "blocked reason:\t%s\n", t.BlockedReason)
	}
	fmt.Fprintf(table, "branch:\t%s\n", t.Branch)
	fmt.Fprintf(table, "base:\t%s\n", t.BaseSHA)
	if t.HeadSHA != "" {
		fmt.Fprintf(table, "head:\t%s\n", t.HeadSHA)
	}
	fmt.Fprintf(table, "config:\t%s\n", t.ConfigID)
	fmt.Fprintf(table, "fix cycles:\t%d/%d\n", t.FixCycles, t.MaxFixCycles)
	fmt.Fprintf(table, "created:\t%s\n", t.CreatedAt.Local().Format(time.DateTime))
	fmt.Fprintf(table, "updated:\t%s\n", t.UpdatedAt.Local().Format(time.DateTime))
	if err := table.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(output, "\n%s\n", t.Description)
	if len(t.Events) == 0 {
		return nil
	}
	fmt.Fprintln(output)
	table = tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "AT\tEVENT\tFROM\tTO\tREASON")
	for _, event := range t.Events {
		name := event.Event
		if event.Stale {
			name += " (stale)"
		}
		from := event.From
		if from == "" {
			from = "-"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n",
			event.At.Local().Format(time.DateTime), name, from, event.To, event.Reason)
	}
	return table.Flush()
}

// taskTransition asks the daemon to cancel or resume a task.
func taskTransition(ctx context.Context, client daemonClient, project, id, action string, output io.Writer) error {
	var t taskDocument
	path := "/v1/tasks/" + url.PathEscape(id) + "/" + action
	if err := client.call(ctx, http.MethodPost, path, map[string]string{"project_id": project}, &t); err != nil {
		return err
	}
	_, err := fmt.Fprintf(output, "task %s: %s\n", t.ID, t.State)
	return err
}

// summary returns the first line of a description, shortened for a table.
func summary(description string) string {
	line, _, _ := strings.Cut(description, "\n")
	if runes := []rune(line); len(runes) > 60 {
		return string(runes[:59]) + "…"
	}
	return line
}
