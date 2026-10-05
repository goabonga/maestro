// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package tui is the terminal dashboard of the maestro client: a Bubble
// Tea program that reads the daemon's versioned API over its socket and
// shows the daemon status, the registered projects, their tasks and one
// task's history, refreshed periodically. It also attaches the terminal
// to a session, suspending itself for the time of the attach.
package tui

import (
	"context"
	"errors"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// DefaultInterval is the refresh period when Options leaves it unset.
const DefaultInterval = 2 * time.Second

// requestTimeout bounds one refresh.
const requestTimeout = 5 * time.Second

// Options configures the dashboard.
type Options struct {
	// Socket is the daemon's Unix socket.
	Socket string
	// Project, when set, opens the dashboard on that project's tasks.
	Project string
	// Interval is the refresh period; zero means DefaultInterval.
	Interval time.Duration
	// Renderer styles the output; nil means a renderer detecting
	// nothing, which writes plain text.
	Renderer *lipgloss.Renderer
}

// screen identifies what the dashboard shows.
type screen int

// The screens of the dashboard.
const (
	dashboardScreen screen = iota
	tasksScreen
	detailScreen
)

// tickMsg asks for a periodic refresh.
type tickMsg struct{}

// refreshedMsg carries the result of one refresh.
type refreshedMsg struct {
	seq      int
	status   statusDocument
	projects []projectDocument
	tasks    []taskDocument
	detail   taskDocument
	// err is set when the daemon could not be reached.
	err error
	// screenErr is set when the daemon answered the screen's request
	// with an error.
	screenErr error
}

// Model is the Bubble Tea model of the dashboard.
type Model struct {
	client   client
	interval time.Duration
	styles   styles

	screen     screen
	project    string
	taskID     string
	cursor     int
	taskCursor int

	seq       int
	loading   bool
	loaded    bool
	err       error
	screenErr error
	status    statusDocument
	projects  []projectDocument
	tasks     []taskDocument
	detail    taskDocument
	width     int

	// prompting is set while the attach prompt reads a session id into
	// input.
	prompting bool
	input     string
	// notice and noticeErr report the outcome of the last attach.
	notice    string
	noticeErr error
}

// New returns the dashboard model for the given options.
func New(options Options) Model {
	interval := options.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	renderer := options.Renderer
	if renderer == nil {
		renderer = lipgloss.NewRenderer(io.Discard)
	}
	m := Model{client: newClient(options.Socket), interval: interval, styles: newStyles(renderer)}
	if options.Project != "" {
		m.screen, m.project = tasksScreen, options.Project
	}
	m.seq, m.loading = 1, true
	return m
}

// Init starts the first refresh and the refresh timer.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.fetch(), m.tick())
}

// tick arms the next periodic refresh.
func (m Model) tick() tea.Cmd {
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return tickMsg{} })
}

// refresh starts a new refresh of the current screen; a result of an
// earlier refresh still in flight is then ignored.
func (m *Model) refresh() tea.Cmd {
	m.seq++
	m.loading = true
	return m.fetch()
}

// fetch reads what the current screen shows, for refresh number m.seq.
func (m Model) fetch() tea.Cmd {
	c, current, project, id, seq := m.client, m.screen, m.project, m.taskID, m.seq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		msg := refreshedMsg{seq: seq}
		msg.status, msg.err = c.status(ctx)
		if msg.err != nil {
			return msg
		}
		switch current {
		case dashboardScreen:
			msg.projects, msg.screenErr = c.projects(ctx)
		case tasksScreen:
			msg.tasks, msg.screenErr = c.tasks(ctx, project)
		case detailScreen:
			msg.detail, msg.screenErr = c.task(ctx, project, id)
		}
		if errors.Is(msg.screenErr, ErrUnreachable) {
			msg.err, msg.screenErr = msg.screenErr, nil
		}
		return msg
	}
}

// Update applies a key, a refresh result, a timer tick or a resize.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.prompting {
			return m.promptKey(msg)
		}
		return m.key(msg)
	case attachedMsg:
		return m.attached(msg)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tickMsg:
		if m.loading {
			return m, m.tick()
		}
		return m, tea.Batch(m.refresh(), m.tick())
	case refreshedMsg:
		if msg.seq != m.seq {
			return m, nil
		}
		m.apply(msg)
		return m, nil
	}
	return m, nil
}

// apply stores the result of the latest refresh.
func (m *Model) apply(msg refreshedMsg) {
	m.loading, m.loaded = false, true
	m.err, m.screenErr = msg.err, msg.screenErr
	if msg.err != nil {
		return
	}
	m.status = msg.status
	if msg.screenErr != nil {
		return
	}
	switch m.screen {
	case dashboardScreen:
		m.projects = msg.projects
		m.cursor = clamp(m.cursor, len(m.projects))
	case tasksScreen:
		m.tasks = msg.tasks
		m.taskCursor = clamp(m.taskCursor, len(m.tasks))
	case detailScreen:
		m.detail = msg.detail
	}
}

// clamp keeps a cursor inside a list of n rows.
func clamp(cursor, n int) int {
	if cursor >= n {
		cursor = n - 1
	}
	if cursor < 0 {
		cursor = 0
	}
	return cursor
}

// key handles one key press.
func (m Model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "r":
		return m, m.refresh()
	case "a":
		m.prompting, m.input = true, ""
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "enter":
		return m.open()
	case "esc", "backspace":
		return m.back()
	}
	return m, nil
}

// move shifts the cursor of the current list.
func (m *Model) move(delta int) {
	switch m.screen {
	case dashboardScreen:
		m.cursor = clamp(m.cursor+delta, len(m.projects))
	case tasksScreen:
		m.taskCursor = clamp(m.taskCursor+delta, len(m.tasks))
	}
}

// open enters the selected project or task.
func (m Model) open() (tea.Model, tea.Cmd) {
	switch {
	case m.screen == dashboardScreen && len(m.projects) > 0:
		m.screen, m.project = tasksScreen, m.projects[m.cursor].ID
		m.tasks, m.taskCursor, m.screenErr, m.loaded = nil, 0, nil, false
		return m, m.refresh()
	case m.screen == tasksScreen && len(m.tasks) > 0:
		m.screen, m.taskID = detailScreen, m.tasks[m.taskCursor].ID
		m.detail, m.screenErr, m.loaded = taskDocument{}, nil, false
		return m, m.refresh()
	}
	return m, nil
}

// back returns to the previous screen.
func (m Model) back() (tea.Model, tea.Cmd) {
	switch m.screen {
	case detailScreen:
		m.screen, m.taskID, m.screenErr = tasksScreen, "", nil
		return m, m.refresh()
	case tasksScreen:
		m.screen, m.project, m.screenErr = dashboardScreen, "", nil
		return m, m.refresh()
	}
	return m, nil
}

// Run runs the dashboard on a terminal until the user quits or ctx is
// cancelled.
func Run(ctx context.Context, options Options, input io.Reader, output io.Writer) error {
	if options.Renderer == nil {
		options.Renderer = lipgloss.NewRenderer(output)
	}
	program := tea.NewProgram(New(options), tea.WithContext(ctx), tea.WithInput(input),
		tea.WithOutput(output), tea.WithAltScreen())
	_, err := program.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}
