// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/worktree"
)

// backup snapshots the data directory into a new destination.
func backup(args []string, output io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: maestro backup <destination>")
	}
	store, err := worktree.DefaultStore()
	if err != nil {
		return err
	}
	manifest, err := state.BackupData(store.Base, args[0])
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "backup written to %s (%d files, schema version %d)\n",
		args[0], len(manifest.Files), manifest.SchemaVersion)
	return err
}

// restore verifies a backup and restores it into a new directory.
func restore(args []string, output io.Writer) error {
	if len(args) != 2 {
		return errors.New("usage: maestro restore <backup> <new-data-directory>")
	}
	manifest, err := state.RestoreData(args[0], args[1])
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output,
		"restored %d files into %s (schema version %d); point MAESTRO_DATA_HOME at it to use it\n",
		len(manifest.Files), args[1], manifest.SchemaVersion)
	return err
}

// gc previews or applies the garbage collection plan.
func gc(args []string, output io.Writer) error {
	if len(args) != 1 || (args[0] != "--dry-run" && args[0] != "--apply") {
		return errors.New("usage: maestro gc --dry-run | --apply")
	}
	store, err := worktree.DefaultStore()
	if err != nil {
		return err
	}
	plan, err := state.PlanGC(store.Base)
	if err != nil {
		return err
	}
	if len(plan.Candidates) == 0 {
		_, err = fmt.Fprintln(output, "nothing to collect")
		return err
	}
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "PATH\tSIZE\tREASON")
	for _, candidate := range plan.Candidates {
		fmt.Fprintf(table, "%s\t%d\t%s\n", candidate.Path, candidate.Size, candidate.Reason)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if args[0] == "--dry-run" {
		_, err = fmt.Fprintln(output, "dry run: nothing was removed")
		return err
	}
	journal, err := state.ApplyGC(store.Base, plan)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "removed %d objects; journal at %s\n", len(plan.Candidates), journal)
	return err
}
