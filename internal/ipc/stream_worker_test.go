// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package ipc

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/launcher"
	"github.com/goabonga/maestro/internal/session"
	"github.com/goabonga/maestro/internal/worker"
)

// fakePilots attaches one worker to a fixture session, or refuses with
// err, and reports how each attach ended.
type fakePilots struct {
	live     *session.Session
	err      error
	attached chan string
	ended    chan bool
}

func (f *fakePilots) Attach(projectID, name string) (*session.Session, func(bool), error) {
	f.attached <- projectID + "/" + name
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.live, func(detached bool) { f.ended <- detached }, nil
}

// newPilots returns pilots attaching a confined cat session.
func newPilots(t *testing.T) *fakePilots {
	t.Helper()
	live, err := session.Start(confined(t), session.Config{
		Spec: launcher.Spec{Argv: []string{"/bin/cat"}, Dir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Stop(200 * time.Millisecond) })
	return &fakePilots{live: live, attached: make(chan string, 4), ended: make(chan bool, 4)}
}

// awaitEnd waits for the end of an attach.
func awaitEnd(t *testing.T, pilots *fakePilots) bool {
	t.Helper()
	select {
	case detached := <-pilots.ended:
		return detached
	case <-time.After(10 * time.Second):
		t.Fatal("the attach never ended")
		return false
	}
}

func TestWorkerStreamAttachesThePilotAndReportsADetach(t *testing.T) {
	server, _, project := workerServer(t)
	pilots := newPilots(t)
	server.Pilots = pilots
	path := fmt.Sprintf("/v1/workers/claude-01/stream?project_id=%s", project.ID)

	connection := upgradeStream(t, server.Handler(), path)
	if got := <-pilots.attached; got != project.ID+"/claude-01" {
		t.Fatalf("attached %q", got)
	}
	if err := WriteFrame(connection, FrameInput, []byte("by-hand\r")); err != nil {
		t.Fatal(err)
	}
	collectOutput(t, connection, "by-hand")
	if err := WriteFrame(connection, FrameDetach, nil); err != nil {
		t.Fatal(err)
	}
	if !awaitEnd(t, pilots) {
		t.Fatal("a detach was reported as a lost connection")
	}

	// A client that hangs up loses its connection.
	connection = upgradeStream(t, server.Handler(), path)
	<-pilots.attached
	_ = connection.Close()
	if awaitEnd(t, pilots) {
		t.Fatal("a hang-up was reported as a detach")
	}
	if state := pilots.live.State(); state.Phase != session.Running {
		t.Fatalf("the end of the attach ended the session: %+v", state)
	}
}

func TestWorkerStreamRefusals(t *testing.T) {
	server, web, project := workerServer(t)
	upgrade := map[string]string{"Upgrade": StreamProtocol, "Connection": "Upgrade"}
	path := "/v1/workers/claude-01/stream?project_id=" + project.ID

	status, envelope, raw := call(t, web, "GET", path, upgrade, "")
	if status != http.StatusNotFound || envelope.Error == nil || envelope.Error.Message != "this daemon attaches no worker" {
		t.Fatalf("without pilots: status=%d body=%s", status, raw)
	}

	pilots := &fakePilots{attached: make(chan string, 4), ended: make(chan bool, 4)}
	server.Pilots = pilots
	for _, c := range []struct {
		path    string
		headers map[string]string
		err     error
		status  int
		code    string
	}{
		{"/v1/workers/claude-01/stream", upgrade, nil, http.StatusBadRequest, CodeInvalidRequest},
		{"/v1/workers/claude-01/stream?project_id=missing", upgrade, nil, http.StatusNotFound, CodeNotFound},
		{path, nil, nil, http.StatusUpgradeRequired, CodeInvalidRequest},
		{path, upgrade, worker.ErrNotFound, http.StatusNotFound, CodeNotFound},
		{path, upgrade, fmt.Errorf("attach in BUSY: %w: the turn is neither quiescent nor interrupted", worker.ErrGuard),
			http.StatusConflict, CodeConflict},
		{path, upgrade, fmt.Errorf("%w: attach in ATTACHED", worker.ErrTransition), http.StatusConflict, CodeConflict},
	} {
		pilots.err = c.err
		status, envelope, raw := call(t, web, "GET", c.path, c.headers, "")
		if status != c.status || envelope.Error == nil || envelope.Error.Code != c.code {
			t.Fatalf("%s %v: status=%d body=%s", c.path, c.err, status, raw)
		}
		if c.err != nil {
			<-pilots.attached
		}
	}
	if len(pilots.attached) != 0 || len(pilots.ended) != 0 {
		t.Fatal("a refused request attached a pilot or ended an attach")
	}
}
