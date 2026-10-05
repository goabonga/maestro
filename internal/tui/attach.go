// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package tui

import (
	"context"
	"errors"
	"io"
	"os"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goabonga/maestro/internal/attach"
)

// maxSessionID bounds the session id typed in the attach prompt.
const maxSessionID = 128

// ErrNoTerminal reports an attach requested while the dashboard does not
// read a terminal.
var ErrNoTerminal = errors.New("attach needs a terminal")

// attachedMsg reports the end of an attach.
type attachedMsg struct {
	session string
	err     error
}

// attachExec runs one attach on the program's terminal while Bubble Tea
// is suspended. It implements tea.ExecCommand.
type attachExec struct {
	socket  string
	session string
	in      io.Reader
	out     io.Writer
}

// SetStdin receives the program's input.
func (a *attachExec) SetStdin(r io.Reader) { a.in = r }

// SetStdout receives the program's output.
func (a *attachExec) SetStdout(w io.Writer) { a.out = w }

// SetStderr ignores the error stream: the attach reports through Run.
func (a *attachExec) SetStderr(io.Writer) {}

// Run attaches the terminal to the session until Ctrl-] or the end of
// the session; the attach restores the terminal before returning.
func (a *attachExec) Run() error {
	in, ok := a.in.(*os.File)
	if !ok {
		return ErrNoTerminal
	}
	winch, stop := attach.Resizes()
	defer stop()
	return attach.Run(context.Background(), a.socket, a.session,
		attach.Terminal{In: in, Out: a.out, Winch: winch})
}

// attachTo suspends the program and attaches its terminal to a session;
// the program resumes with an attachedMsg once the attach returns.
func (m Model) attachTo(session string) tea.Cmd {
	return tea.Exec(&attachExec{socket: m.client.socket, session: session}, func(err error) tea.Msg {
		return attachedMsg{session: session, err: err}
	})
}

// promptKey edits the session id of the attach prompt, starts the attach
// on enter and leaves the prompt on esc.
func (m Model) promptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyEsc:
		m.prompting, m.input = false, ""
	case tea.KeyBackspace:
		if runes := []rune(m.input); len(runes) > 0 {
			m.input = string(runes[:len(runes)-1])
		}
	case tea.KeyEnter:
		session := m.input
		m.prompting, m.input = false, ""
		if session == "" {
			return m, nil
		}
		m.notice, m.noticeErr = "", nil
		return m, m.attachTo(session)
	case tea.KeyRunes:
		for _, r := range msg.Runes {
			if unicode.IsPrint(r) && !unicode.IsSpace(r) && len(m.input) < maxSessionID {
				m.input += string(r)
			}
		}
	}
	return m, nil
}

// attached records the outcome of an attach and refreshes the screen.
func (m Model) attached(msg attachedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.notice, m.noticeErr = "", msg.err
	} else {
		m.notice, m.noticeErr = "detached from "+msg.session, nil
	}
	return m, m.refresh()
}
