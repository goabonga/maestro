// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/goabonga/maestro/internal/scheduler"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/worktree"
)

// Server serves the daemon's versioned API.
type Server struct {
	DB      *state.DB
	Store   worktree.Store
	Service string
	Version string

	// Shutdown asks the daemon to stop; nil disables the stop route.
	Shutdown func()

	// Capacity exposes the global ceilings; nil hides them in status.
	Capacity *scheduler.Capacity

	// Sessions resolves streamable sessions; nil disables streaming.
	Sessions SessionSource
}

// projectView is the JSON shape of a project.
type projectView struct {
	ID         string `json:"project_id"`
	Repository string `json:"repository"`
	State      string `json:"state"`
}

// Handler returns the daemon's routes: the health endpoint and the
// versioned /v1 JSON API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"service": s.Service, "version": s.Version, "status": "ok"})
	})
	mux.HandleFunc("GET /v1/projects", s.listProjects)
	mux.HandleFunc("POST /v1/projects", idempotent(s.DB, s.registerProject))
	mux.HandleFunc("POST /v1/daemon/stop", s.stopDaemon)
	mux.HandleFunc("GET /v1/status", s.status)
	mux.HandleFunc("GET /v1/sessions/{id}/stream", s.streamSession)
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, r, http.StatusNotFound, CodeNotFound, "unknown route: "+r.URL.Path)
	})
	return mux
}

// status reports the daemon identity and the global capacity:
// consumption, ceilings and the per-project detail.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	document := map[string]interface{}{"service": s.Service, "version": s.Version}
	if s.Capacity != nil {
		document["capacity"] = s.Capacity.Usage()
	}
	reply(w, r, http.StatusOK, document)
}

// stopDaemon asks the daemon to shut down gracefully. Stopping is
// idempotent by nature — a stopped daemon no longer answers — so the
// route carries no idempotency key.
func (s *Server) stopDaemon(w http.ResponseWriter, r *http.Request) {
	if s.Shutdown == nil {
		fail(w, r, http.StatusNotFound, CodeNotFound, "this daemon cannot be stopped over its API")
		return
	}
	reply(w, r, http.StatusOK, map[string]string{"status": "stopping"})
	s.Shutdown()
}

// listProjects returns every registered project.
func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.Store.Projects()
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	views := make([]projectView, 0, len(projects))
	for _, project := range projects {
		views = append(views, projectView{ID: project.ID, Repository: project.UserRepository, State: project.State()})
	}
	reply(w, r, http.StatusOK, views)
}

// registerRequest is the body of POST /v1/projects.
type registerRequest struct {
	Path string `json:"path"`
}

// registerProject registers the repository at a path, as maestro init
// does; registering an already known repository replies with it.
func (s *Server) registerProject(w http.ResponseWriter, r *http.Request) {
	var request registerRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.Path) == "" {
		fail(w, r, http.StatusBadRequest, CodeInvalidRequest, "body must be {\"path\": \"<repository>\"}")
		return
	}
	project, created, err := s.Store.Init(request.Path)
	if err != nil {
		status, code := http.StatusInternalServerError, CodeInternal
		if strings.Contains(err.Error(), "not a Git repository") || strings.Contains(err.Error(), "no commits") {
			status, code = http.StatusBadRequest, CodeInvalidRequest
		}
		fail(w, r, status, code, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	reply(w, r, status, projectView{ID: project.ID, Repository: project.UserRepository, State: project.State()})
}
