// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/goabonga/maestro/internal/integration"
	"github.com/goabonga/maestro/internal/worktree"
)

// publicationView is the JSON shape of a publication to the user
// repository.
type publicationView struct {
	ProjectID   string `json:"project_id"`
	Reference   string `json:"reference"`
	Previous    string `json:"previous_sha,omitempty"`
	Published   string `json:"published_sha"`
	UpToDate    bool   `json:"up_to_date"`
	OperationID string `json:"operation_id,omitempty"`
	State       string `json:"state,omitempty"`
}

// publishRoutes registers the publication route; it is idempotent.
func (s *Server) publishRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{id}/publish", idempotent(s.DB, s.publishProject))
}

// publishProject fast-forwards refs/heads/maestro/integration of the
// project's user repository to the head of its private integration
// branch. A refused publication is a conflict naming its cause.
func (s *Server) publishProject(w http.ResponseWriter, r *http.Request) {
	if s.Operations == nil {
		fail(w, r, http.StatusNotFound, CodeNotFound, "this daemon does not publish")
		return
	}
	id := r.PathValue("id")
	project, ok, err := s.Store.Lookup(id)
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if !ok {
		fail(w, r, http.StatusNotFound, CodeNotFound, "unknown project: "+id)
		return
	}
	if project.State() != worktree.StateOK {
		fail(w, r, http.StatusConflict, CodeConflict,
			fmt.Sprintf("the repository of project %s is missing at %s; relocate it first", project.ID, project.UserRepository))
		return
	}
	publication, err := s.Operations.PublishToUser(project)
	for _, refused := range []error{
		integration.ErrPublishUntested, integration.ErrPublishCheckedOut,
		integration.ErrPublishDiverged, integration.ErrPublishConcurrent,
	} {
		if errors.Is(err, refused) {
			fail(w, r, http.StatusConflict, CodeConflict, err.Error())
			return
		}
	}
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	reply(w, r, http.StatusOK, publicationView{
		ProjectID: project.ID, Reference: integration.UserPublishRef, Previous: publication.Previous,
		Published: publication.Published, UpToDate: publication.UpToDate,
		OperationID: publication.Operation.ID, State: string(publication.Operation.State),
	})
}
