// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package session runs one confined process on a persistent PTY the
// daemon owns: bounded output, resize propagation, explicit stop with
// group termination, and state reconciliation when the process exits
// on its own.
package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/goabonga/maestro/internal/launcher"
)

// Phases of a session's lifecycle.
const (
	// Running means the process group is alive on its PTY.
	Running = "running"
	// Exited means the process ended on its own; the exit code is
	// reconciled into the state.
	Exited = "exited"
	// Stopped means the daemon terminated the group explicitly.
	Stopped = "stopped"
)

// DefaultBuffer bounds the kept output of a session.
const DefaultBuffer = 1 << 20

// Config describes one session.
type Config struct {
	// Spec is the confined command to run on the PTY.
	Spec launcher.Spec
	// BufferBytes bounds the kept output; 0 means DefaultBuffer.
	BufferBytes int
	// Rows and Cols give the initial terminal size; 0 means 24x80.
	Rows uint16
	Cols uint16
}

// State is the reconciled view of a session.
type State struct {
	Phase    string
	ExitCode int
	// DroppedOutput reports bytes seen beyond the kept buffer.
	TotalOutput uint64
}

// Session is one live or finished PTY session.
type Session struct {
	mu           sync.Mutex
	master       *os.File
	pid          int
	output       *ring
	phase        string
	exit         int
	done         chan struct{}
	inputBlocked bool
	subscribers  map[chan []byte]func()
}

// Start launches the confined command on a fresh PTY.
func Start(l *launcher.Launcher, config Config) (*Session, error) {
	readinessDir, err := os.MkdirTemp("", "maestro-profile-ready-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(readinessDir) }()
	readinessPath := filepath.Join(readinessDir, "ready")
	if err := syscall.Mkfifo(readinessPath, 0o600); err != nil {
		return nil, err
	}
	// O_RDWR avoids blocking open before the confined writer exists. Nonblock
	// makes the FIFO pollable by Go, so a failed mount has a bounded deadline.
	readinessRoot, err := os.OpenRoot(readinessDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = readinessRoot.Close() }()
	readyFile, err := readinessRoot.OpenFile("ready", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = readyFile.Close() }()
	command, err := l.CommandReady(config.Spec, readinessPath)
	if err != nil {
		return nil, err
	}
	rows, cols := config.Rows, config.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	master, err := pty.StartWithSize(command, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, err
	}
	// PTY files from creack/pty start in blocking mode. Reopen a CLOEXEC
	// duplicate as pollable so Close can interrupt a stalled writer or reader.
	master, err = pollableMaster(master)
	if err != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		return nil, err
	}
	buffer := config.BufferBytes
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	session := &Session{
		master:      master,
		pid:         command.Process.Pid,
		output:      newRing(buffer),
		phase:       Running,
		done:        make(chan struct{}),
		subscribers: map[chan []byte]func(){},
	}
	readDone := make(chan struct{})
	go func() {
		// The PTY master returns EIO once the group is gone; every
		// byte before that lands in the bounded buffer and reaches
		// the live subscribers, which can never block this read.
		chunk := make([]byte, 32<<10)
		for {
			n, err := master.Read(chunk)
			if n > 0 {
				_, _ = session.output.Write(chunk[:n])
				session.broadcast(chunk[:n])
			}
			if err != nil {
				// A reaped group closes every slave. Drain through EIO
				// before closing the master, preserving the final output.
				_ = master.Close()
				close(readDone)
				// Subscribers learn of the end only once the state is
				// reconciled, so they never see a dead session as
				// running.
				<-session.done
				session.closeSubscribers()
				return
			}
		}
	}()
	go func() {
		err := command.Wait()
		<-readDone
		session.mu.Lock()
		if session.phase == Running {
			session.phase = Exited
		}
		session.exit = exitCode(err)
		session.mu.Unlock()
		close(session.done)
	}()
	// Bubblewrap's child PID report precedes mount setup. Only this fixed
	// wrapper's private FIFO acknowledgement, emitted before exec of the agent,
	// confirms that mounts and limits are installed. PTY output is not proof.
	if err := readyFile.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		_ = session.Stop(100 * time.Millisecond)
		return nil, fmt.Errorf("bound sandbox confirmation: %w", err)
	}
	byteRead := make([]byte, 1)
	if n, err := readyFile.Read(byteRead); err != nil || n != 1 || byteRead[0] != 'R' {
		stopErr := session.Stop(100 * time.Millisecond)
		return nil, fmt.Errorf("confirm sandbox profile: %w", errors.Join(err, stopErr, errors.New("missing mount acknowledgement")))
	}
	return session, nil
}

// Subscribe returns a bounded live-output channel and its cancel. The
// channel is closed when the session's output ends — or, for a slow
// client, when its queue overflows: the master read never waits on a
// subscriber.
func (s *Session) Subscribe(queue int) (<-chan []byte, func()) {
	if queue <= 0 {
		queue = 64
	}
	channel := make(chan []byte, queue)
	s.mu.Lock()
	closed := s.subscribers == nil
	var cancel func()
	if !closed {
		cancel = func() {
			s.mu.Lock()
			if s.subscribers != nil {
				if _, ok := s.subscribers[channel]; ok {
					delete(s.subscribers, channel)
					close(channel)
				}
			}
			s.mu.Unlock()
		}
		s.subscribers[channel] = cancel
	}
	s.mu.Unlock()
	if closed {
		close(channel)
		return channel, func() {}
	}
	return channel, cancel
}

// broadcast fans one output chunk out without ever blocking: a full
// queue disconnects its subscriber instead.
func (s *Session) broadcast(chunk []byte) {
	s.mu.Lock()
	var dropped []func()
	for channel := range s.subscribers {
		copied := make([]byte, len(chunk))
		copy(copied, chunk)
		select {
		case channel <- copied:
		default:
			dropped = append(dropped, s.subscribers[channel])
		}
	}
	s.mu.Unlock()
	for _, cancel := range dropped {
		cancel()
	}
}

// closeSubscribers ends every live subscription and marks the session
// output finished: later subscriptions come back already closed.
func (s *Session) closeSubscribers() {
	s.mu.Lock()
	for channel := range s.subscribers {
		close(channel)
	}
	s.subscribers = nil
	s.mu.Unlock()
}

// Write sends input to the session's terminal.
func (s *Session) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.inputBlocked {
		s.mu.Unlock()
		return 0, errors.New("session input is fenced at a permissions boundary")
	}
	if s.phase != Running {
		phase := s.phase
		s.mu.Unlock()
		return 0, fmt.Errorf("the session is %s", phase)
	}
	master := s.master
	s.mu.Unlock()
	// Admission is serialized with BlockInput, not the potentially blocking
	// I/O. Stop must be able to close the PTY even when the agent stops reading.
	return master.Write(p)
}

// BlockInput permanently closes admission to this PTY. It also fences
// clients retaining a session pointer across a permissions boundary.
func (s *Session) BlockInput() {
	s.mu.Lock()
	s.inputBlocked = true
	s.mu.Unlock()
}

// Resize propagates a new terminal size to the PTY.
func (s *Session) Resize(rows, cols uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != Running {
		return fmt.Errorf("the session is %s", s.phase)
	}
	connection, err := s.master.SyscallConn()
	if err != nil {
		return err
	}
	var resizeErr error
	err = connection.Control(func(fd uintptr) {
		resizeErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	})
	return errors.Join(err, resizeErr)
}

// Output returns the kept output and the total bytes ever produced.
func (s *Session) Output() ([]byte, uint64) {
	return s.output.Snapshot()
}

// State returns the reconciled state of the session.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, total := s.output.Snapshot()
	return State{Phase: s.phase, ExitCode: s.exit, TotalOutput: total}
}

// Wait blocks until the process group is gone.
func (s *Session) Wait() {
	<-s.done
}

// Stop terminates the group: SIGTERM to the process group, SIGKILL
// after the grace period, and returns once the leader is reaped — the
// group's PID namespace dies with it, so no descendant survives.
func (s *Session) Stop(grace time.Duration) error {
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return nil
	default:
	}
	s.inputBlocked = true
	s.phase = Stopped
	pid := s.pid
	s.mu.Unlock()

	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-s.done:
		return nil
	case <-time.After(grace):
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	select {
	case <-s.done:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("the session group did not die after SIGKILL")
	}
}

// pollableMaster transfers ownership to a descriptor registered with Go's
// poller. Fd must not be called on the resulting file: it restores blocking
// mode; Resize uses SyscallConn instead.
func pollableMaster(master *os.File) (*os.File, error) {
	duplicate, err := unix.FcntlInt(master.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	_ = master.Close()
	if err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(duplicate, true); err != nil {
		_ = unix.Close(duplicate)
		return nil, err
	}
	return os.NewFile(uintptr(duplicate), "maestro-pty"), nil
}

// exitCode extracts a process exit code from a wait error.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}
