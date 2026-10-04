// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/worktree"
)

// projectCommand dispatches the project registry subcommands.
func projectCommand(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: maestro project list | maestro project relocate <id> <path>")
	}
	switch args[0] {
	case "list":
		return projectList(args[1:], output)
	case "relocate":
		return projectRelocate(args[1:], output)
	default:
		return fmt.Errorf("unknown project command: %s", args[0])
	}
}

// projectList prints every registered project with its path and state.
func projectList(args []string, output io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("unexpected argument: %s", args[0])
	}
	store, err := worktree.DefaultStore()
	if err != nil {
		return err
	}
	projects, err := store.Projects()
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		_, err = fmt.Fprintln(output, "no projects")
		return err
	}
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tREPOSITORY\tSTATE")
	for _, project := range projects {
		fmt.Fprintf(table, "%s\t%s\t%s\n", project.ID, project.UserRepository, project.State())
	}
	return table.Flush()
}

// projectRelocate re-points a project at the new path of its moved
// repository, under the user lock.
func projectRelocate(args []string, output io.Writer) error {
	if len(args) != 2 {
		return errors.New("usage: maestro project relocate <id> <path>")
	}
	store, err := worktree.DefaultStore()
	if err != nil {
		return err
	}
	lock, err := state.Acquire(filepath.Join(store.Base, "lock"))
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	project, err := store.Relocate(args[0], args[1])
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "project %s now at %s\n", project.ID, project.UserRepository)
	return err
}
