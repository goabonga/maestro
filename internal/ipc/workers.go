// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/goabonga/maestro/internal/worker"
	"github.com/goabonga/maestro/internal/worktree"
)

// RecentWorkerEvents bounds the events a worker is shown with: the most
// recent ones, oldest first.
const RecentWorkerEvents = 20

// workerView is the JSON shape of a worker.
type workerView struct {
	ProjectID  string            `json:"project_id"`
	Name       string            `json:"name"`
	Agent      string            `json:"agent"`
	AgentKind  string            `json:"agent_kind"`
	Driver     string            `json:"driver"`
	Repository string            `json:"repository"`
	State      string            `json:"state"`
	Version    int64             `json:"version"`
	Assignment *assignmentView   `json:"assignment,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	Events     []workerEventView `json:"events,omitempty"`
}

// assignmentView is the JSON shape of a worker's current assignment.
type assignmentView struct {
	TaskID string `json:"task_id"`
	Role   string `json:"role"`
	TurnID string `json:"turn_id"`
}

// workerEventView is the JSON shape of a recorded worker event.
type workerEventView struct {
	Event  string    `json:"event"`
	From   string    `json:"from,omitempty"`
	To     string    `json:"to"`
	TaskID string    `json:"task_id,omitempty"`
	TurnID string    `json:"turn_id,omitempty"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// viewWorker renders a worker.
func viewWorker(w worker.Worker) workerView {
	view := workerView{
		ProjectID: w.ProjectID, Name: w.Name, Agent: w.Agent, AgentKind: w.AgentKind, Driver: w.Driver,
		Repository: w.Repository, State: string(w.State), Version: w.Version, Reason: w.Reason,
		CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
	if w.Assignment != nil {
		view.Assignment = &assignmentView{TaskID: w.Assignment.TaskID, Role: string(w.Assignment.Role), TurnID: w.Assignment.TurnID}
	}
	return view
}

// workerRoutes registers the worker routes, which only read the
// registry.
func (s *Server) workerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workers", s.listWorkers)
	mux.HandleFunc("GET /v1/workers/{name}", s.showWorker)
}

// workerProject resolves the project a worker request names, writing
// the error envelope when it cannot.
func (s *Server) workerProject(w http.ResponseWriter, r *http.Request, id string) (worktree.Project, bool) {
	if s.Workers == nil {
		fail(w, r, http.StatusNotFound, CodeNotFound, "this daemon does not serve workers")
		return worktree.Project{}, false
	}
	if strings.TrimSpace(id) == "" {
		fail(w, r, http.StatusBadRequest, CodeInvalidRequest, "a worker request needs its project_id")
		return worktree.Project{}, false
	}
	project, ok, err := s.Store.Lookup(id)
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return worktree.Project{}, false
	}
	if !ok {
		fail(w, r, http.StatusNotFound, CodeNotFound, "unknown project: "+id)
		return worktree.Project{}, false
	}
	return project, true
}

// listWorkers returns the workers of the project named by
// ?project_id=, by name.
func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request) {
	project, ok := s.workerProject(w, r, r.URL.Query().Get("project_id"))
	if !ok {
		return
	}
	workers, err := s.Workers.List(project.ID)
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	views := make([]workerView, 0, len(workers))
	for _, found := range workers {
		views = append(views, viewWorker(found))
	}
	reply(w, r, http.StatusOK, views)
}

// showWorker returns one worker of the project named by ?project_id=,
// with its most recent events.
func (s *Server) showWorker(w http.ResponseWriter, r *http.Request) {
	project, ok := s.workerProject(w, r, r.URL.Query().Get("project_id"))
	if !ok {
		return
	}
	name := r.PathValue("name")
	found, err := s.Workers.Get(project.ID, name)
	if errors.Is(err, worker.ErrNotFound) {
		fail(w, r, http.StatusNotFound, CodeNotFound, fmt.Sprintf("unknown worker in project %s: %s", project.ID, name))
		return
	}
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	records, err := s.Workers.Events(project.ID, found.Name)
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if len(records) > RecentWorkerEvents {
		records = records[len(records)-RecentWorkerEvents:]
	}
	view := viewWorker(found)
	for _, record := range records {
		view.Events = append(view.Events, workerEventView{
			Event: string(record.Event), From: string(record.From), To: string(record.To),
			TaskID: record.TaskID, TurnID: record.TurnID, Reason: record.Reason, At: record.At,
		})
	}
	reply(w, r, http.StatusOK, view)
}
