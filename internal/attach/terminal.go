// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package attach

import (
	"golang.org/x/sys/unix"
)

// rawTerminal switches a terminal to raw mode and returns the function
// that restores its previous settings. A non-terminal is left alone and
// gets a no-op restore.
func rawTerminal(fd int) (func() error, error) {
	previous, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return func() error { return nil }, nil
	}
	raw := *previous
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	return func() error { return unix.IoctlSetTermios(fd, unix.TCSETS, previous) }, nil
}

// terminalSize returns the size of a terminal, or false for a
// non-terminal.
func terminalSize(fd int) (uint16, uint16, bool) {
	size, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || size.Row == 0 || size.Col == 0 {
		return 0, 0, false
	}
	return size.Row, size.Col, true
}
