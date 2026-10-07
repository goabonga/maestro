// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package tui

import (
	"context"
	"errors"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goabonga/maestro/internal/attach"
)

// ErrNoTerminal reports an attach requested while the dashboard does not
// read a terminal.
var ErrNoTerminal = errors.New("attach needs a terminal")

// errNoDriver reports an attach asked on a task no worker drives.
var errNoDriver = errors.New("no worker drives this task")

// attachedMsg reports the end of an attach.
type attachedMsg struct {
	worker string
	err    error
}

// attachExec runs one attach on the program's terminal while Bubble Tea
// is suspended. It implements tea.ExecCommand.
type attachExec struct {
	socket  string
	project string
	worker  string
	in      io.Reader
	out     io.Writer
}

// SetStdin receives the program's input.
func (a *attachExec) SetStdin(r io.Reader) { a.in = r }

// SetStdout receives the program's output.
func (a *attachExec) SetStdout(w io.Writer) { a.out = w }

// SetStderr ignores the error stream: the attach reports through Run.
func (a *attachExec) SetStderr(io.Writer) {}

// Run attaches the terminal as the worker's pilot until Ctrl-] or the
// end of its session; the attach restores the terminal before
// returning.
func (a *attachExec) Run() error {
	in, ok := a.in.(*os.File)
	if !ok {
		return ErrNoTerminal
	}
	winch, stop := attach.Resizes()
	defer stop()
	return attach.Worker(context.Background(), a.socket, a.project, a.worker,
		attach.Terminal{In: in, Out: a.out, Winch: winch})
}

// attachSelected attaches the terminal to the worker the screen points
// at: the selected worker of the workers screen, the worker shown, or
// the worker driving the task shown. Other screens show no worker, and
// the key does nothing there.
func (m Model) attachSelected() (tea.Model, tea.Cmd) {
	var name string
	switch {
	case m.screen == workersScreen && len(m.workers) > 0:
		name = m.workers[m.workerCursor].Name
	case m.screen == workerScreen:
		name = m.workerName
	case m.screen == detailScreen && len(m.drivers) > 0:
		name = m.drivers[0].Name
	case m.screen == detailScreen:
		m.notice, m.noticeErr = "", errNoDriver
		return m, nil
	default:
		return m, nil
	}
	m.notice, m.noticeErr = "", nil
	return m, m.attachTo(name)
}

// attachTo suspends the program and attaches its terminal to a worker
// of the current project; the program resumes with an attachedMsg once
// the attach returns.
func (m Model) attachTo(name string) tea.Cmd {
	return tea.Exec(&attachExec{socket: m.client.socket, project: m.project, worker: name}, func(err error) tea.Msg {
		return attachedMsg{worker: name, err: err}
	})
}

// attached records the outcome of an attach and refreshes the screen.
func (m Model) attached(msg attachedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.notice, m.noticeErr = "", msg.err
	} else {
		m.notice, m.noticeErr = "detached from "+msg.worker, nil
	}
	return m, m.refresh()
}
