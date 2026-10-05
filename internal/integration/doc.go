// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package integration journals the operations that change the
// integration branch. Its explicit transition table is the single
// authority on how an INTEGRATE, SYNC or PUBLISH operation moves from
// PREPARED to COMMITTED, or through FAILED to ROLLED_BACK; each
// transition is stored with its event in one compare-and-set
// transaction, and no transition turns an unfinished or failed
// operation into a success. An integration freezes its Git inputs and
// commit metadata when it is prepared, before any Git mutation.
package integration
