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

	"github.com/goabonga/maestro/internal/transport"
)

// publicationDocument is a publication as the daemon reports it.
type publicationDocument struct {
	Reference   string `json:"reference"`
	Previous    string `json:"previous_sha"`
	Published   string `json:"published_sha"`
	UpToDate    bool   `json:"up_to_date"`
	OperationID string `json:"operation_id"`
}

// publish asks the daemon to fast-forward maestro/integration of the
// user repository to the project's integration head, on the project of
// the current repository or the one named by --project.
func publish(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("maestro publish", flag.ContinueOnError)
	flags.SetOutput(output)
	socket := flags.String("socket", transport.DefaultSocket(), "daemon Unix socket path")
	projectID := flags.String("project", "", "project id (default: the project of the current repository)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument: %s", flags.Arg(0))
	}
	project, err := selectProject(*projectID)
	if err != nil {
		return err
	}
	var publication publicationDocument
	path := "/v1/projects/" + url.PathEscape(project) + "/publish"
	if err := (daemonClient{socket: *socket}).call(ctx, http.MethodPost, path, nil, &publication); err != nil {
		return err
	}
	if publication.UpToDate {
		_, err = fmt.Fprintf(output, "%s already at %s\n", publication.Reference, publication.Published)
		return err
	}
	previous := publication.Previous
	if previous == "" {
		previous = "created"
	}
	_, err = fmt.Fprintf(output, "published %s to %s (was %s)\noperation: %s\n",
		publication.Published, publication.Reference, previous, publication.OperationID)
	return err
}
