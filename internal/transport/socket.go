// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// maxSocketPath is the sun_path capacity of sockaddr_un, including its NUL.
const maxSocketPath = 108

// DefaultSocket returns the per-user path of the daemon's Unix socket. It
// lives in $XDG_RUNTIME_DIR when available, otherwise in a per-user directory
// under the system temporary directory.
func DefaultSocket() string {
	if runtime := os.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
		return filepath.Join(runtime, "maestro", "svc.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("maestro-%d", os.Getuid()), "svc.sock")
}

// Listen binds a Unix socket readable and writable only by its owner. It
// removes a stale socket left by a crashed daemon, and refuses to replace a
// live daemon or a regular file.
func Listen(path string) (net.Listener, error) {
	if len(path) >= maxSocketPath {
		return nil, fmt.Errorf("socket path exceeds %d bytes: %s", maxSocketPath-1, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if conn, err := net.Dial("unix", path); err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("a daemon is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

// Client returns an HTTP client that sends every request over the Unix socket
// at path. The host in request URLs is ignored.
func Client(path string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", path)
			},
		},
	}
}
