// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package tui

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
)

// styles are the styles of the dashboard.
type styles struct {
	title    lipgloss.Style
	header   lipgloss.Style
	selected lipgloss.Style
	err      lipgloss.Style
	help     lipgloss.Style
	frame    lipgloss.Style
}

// newStyles returns the styles bound to a renderer.
func newStyles(r *lipgloss.Renderer) styles {
	return styles{
		title:    r.NewStyle().Bold(true),
		header:   r.NewStyle().Bold(true),
		selected: r.NewStyle().Reverse(true),
		err:      r.NewStyle().Foreground(lipgloss.Color("1")),
		help:     r.NewStyle().Faint(true),
		frame:    r.NewStyle(),
	}
}

// View renders the current screen.
func (m Model) View() string {
	var b strings.Builder
	b.WriteString(m.styles.title.Render("maestro · "+m.heading()) + "\n\n")
	switch {
	case m.noticeErr != nil:
		b.WriteString(m.styles.err.Render(printable(m.noticeErr.Error())) + "\n\n")
	case m.notice != "":
		b.WriteString(printable(m.notice) + "\n\n")
	}
	switch {
	case m.err != nil:
		b.WriteString(m.styles.err.Render(m.err.Error()) + "\n")
		fmt.Fprintf(&b, "retrying every %s\n", m.interval)
	case !m.loaded:
		b.WriteString("loading…\n")
	default:
		fmt.Fprintf(&b, "daemon: %s %s\n", m.status.Service, m.status.Version)
		if m.screenErr != nil {
			b.WriteString("\n" + m.styles.err.Render(m.screenErr.Error()) + "\n")
		} else {
			switch m.screen {
			case dashboardScreen:
				m.viewDashboard(&b)
			case tasksScreen:
				m.viewTasks(&b)
			case detailScreen:
				m.viewDetail(&b)
			case workersScreen:
				m.viewWorkers(&b)
			case workerScreen:
				m.viewWorker(&b)
			}
		}
	}
	if m.prompting {
		b.WriteString("\nattach to session: " + m.input + "▏\n")
	}
	b.WriteString("\n" + m.styles.help.Render(m.help()) + "\n")
	if m.width > 0 {
		return m.styles.frame.MaxWidth(m.width).Render(b.String())
	}
	return b.String()
}

// heading names the current screen.
func (m Model) heading() string {
	switch m.screen {
	case tasksScreen:
		return "tasks of " + m.project
	case detailScreen:
		return "task " + m.taskID
	case workersScreen:
		return "workers of " + m.project
	case workerScreen:
		return "worker " + printable(m.workerName)
	}
	return "dashboard"
}

// help lists the keys of the current screen.
func (m Model) help() string {
	if m.prompting {
		return "enter attach · esc cancel · Ctrl-] detaches once attached"
	}
	switch m.screen {
	case tasksScreen:
		return "↑/↓ select · enter show · w workers · esc back · a attach · r refresh · q quit"
	case workersScreen:
		return "↑/↓ select · enter show · esc back · a attach · r refresh · q quit"
	case detailScreen, workerScreen:
		return "esc back · a attach · r refresh · q quit"
	}
	return "↑/↓ select · enter tasks · w workers · a attach · r refresh · q quit"
}

// table renders rows aligned in columns, with a styled header and the
// selected row highlighted; selected is -1 for none.
func (m Model) table(header string, rows []string, selected int) string {
	var buffer bytes.Buffer
	writer := tabwriter.NewWriter(&buffer, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, header)
	for _, row := range rows {
		fmt.Fprintln(writer, row)
	}
	_ = writer.Flush()
	lines := strings.Split(strings.TrimSuffix(buffer.String(), "\n"), "\n")
	var b strings.Builder
	for i, line := range lines {
		switch {
		case i == 0:
			line = m.styles.header.Render(line)
		case i-1 == selected:
			line = m.styles.selected.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// marker prefixes a selectable row.
func marker(selected bool) string {
	if selected {
		return "> "
	}
	return "  "
}

// viewDashboard renders the capacity and the registered projects.
func (m Model) viewDashboard(b *strings.Builder) {
	if len(m.status.Capacity) > 0 {
		kinds := make([]string, 0, len(m.status.Capacity))
		for kind := range m.status.Capacity {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		rows := make([]string, 0, len(kinds))
		for _, kind := range kinds {
			usage := m.status.Capacity[kind]
			projects := make([]string, 0, len(usage.ByProject))
			for project, count := range usage.ByProject {
				projects = append(projects, fmt.Sprintf("%s: %d", project, count))
			}
			sort.Strings(projects)
			rows = append(rows, fmt.Sprintf("%s\t%d\t%d\t%s", kind, usage.Used, usage.Limit, strings.Join(projects, ", ")))
		}
		b.WriteString("\n" + m.table("CAPACITY\tUSED\tLIMIT\tBY PROJECT", rows, -1))
	}
	b.WriteString("\n")
	if len(m.projects) == 0 {
		b.WriteString("no projects\n")
		return
	}
	rows := make([]string, 0, len(m.projects))
	for i, project := range m.projects {
		rows = append(rows, fmt.Sprintf("%s%s\t%s\t%s", marker(i == m.cursor), project.ID, project.State, printable(project.Repository)))
	}
	b.WriteString(m.table("  PROJECT\tSTATE\tREPOSITORY", rows, m.cursor))
}

// viewTasks renders the tasks of the selected project.
func (m Model) viewTasks(b *strings.Builder) {
	b.WriteString("\n")
	if len(m.tasks) == 0 {
		b.WriteString("no tasks\n")
		return
	}
	rows := make([]string, 0, len(m.tasks))
	for i, t := range m.tasks {
		rows = append(rows, fmt.Sprintf("%s%s\t%s\t%s\t%s", marker(i == m.taskCursor), t.ID, t.State,
			t.CreatedAt.Local().Format(time.DateTime), summary(t.Description)))
	}
	b.WriteString(m.table("  ID\tSTATE\tCREATED\tDESCRIPTION", rows, m.taskCursor))
}

// viewDetail renders one task and its recorded events.
func (m Model) viewDetail(b *strings.Builder) {
	t := m.detail
	fields := []string{
		"project:\t" + t.ProjectID,
		"state:\t" + t.State,
	}
	if t.ResumeState != "" {
		fields = append(fields, "resume state:\t"+t.ResumeState, "blocked reason:\t"+printable(t.BlockedReason))
	}
	fields = append(fields, "branch:\t"+t.Branch, "base:\t"+t.BaseSHA)
	if t.HeadSHA != "" {
		fields = append(fields, "head:\t"+t.HeadSHA)
	}
	if m.driversKnown {
		drivers := make([]string, 0, len(m.drivers))
		for _, w := range m.drivers {
			drivers = append(drivers, printable(w.Name)+" ("+w.Assignment.Role+", "+w.State+")")
		}
		fields = append(fields, "worker:\t"+orDash(strings.Join(drivers, ", ")))
	}
	fields = append(fields,
		"config:\t"+t.ConfigID,
		fmt.Sprintf("fix cycles:\t%d/%d", t.FixCycles, t.MaxFixCycles),
		"created:\t"+t.CreatedAt.Local().Format(time.DateTime),
		"updated:\t"+t.UpdatedAt.Local().Format(time.DateTime),
	)
	var buffer bytes.Buffer
	writer := tabwriter.NewWriter(&buffer, 0, 0, 2, ' ', 0)
	for _, field := range fields {
		fmt.Fprintln(writer, field)
	}
	_ = writer.Flush()
	b.WriteString("\n" + buffer.String())
	for _, line := range strings.Split(strings.TrimRight(t.Description, "\n"), "\n") {
		b.WriteString("\n" + printable(line))
	}
	b.WriteString("\n")
	if len(t.Events) == 0 {
		return
	}
	rows := make([]string, 0, len(t.Events))
	for _, event := range t.Events {
		name := event.Event
		if event.Stale {
			name += " (stale)"
		}
		from := event.From
		if from == "" {
			from = "-"
		}
		rows = append(rows, fmt.Sprintf("%s\t%s\t%s\t%s\t%s",
			event.At.Local().Format(time.DateTime), name, from, event.To, printable(event.Reason)))
	}
	b.WriteString("\n" + m.table("AT\tEVENT\tFROM\tTO\tREASON", rows, -1))
}

// orDash returns text, or "-" when it is empty.
func orDash(text string) string {
	if text == "" {
		return "-"
	}
	return text
}

// viewWorkers renders the workers of the selected project.
func (m Model) viewWorkers(b *strings.Builder) {
	b.WriteString("\n")
	if len(m.workers) == 0 {
		b.WriteString("no workers\n")
		return
	}
	rows := make([]string, 0, len(m.workers))
	for i, w := range m.workers {
		taskID, role := "-", "-"
		if w.Assignment != nil {
			taskID, role = w.Assignment.TaskID, w.Assignment.Role
		}
		rows = append(rows, fmt.Sprintf("%s%s\t%s\t%s\t%s\t%s\t%s\t%s", marker(i == m.workerCursor),
			printable(w.Name), printable(w.Agent), w.State, taskID, role,
			w.UpdatedAt.Local().Format(time.DateTime), summary(orDash(w.Reason))))
	}
	b.WriteString(m.table("  NAME\tAGENT\tSTATE\tTASK\tROLE\tUPDATED\tREASON", rows, m.workerCursor))
}

// viewWorker renders one worker and its recent events.
func (m Model) viewWorker(b *strings.Builder) {
	w := m.worker
	fields := []string{
		"project:\t" + w.ProjectID,
		"agent:\t" + printable(w.Agent) + " (" + printable(w.AgentKind) + ")",
		"driver:\t" + printable(w.Driver),
		"state:\t" + w.State,
	}
	if w.Reason != "" {
		fields = append(fields, "reason:\t"+printable(w.Reason))
	}
	if w.Assignment != nil {
		fields = append(fields, "task:\t"+w.Assignment.TaskID, "role:\t"+w.Assignment.Role, "turn:\t"+w.Assignment.TurnID)
	} else {
		fields = append(fields, "assignment:\t-")
	}
	fields = append(fields,
		"repository:\t"+printable(w.Repository),
		"created:\t"+w.CreatedAt.Local().Format(time.DateTime),
		"updated:\t"+w.UpdatedAt.Local().Format(time.DateTime),
	)
	var buffer bytes.Buffer
	writer := tabwriter.NewWriter(&buffer, 0, 0, 2, ' ', 0)
	for _, field := range fields {
		fmt.Fprintln(writer, field)
	}
	_ = writer.Flush()
	b.WriteString("\n" + buffer.String())
	if len(w.Events) == 0 {
		return
	}
	rows := make([]string, 0, len(w.Events))
	for _, event := range w.Events {
		rows = append(rows, fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s",
			event.At.Local().Format(time.DateTime), event.Event, orDash(event.From), event.To,
			orDash(event.TaskID), printable(event.Reason)))
	}
	b.WriteString("\n" + m.table("AT\tEVENT\tFROM\tTO\tTASK\tREASON", rows, -1))
}

// summary returns the first line of a description, shortened for a table.
func summary(description string) string {
	line, _, _ := strings.Cut(description, "\n")
	line = printable(line)
	if runes := []rune(line); len(runes) > 60 {
		return string(runes[:59]) + "…"
	}
	return line
}

// printable replaces the control characters of a text written by a user
// or read from disk, so it cannot drive the terminal.
func printable(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, text)
}
