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
	"strings"
	"time"

	"github.com/goabonga/maestro/internal/transport"
)

// syncUsage is the usage of the sync command.
const syncUsage = `usage: maestro sync --from <branch> [--project <id>] [--socket <path>]`

// syncPoll is the delay between two reads of a running sync.
const syncPoll = 200 * time.Millisecond

// syncDocument is a SYNC operation as the daemon reports it.
type syncDocument struct {
	OperationID   string   `json:"operation_id"`
	Branch        string   `json:"branch"`
	State         string   `json:"state"`
	PreviousSHA   string   `json:"previous_sha"`
	SyncedSHA     string   `json:"synced_sha"`
	Diagnostics   []string `json:"diagnostics"`
	TestReportIDs []string `json:"test_report_ids"`
	Error         string   `json:"error"`
}

// syncCommand asks the daemon to sync the integration branch of the
// project to the commit a branch of the user repository points to now,
// then follows the sync until it is committed or rolled back.
func syncCommand(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("maestro sync", flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", transport.DefaultSocket(), "daemon Unix socket path")
	projectID := flags.String("project", "", "project id (default: the project of the current repository)")
	from := flags.String("from", "", "branch of the user repository to sync from")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *from == "" {
		return errors.New(syncUsage)
	}
	project, err := selectProject(*projectID)
	if err != nil {
		return err
	}
	client := daemonClient{socket: *socket}
	var started syncDocument
	body := map[string]string{"project_id": project, "branch": *from}
	if err := client.call(ctx, http.MethodPost, "/v1/syncs", body, &started); err != nil {
		return err
	}
	fmt.Fprintf(output, "syncing branch %s at %s from integration %s (operation %s)\n",
		started.Branch, started.SyncedSHA, started.PreviousSHA, started.OperationID)
	for _, diagnostic := range started.Diagnostics {
		fmt.Fprintf(output, "note: %s\n", diagnostic)
	}
	path := "/v1/syncs/" + url.PathEscape(started.OperationID) + "?project_id=" + url.QueryEscape(project)
	current := started
	for current.State != "COMMITTED" && current.State != "ROLLED_BACK" {
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped following sync %s, still %s: %w", started.OperationID, current.State, ctx.Err())
		case <-time.After(syncPoll):
		}
		if err := client.call(ctx, http.MethodGet, path, nil, &current); err != nil {
			return err
		}
	}
	if current.State != "COMMITTED" {
		return fmt.Errorf("sync %s rolled back, the integration is still at %s: %s",
			current.OperationID, current.PreviousSHA, current.Error)
	}
	_, err = fmt.Fprintf(output, "synced %s: integration advanced from %s\ntests: %s\n",
		current.SyncedSHA, current.PreviousSHA, strings.Join(current.TestReportIDs, ", "))
	return err
}
