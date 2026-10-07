// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// maxCommand bounds the text typed in the command bar.
const maxCommand = 256

// commandUsage lists the commands of the command bar.
const commandUsage = "commands: start <agent> [n] · stop <worker>"

// commandedMsg reports the outcome of a command.
type commandedMsg struct {
	notice string
	err    error
}

// commandProject is the project a command acts on: the one shown, or
// the selected one on the dashboard.
func (m Model) commandProject() (string, bool) {
	if m.project != "" {
		return m.project, true
	}
	if m.screen == dashboardScreen && len(m.projects) > 0 {
		return m.projects[m.cursor].ID, true
	}
	return "", false
}

// commandKey edits the command bar, runs the command on enter and leaves
// the bar on esc.
func (m Model) commandKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyEsc:
		m.commanding, m.input = false, ""
	case tea.KeyBackspace:
		if runes := []rune(m.input); len(runes) > 0 {
			m.input = string(runes[:len(runes)-1])
		}
	case tea.KeySpace:
		if len(m.input) < maxCommand {
			m.input += " "
		}
	case tea.KeyEnter:
		line := m.input
		m.commanding, m.input = false, ""
		if strings.TrimSpace(line) == "" {
			return m, nil
		}
		m.notice, m.noticeErr = "", nil
		cmd, err := m.command(line)
		if err != nil {
			m.noticeErr = err
			return m, nil
		}
		return m, cmd
	case tea.KeyRunes:
		for _, r := range msg.Runes {
			if unicode.IsPrint(r) && len(m.input) < maxCommand {
				m.input += string(r)
			}
		}
	}
	return m, nil
}

// command parses a command line and returns the request it runs, or the
// error that refuses it before any request.
func (m Model) command(line string) (tea.Cmd, error) {
	fields := strings.Fields(line)
	project, ok := m.commandProject()
	if !ok {
		return nil, errors.New("no project selected")
	}
	c := m.client
	switch {
	case fields[0] == "start" && (len(fields) == 2 || len(fields) == 3):
		agent, count := fields[1], 1
		if len(fields) == 3 {
			n, err := strconv.Atoi(fields[2])
			if err != nil || n < 1 {
				return nil, fmt.Errorf("start: the count must be a number of at least 1, not %q", fields[2])
			}
			count = n
		}
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
			defer cancel()
			started, err := c.startWorkers(ctx, project, agent, count)
			if err != nil {
				return commandedMsg{err: fmt.Errorf("start %s: %w", agent, err)}
			}
			names := make([]string, 0, len(started))
			for _, w := range started {
				names = append(names, w.Name)
			}
			return commandedMsg{notice: "starting " + strings.Join(names, ", ")}
		}, nil
	case fields[0] == "stop" && len(fields) == 2:
		name := fields[1]
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
			defer cancel()
			stopped, err := c.stopWorker(ctx, project, name)
			if err != nil {
				return commandedMsg{err: fmt.Errorf("stop %s: %w", name, err)}
			}
			return commandedMsg{notice: stopped.Name + ": " + stopped.State}
		}, nil
	}
	return nil, fmt.Errorf("unknown command %q; %s", line, commandUsage)
}

// commanded records the outcome of a command and refreshes the screen.
func (m Model) commanded(msg commandedMsg) (tea.Model, tea.Cmd) {
	m.notice, m.noticeErr = msg.notice, msg.err
	return m, m.refresh()
}
