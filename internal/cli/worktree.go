// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/goabonga/maestro/internal/worktree"
)

// project resolves the registered project containing path.
func project(path string) (worktree.Project, error) {
	store, err := worktree.DefaultStore()
	if err != nil {
		return worktree.Project{}, err
	}
	found, ok, err := store.Find(path)
	if err != nil {
		return worktree.Project{}, err
	}
	if !ok {
		return worktree.Project{}, fmt.Errorf("the repository is not registered; run maestro init first")
	}
	return found, nil
}

// worktreeCommand dispatches the read-only worktree subcommands.
func worktreeCommand(args []string, output io.Writer) error {
	if len(args) == 0 || args[0] != "list" {
		return errors.New("usage: maestro worktree list [--path <repository>]")
	}
	flags := flag.NewFlagSet("maestro worktree list", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("path", ".", "path inside the registered repository")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument: %s", flags.Arg(0))
	}
	found, err := project(*path)
	if err != nil {
		return err
	}
	statuses, err := found.TaskWorktreeStatuses()
	if err != nil {
		return err
	}
	if len(statuses) == 0 {
		_, err = fmt.Fprintln(output, "no task worktrees")
		return err
	}
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "WORKER\tTASK\tBRANCH\tHEAD\tSTATE\tPATH")
	for _, status := range statuses {
		state := "clean"
		if status.Dirty {
			state = "dirty"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			status.Worker, status.TaskID, status.Branch, status.Head[:12], state, status.Path)
	}
	return table.Flush()
}

// diff prints the pending changes of one task worktree of a worker.
func diff(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("maestro diff", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("path", ".", "path inside the registered repository")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// Accept flags after the positional arguments too.
	var positionals []string
	remaining := flags.Args()
	for len(remaining) > 0 && !strings.HasPrefix(remaining[0], "-") {
		positionals = append(positionals, remaining[0])
		remaining = remaining[1:]
	}
	if err := flags.Parse(remaining); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(positionals) < 1 || len(positionals) > 2 || flags.NArg() != 0 {
		return errors.New("usage: maestro diff <worker> [task] [--path <repository>]")
	}
	found, err := project(*path)
	if err != nil {
		return err
	}
	worker, ok, err := found.Worker(positionals[0])
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("unknown worker: %s", positionals[0])
	}
	var taskID string
	if len(positionals) == 2 {
		taskID = positionals[1]
	}
	if taskID == "" {
		taskID, err = soleTask(found, worker)
		if err != nil {
			return err
		}
	}
	changes, err := found.Diff(worker, taskID)
	if err != nil {
		return err
	}
	if changes == "" {
		return nil
	}
	_, err = fmt.Fprintln(output, changes)
	return err
}

// soleTask returns the only task of a worker, or an error naming the
// candidates.
func soleTask(project worktree.Project, worker worktree.Worker) (string, error) {
	statuses, err := project.TaskWorktreeStatuses()
	if err != nil {
		return "", err
	}
	var tasks []string
	for _, status := range statuses {
		if status.Worker == worker.Name {
			tasks = append(tasks, status.TaskID)
		}
	}
	switch len(tasks) {
	case 0:
		return "", fmt.Errorf("worker %s has no task worktree", worker.Name)
	case 1:
		return tasks[0], nil
	default:
		return "", fmt.Errorf("worker %s has several task worktrees, pick one: %s",
			worker.Name, strings.Join(tasks, ", "))
	}
}
