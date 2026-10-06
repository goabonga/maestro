// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goabonga/maestro/internal/worker"
	"github.com/goabonga/maestro/internal/worktree"
)

// sweepInterval is the period at which every project is driven again,
// so that an input wait past its bound is noticed without any request.
const sweepInterval = time.Minute

// driver runs the task engine of the daemon in the background: one
// Drive per project at a time, until the daemon stops.
type driver struct {
	ctx    context.Context
	engine *worker.Engine
	store  worktree.Store
	output *lockedWriter

	mu      sync.Mutex
	closed  bool
	running sync.WaitGroup
}

// lockedWriter serializes the reports of the background drives.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// project drives the tasks of one project in the background. A drive
// asked while another advances the project makes it run one more pass.
func (d *driver) project(project worktree.Project) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.running.Add(1)
	go func() {
		defer d.running.Done()
		err := d.engine.Drive(d.ctx, project)
		if err != nil && !errors.Is(err, context.Canceled) {
			_, _ = fmt.Fprintf(d.output, "drive the tasks of project %s: %v\n", project.ID, err)
		}
	}()
}

// all drives the tasks of every registered project.
func (d *driver) all() {
	projects, err := d.store.Projects()
	if err != nil {
		_, _ = fmt.Fprintf(d.output, "list the projects to drive: %v\n", err)
		return
	}
	for _, project := range projects {
		d.project(project)
	}
}

// sweep drives every project now, then at every interval until the
// context ends.
func (d *driver) sweep(interval time.Duration) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.running.Add(1)
	d.mu.Unlock()
	go func() {
		defer d.running.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			d.all()
			select {
			case <-d.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// close refuses new drives and waits for the running ones, which end
// once the driver's context is done.
func (d *driver) close() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.running.Wait()
}

// advancing wraps the daemon's routes: every request that creates or
// changes a task, such as a creation or a resume, drives the projects
// once it is answered.
func advancing(next http.Handler, d *driver) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.Method == http.MethodPost && (r.URL.Path == "/v1/tasks" || strings.HasPrefix(r.URL.Path, "/v1/tasks/")) {
			d.all()
		}
	})
}
