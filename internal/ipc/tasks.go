// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/goabonga/maestro/internal/budget"
	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/task"
	"github.com/goabonga/maestro/internal/worktree"
)

// Reasons recorded with the task transitions a client asks for.
const (
	reasonCancelled = "cancelled by the user"
	reasonResumed   = "resumed by the user"
)

// taskView is the JSON shape of a task.
type taskView struct {
	ID            string      `json:"task_id"`
	ProjectID     string      `json:"project_id"`
	Description   string      `json:"description"`
	ConfigID      string      `json:"config_id"`
	BaseSHA       string      `json:"task_base_sha"`
	Branch        string      `json:"branch"`
	State         string      `json:"state"`
	Version       int64       `json:"version"`
	ResumeState   string      `json:"resume_state,omitempty"`
	BlockedReason string      `json:"blocked_reason,omitempty"`
	HeadSHA       string      `json:"head_sha,omitempty"`
	ApprovedSHA   string      `json:"approved_sha,omitempty"`
	ResultSHA     string      `json:"result_sha,omitempty"`
	FixCycles     int         `json:"fix_cycles"`
	MaxFixCycles  int         `json:"max_fix_cycles"`
	Reason        string      `json:"reason,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
	Events        []eventView `json:"events,omitempty"`
}

// eventView is the JSON shape of a recorded task event.
type eventView struct {
	Event    string    `json:"event"`
	From     string    `json:"from,omitempty"`
	To       string    `json:"to"`
	Revision string    `json:"revision,omitempty"`
	Stale    bool      `json:"stale,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
}

// viewTask renders a task.
func viewTask(t task.Task) taskView {
	return taskView{
		ID: t.ID, ProjectID: t.ProjectID, Description: t.Description, ConfigID: t.ConfigID,
		BaseSHA: t.BaseSHA, Branch: t.Branch, State: string(t.State), Version: t.Version,
		ResumeState: string(t.ResumeState), BlockedReason: t.BlockedReason, HeadSHA: t.HeadSHA,
		ApprovedSHA: t.ApprovedSHA, ResultSHA: t.ResultSHA, FixCycles: t.FixCycles,
		MaxFixCycles: t.MaxFixCycles, Reason: t.Reason, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// taskRoutes registers the task routes; mutations are idempotent.
func (s *Server) taskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tasks", s.listTasks)
	mux.HandleFunc("POST /v1/tasks", idempotent(s.DB, s.createTask))
	mux.HandleFunc("GET /v1/tasks/{id}", s.showTask)
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", idempotent(s.DB, s.cancelTask))
	mux.HandleFunc("POST /v1/tasks/{id}/resume", idempotent(s.DB, s.resumeTask))
	mux.HandleFunc("POST /v1/tasks/{id}/config", idempotent(s.DB, s.updateTaskConfig))
}

// taskProject resolves the project a task request names, writing the
// error envelope when it cannot.
func (s *Server) taskProject(w http.ResponseWriter, r *http.Request, id string) (worktree.Project, bool) {
	if s.Tasks == nil {
		fail(w, r, http.StatusNotFound, CodeNotFound, "this daemon does not serve tasks")
		return worktree.Project{}, false
	}
	if strings.TrimSpace(id) == "" {
		fail(w, r, http.StatusBadRequest, CodeInvalidRequest, "a task request needs its project_id")
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

// projectTask loads a task of the project, writing the error envelope
// when it is unknown there.
func (s *Server) projectTask(w http.ResponseWriter, r *http.Request, project worktree.Project) (task.Task, bool) {
	id := r.PathValue("id")
	found, err := s.Tasks.Get(id)
	if errors.Is(err, task.ErrNotFound) || err == nil && found.ProjectID != project.ID {
		fail(w, r, http.StatusNotFound, CodeNotFound, fmt.Sprintf("unknown task in project %s: %s", project.ID, id))
		return task.Task{}, false
	}
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return task.Task{}, false
	}
	return found, true
}

// listTasks returns the tasks of the project named by ?project_id=.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	project, ok := s.taskProject(w, r, r.URL.Query().Get("project_id"))
	if !ok {
		return
	}
	tasks, err := s.Tasks.List(project.ID)
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	views := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		views = append(views, viewTask(t))
	}
	reply(w, r, http.StatusOK, views)
}

// showTask returns one task of the project named by ?project_id=, with
// its recorded events.
func (s *Server) showTask(w http.ResponseWriter, r *http.Request) {
	project, ok := s.taskProject(w, r, r.URL.Query().Get("project_id"))
	if !ok {
		return
	}
	found, ok := s.projectTask(w, r, project)
	if !ok {
		return
	}
	records, err := s.Tasks.Events(found.ID)
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	view := viewTask(found)
	for _, record := range records {
		view.Events = append(view.Events, eventView{
			Event: string(record.Event), From: string(record.From), To: string(record.To),
			Revision: record.Revision, Stale: record.Stale, Reason: record.Reason, At: record.At,
		})
	}
	reply(w, r, http.StatusOK, view)
}

// createRequest is the body of POST /v1/tasks.
type createRequest struct {
	ProjectID   string `json:"project_id"`
	Description string `json:"description"`
}

// createTask creates a NEW task of a project: it takes and persists the
// configuration snapshot of the user's repository as it is now, and
// starts the task from the head of the project's integration branch.
func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var request createRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		fail(w, r, http.StatusBadRequest, CodeInvalidRequest, "body must be {\"project_id\": \"<id>\", \"description\": \"<text>\"}")
		return
	}
	project, ok := s.taskProject(w, r, request.ProjectID)
	if !ok {
		return
	}
	configID, ok := s.snapshotProject(w, r, project)
	if !ok {
		return
	}
	base, err := integrationHead(project)
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	created, err := s.Tasks.Create(project.ID, request.Description, configID, base)
	if errors.Is(err, task.ErrInvalid) {
		fail(w, r, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	reply(w, r, http.StatusCreated, viewTask(created))
}

// snapshotProject takes and persists the configuration snapshot of a
// project's user repository as it is now, writing the error envelope
// when it cannot.
func (s *Server) snapshotProject(w http.ResponseWriter, r *http.Request, project worktree.Project) (string, bool) {
	if project.State() != worktree.StateOK {
		fail(w, r, http.StatusConflict, CodeConflict,
			fmt.Sprintf("the repository of project %s is missing at %s; relocate it first", project.ID, project.UserRepository))
		return "", false
	}
	workTree, err := userWorkTree(project)
	if err != nil {
		fail(w, r, http.StatusConflict, CodeConflict, err.Error())
		return "", false
	}
	snapshot, err := config.Take(workTree)
	if errors.Is(err, config.ErrInvalid) {
		fail(w, r, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return "", false
	}
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return "", false
	}
	configID, err := config.Persist(s.DB, snapshot)
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return "", false
	}
	return configID, true
}

// transitionRequest is the body of the task mutations.
type transitionRequest struct {
	ProjectID string `json:"project_id"`
}

// cancelTask cancels a task. No process, integration operation or
// publication runs for a task yet: the guard receives them as stopped,
// settled and not published. The compare-and-set transition serializes
// the cancellation against any concurrent transition.
func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, task.Input{Event: task.Cancel, Reason: reasonCancelled, Guard: task.Guard{
		Published: false, ProcessesStopped: true, OperationSettled: true,
	}})
}

// resumeTask resumes a blocked task in its stored continuation. The
// client asking for it states that the cause is lifted; no effect of an
// interrupted step remains to reconcile, since no step runs yet.
func (s *Server) resumeTask(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, task.Input{Event: task.Resume, Reason: reasonResumed, Guard: task.Guard{
		CauseLifted: true, Reconciled: true,
	}})
}

// transition applies one client event to a task of a project. A refused
// event or an unmet guard is a conflict.
func (s *Server) transition(w http.ResponseWriter, r *http.Request, in task.Input) {
	var request transitionRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		fail(w, r, http.StatusBadRequest, CodeInvalidRequest, "body must be {\"project_id\": \"<id>\"}")
		return
	}
	project, ok := s.taskProject(w, r, request.ProjectID)
	if !ok {
		return
	}
	found, ok := s.projectTask(w, r, project)
	if !ok {
		return
	}
	next, err := s.Tasks.Transition(found.ID, in)
	if errors.Is(err, task.ErrTransition) || errors.Is(err, task.ErrGuard) {
		fail(w, r, http.StatusConflict, CodeConflict, err.Error())
		return
	}
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	reply(w, r, http.StatusOK, viewTask(next))
}

// configUpdateView is the JSON shape of a configuration update.
type configUpdateView struct {
	Task     taskView        `json:"task"`
	Previous string          `json:"previous_config_id"`
	ConfigID string          `json:"config_id"`
	Updated  bool            `json:"updated"`
	Impact   config.Impact   `json:"impact,omitempty"`
	Changes  []config.Change `json:"changes"`
}

// updateTaskConfig makes a task adopt a new snapshot of its project's
// current configuration and instruction files. A snapshot equal to the
// task's own is reported with no change and stores nothing. Otherwise
// the active time interval a crash left open is reconciled first —
// charged, never refunded; a budget it exceeds does not stop the update,
// which may be what raises it — then the task adopts the snapshot after
// quiescence. No publication or integration operation exists for a task
// yet: the guard receives them as not published and settled.
func (s *Server) updateTaskConfig(w http.ResponseWriter, r *http.Request) {
	var request transitionRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		fail(w, r, http.StatusBadRequest, CodeInvalidRequest, "body must be {\"project_id\": \"<id>\"}")
		return
	}
	project, ok := s.taskProject(w, r, request.ProjectID)
	if !ok {
		return
	}
	found, ok := s.projectTask(w, r, project)
	if !ok {
		return
	}
	configID, ok := s.snapshotProject(w, r, project)
	if !ok {
		return
	}
	if configID != found.ConfigID && !found.State.Terminal() {
		_, err := budget.Store{DB: s.DB, Now: s.Tasks.Now}.Recover(found.ID)
		if errors.Is(err, budget.ErrClockRegression) {
			fail(w, r, http.StatusConflict, CodeConflict, err.Error())
			return
		}
		if err != nil && !errors.Is(err, budget.ErrExceeded) {
			fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
			return
		}
	}
	updated, err := s.Tasks.UpdateConfig(found.ID, task.ConfigUpdate{
		ConfigID: configID, Published: false, OperationSettled: true,
	})
	if errors.Is(err, task.ErrTransition) || errors.Is(err, task.ErrGuard) {
		fail(w, r, http.StatusConflict, CodeConflict, err.Error())
		return
	}
	if err != nil {
		fail(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	changes := updated.Changes
	if changes == nil {
		changes = config.Changes{}
	}
	reply(w, r, http.StatusOK, configUpdateView{
		Task: viewTask(updated.Task), Previous: updated.Previous, ConfigID: updated.Task.ConfigID,
		Updated: updated.Previous != updated.Task.ConfigID, Impact: changes.Impact(), Changes: changes,
	})
}

// userWorkTree returns the working tree of a project's user repository,
// whose configuration and instruction files a new task snapshots. The
// project records the common Git directory, so the working tree is its
// parent; a bare repository has none.
func userWorkTree(project worktree.Project) (string, error) {
	if filepath.Base(project.UserRepository) != ".git" {
		return "", fmt.Errorf("the repository of project %s has no working tree to snapshot", project.ID)
	}
	return filepath.Dir(project.UserRepository), nil
}

// integrationHead returns the commit at the head of the project's
// integration branch in its canonical repository.
func integrationHead(project worktree.Project) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/maestro/integration^{commit}")
	cmd.Dir = project.Repository()
	cmd.Env = gitEnvironment()
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read the integration head of project %s: %w", project.ID, err)
	}
	return strings.TrimSpace(string(output)), nil
}
