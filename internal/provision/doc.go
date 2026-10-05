// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package provision prepares a task worktree before an agent starts in
// it: it materializes the instruction files frozen in a configuration
// snapshot at the native locations the agent reads, without ever
// replacing a file the repository tracks, and keeps them out of Git
// through a worktree-scoped excludes file stored outside the worktree
// sources.
package provision
