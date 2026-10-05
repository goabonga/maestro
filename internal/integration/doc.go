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
//
// The durable proof of an integration is the reference
// refs/maestro/operations/<id>/result in the project's canonical
// repository. It is created atomically with the null object id as its
// expected old value, so it is never overwritten, and before the result
// is recorded in the journal. A result is valid only if its commit has
// the integration base as its only parent, the recorded candidate tree
// and the frozen commit metadata; the presence of the source commits in
// history is never taken as proof of an integration.
package integration
