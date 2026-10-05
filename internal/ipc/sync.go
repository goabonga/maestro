// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/integration"
	"github.com/goabonga/maestro/internal/scheduler"
	"github.com/goabonga/maestro/internal/testrun"
	"github.com/goabonga/maestro/internal/worktree"
)

// syncView is the JSON shape of a SYNC operation.
type syncView struct {
	OperationID   string    `json:"operation_id"`
	ProjectID     string    `json:"project_id"`
	Branch        string    `json:"branch,omitempty"`
	State         string    `json:"state"`
	PreviousSHA   string    `json:"previous_sha"`
	SyncedSHA     string    `json:"synced_sha"`
	ConfigID      string    `json:"config_id"`
	Diagnostics   []string  `json:"diagnostics,omitempty"`
	TestReportIDs []string  `json:"test_report_ids"`
	Error         string    `json:"error,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// viewSync renders a SYNC operation.
func viewSync(op integration.Operation) syncView {
	reports := op.TestReportIDs
	if reports == nil {
		reports = []string{}
	}
	return syncView{
		OperationID: op.ID, ProjectID: op.ProjectID, State: string(op.State), PreviousSHA: op.IntegrationBaseSHA,
		SyncedSHA: op.SourceHeadSHA, ConfigID: op.ConfigID, TestReportIDs: reports, Error: op.Error,
		StartedAt: op.StartedAt, UpdatedAt: op.UpdatedAt,
	}
}

// syncRoutes registers the sync routes; starting a sync is idempotent.
func (s *Server) syncRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/syncs", idempotent(s.DB, s.startSync))
	mux.HandleFunc("GET /v1/syncs/{id}", s.showSync)
}

// syncProject resolves the project a sync request names, writing the
// error envelope when it cannot.
func (s *Server) syncProject(w http.ResponseWriter, r *http.Request, id string) (worktree.Project, bool) {
	if s.Sync == nil {
		fail(w, r, http.StatusNotFound, CodeNotFound, "this daemon does not sync")
		return worktree.Project{}, false
	}
	if strings.TrimSpace(id) == "" {
		fail(w, r, http.StatusBadRequest, CodeInvalidRequest, "a sync request needs its project_id")
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

// syncRequest is the body of POST /v1/syncs.
type syncRequest struct {
	ProjectID string `json:"project_id"`
	Branch    string `json:"branch"`
}

// startSync freezes the commit a branch of the project's user
// repository points to, checks it and journals the SYNC operation, then
// answers 202 with the operation while the import, the tests and the
// advance of the integration branch run in the background. The tests
// are those of the configuration snapshot of the user repository taken
// now, and hold one test slot of the global capacity until the sync
// ends. A refusal journals nothing.
func (s *Server) startSync(w http.ResponseWriter, r *http.Request) {
	var request syncRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		fail(w, r, http.StatusBadRequest, CodeInvalidRequest, "body must be {\"project_id\": \"<id>\", \"branch\": \"<name>\"}")
		return
	}
	project, ok := s.syncProject(w, r, request.ProjectID)
	if !ok {
		return
	}
	configID, ok := s.snapshotProject(w, r, project)
	if !ok {
		return
	}
	snapshot, err := config.LoadSnapshot(s.DB, configID)
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	release := func() {}
	if s.Capacity != nil {
		slot, err := s.Capacity.Reserve(scheduler.Tests, project.ID)
		if errors.Is(err, scheduler.ErrFull) {
			fail(w, r, http.StatusConflict, CodeConflict, err.Error())
			return
		}
		if err != nil {
			fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
			return
		}
		release = slot.Release
	}
	plan, err := s.Sync.Prepare(integration.SyncRequest{
		Project: project, Branch: request.Branch, ConfigID: configID, Tests: testrun.Commands(snapshot.Config),
	})
	if err != nil {
		release()
		status, code := http.StatusInternalServerError, CodeInternal
		switch {
		case errors.Is(err, integration.ErrSyncSource):
			status, code = http.StatusBadRequest, CodeInvalidRequest
		case errors.Is(err, integration.ErrSyncUpToDate), errors.Is(err, integration.ErrSyncDiverged),
			errors.Is(err, integration.ErrSyncUntested):
			status, code = http.StatusConflict, CodeConflict
		}
		fail(w, r, status, code, err.Error())
		return
	}
	s.syncs.Add(1)
	go func() {
		defer s.syncs.Done()
		defer release()
		// The outcome, success or failure, is journaled on the operation.
		_, _ = s.Sync.Run(plan)
	}()
	view := viewSync(plan.Operation)
	view.Branch, view.Diagnostics = plan.Branch, plan.Diagnostics
	reply(w, r, http.StatusAccepted, view)
}

// showSync returns one SYNC operation of the project named by
// ?project_id=.
func (s *Server) showSync(w http.ResponseWriter, r *http.Request) {
	project, ok := s.syncProject(w, r, r.URL.Query().Get("project_id"))
	if !ok {
		return
	}
	id := r.PathValue("id")
	op, err := s.Sync.Store.Get(id)
	if errors.Is(err, integration.ErrNotFound) || err == nil && (op.ProjectID != project.ID || op.Type != integration.Sync) {
		fail(w, r, http.StatusNotFound, CodeNotFound, fmt.Sprintf("unknown sync in project %s: %s", project.ID, id))
		return
	}
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	reply(w, r, http.StatusOK, viewSync(op))
}

// Wait blocks until every sync started in the background has ended.
func (s *Server) Wait() {
	s.syncs.Wait()
}
