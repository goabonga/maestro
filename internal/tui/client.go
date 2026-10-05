// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/goabonga/maestro/internal/ipc"
	"github.com/goabonga/maestro/internal/transport"
)

// maxResponse bounds a daemon response read by the TUI.
const maxResponse = 4 << 20

// ErrUnreachable reports a daemon that does not answer on its socket.
var ErrUnreachable = errors.New("daemon not reachable")

// capacityUsage is the consumption of one global ceiling.
type capacityUsage struct {
	Used      int            `json:"used"`
	Limit     int            `json:"limit"`
	ByProject map[string]int `json:"by_project"`
}

// statusDocument is the daemon status.
type statusDocument struct {
	Service  string                   `json:"service"`
	Version  string                   `json:"version"`
	Capacity map[string]capacityUsage `json:"capacity"`
}

// projectDocument is a registered project.
type projectDocument struct {
	ID         string `json:"project_id"`
	Repository string `json:"repository"`
	State      string `json:"state"`
}

// eventDocument is a recorded task event.
type eventDocument struct {
	Event  string    `json:"event"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Stale  bool      `json:"stale"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// taskDocument is a task, with its events when shown alone.
type taskDocument struct {
	ID            string          `json:"task_id"`
	ProjectID     string          `json:"project_id"`
	Description   string          `json:"description"`
	ConfigID      string          `json:"config_id"`
	BaseSHA       string          `json:"task_base_sha"`
	Branch        string          `json:"branch"`
	State         string          `json:"state"`
	ResumeState   string          `json:"resume_state"`
	BlockedReason string          `json:"blocked_reason"`
	HeadSHA       string          `json:"head_sha"`
	FixCycles     int             `json:"fix_cycles"`
	MaxFixCycles  int             `json:"max_fix_cycles"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	Events        []eventDocument `json:"events"`
}

// client reads the daemon's versioned API over its socket.
type client struct {
	socket string
	http   *http.Client
}

// newClient returns a client of the daemon listening on socket.
func newClient(socket string) client {
	return client{socket: socket, http: transport.Client(socket)}
}

// get sends one read request and decodes the data of the envelope into
// data. A failed connection wraps ErrUnreachable; an error envelope
// becomes an error naming its code.
func (c client) get(ctx context.Context, path string, data any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://maestro"+path, nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%w at %s: %w", ErrUnreachable, c.socket, err)
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
	return json.Unmarshal(envelope.Data, data)
}

// status reads the daemon identity and capacity.
func (c client) status(ctx context.Context) (statusDocument, error) {
	var document statusDocument
	err := c.get(ctx, "/v1/status", &document)
	return document, err
}

// projects reads the registered projects.
func (c client) projects(ctx context.Context) ([]projectDocument, error) {
	var projects []projectDocument
	err := c.get(ctx, "/v1/projects", &projects)
	return projects, err
}

// tasks reads the tasks of a project.
func (c client) tasks(ctx context.Context, project string) ([]taskDocument, error) {
	var tasks []taskDocument
	err := c.get(ctx, "/v1/tasks?project_id="+url.QueryEscape(project), &tasks)
	return tasks, err
}

// task reads one task of a project with its events.
func (c client) task(ctx context.Context, project, id string) (taskDocument, error) {
	var t taskDocument
	err := c.get(ctx, "/v1/tasks/"+url.PathEscape(id)+"?project_id="+url.QueryEscape(project), &t)
	return t, err
}
