// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

// command is one subcommand of maestro: Run dispatches through this
// table and the help is generated from it, so every command is listed.
type command struct {
	name    string
	summary string
	usage   []string
	details string
	run     func(ctx context.Context, args []string, output io.Writer) error
}

// commands returns the subcommands in the order the help lists them.
func commands() []command {
	return []command{
		{name: "init", summary: "register a repository and import it into a private clone",
			usage:   []string{"maestro init [path]"},
			details: "Registers the repository containing path (the current directory by default).\nRunning it again on a registered repository is harmless.",
			run:     func(_ context.Context, args []string, out io.Writer) error { return initProject(args, out) }},
		{name: "project", summary: "list registered projects or follow a moved repository",
			usage: []string{"maestro project list", "maestro project relocate <id> <path>"},
			run:   func(_ context.Context, args []string, out io.Writer) error { return projectCommand(args, out) }},
		{name: "daemon", summary: "start, stop or check the maestro-svc daemon",
			usage:   []string{"maestro daemon start [--socket <path>] [--binary <path>]", "maestro daemon stop [--socket <path>]", "maestro daemon status [--socket <path>]"},
			details: "start looks for maestro-svc next to maestro, then on PATH, unless --binary is given.",
			run:     daemonCommand},
		{name: "status", summary: "show the daemon and its capacity",
			usage: []string{"maestro status [--socket <path>]"},
			run:   status},
		{name: "task", summary: "create, inspect, cancel, resume or reconfigure tasks",
			usage: []string{"maestro task new \"<description>\"", "maestro task show <id>", "maestro task list",
				"maestro task cancel <id>", "maestro task resume <id>", "maestro task config update <id>"},
			details: "Every task command accepts --project <id> (default: the project of the\ncurrent repository) and --socket <path>.",
			run:     taskCommand},
		{name: "worker", summary: "list the workers of a project or show one",
			usage:   []string{"maestro worker list", "maestro worker show <name>"},
			details: "Every worker command accepts --project <id> (default: the project of the\ncurrent repository) and --socket <path>.",
			run:     workerCommand},
		{name: "sync", summary: "import the commit a branch of your repository points to",
			usage:   []string{"maestro sync --from <branch> [--project <id>] [--socket <path>]"},
			details: "The commit must descend from the project's integration head; the configured\ntests run on it before the integration advances.",
			run:     syncCommand},
		{name: "publish", summary: "fast-forward maestro/integration in your repository",
			usage:   []string{"maestro publish [--project <id>] [--socket <path>]"},
			details: "Only refs/heads/maestro/integration is updated; your current branch and\nworktree are never touched.",
			run:     publish},
		{name: "tui", summary: "open the live terminal dashboard",
			usage: []string{"maestro tui [--project <id>] [--interval <duration>] [--socket <path>]"},
			run: func(ctx context.Context, args []string, out io.Writer) error {
				return tuiCommand(ctx, args, os.Stdin, out)
			}},
		{name: "worktree", summary: "list the task worktrees of a project",
			usage: []string{"maestro worktree list [--path <repository>]"},
			run:   func(_ context.Context, args []string, out io.Writer) error { return worktreeCommand(args, out) }},
		{name: "diff", summary: "show the pending changes of a worker's task worktree",
			usage: []string{"maestro diff <worker> [task] [--path <repository>]"},
			run:   func(_ context.Context, args []string, out io.Writer) error { return diff(args, out) }},
		{name: "attach", summary: "attach the terminal to a session (Ctrl-] detaches)",
			usage: []string{"maestro attach <session> [--socket <path>]"},
			run:   attachCommand},
		{name: "agent", summary: "check the sandbox and the installed agent versions",
			usage: []string{"maestro agent doctor"},
			run:   agentCommand},
		{name: "backup", summary: "snapshot the data directory (daemon stopped)",
			usage: []string{"maestro backup <destination>"},
			run:   func(_ context.Context, args []string, out io.Writer) error { return backup(args, out) }},
		{name: "restore", summary: "restore a backup into a new data directory",
			usage: []string{"maestro restore <backup> <new-data-directory>"},
			run:   func(_ context.Context, args []string, out io.Writer) error { return restore(args, out) }},
		{name: "gc", summary: "preview or remove superseded data",
			usage: []string{"maestro gc --dry-run", "maestro gc --apply"},
			run:   func(_ context.Context, args []string, out io.Writer) error { return gc(args, out) }},
	}
}

// lookup finds a subcommand by name.
func lookup(name string) (command, bool) {
	for _, candidate := range commands() {
		if candidate.name == name {
			return candidate, true
		}
	}
	return command{}, false
}

// printOverview lists every subcommand.
func printOverview(output io.Writer) error {
	var text strings.Builder
	text.WriteString("maestro keeps the work on a repository in private Git clones and worktrees, through the maestro-svc daemon.\n\n")
	text.WriteString("Usage:\n  maestro <command> [arguments]\n  maestro help <command>\n  maestro --version\n\nCommands:\n")
	table := tabwriter.NewWriter(&text, 0, 0, 2, ' ', 0)
	for _, entry := range commands() {
		fmt.Fprintf(table, "  %s\t%s\n", entry.name, entry.summary)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	text.WriteString("\nRun 'maestro help <command>' for the arguments and flags of a command.\n")
	_, err := io.WriteString(output, text.String())
	return err
}

// printCommand describes one subcommand.
func printCommand(output io.Writer, entry command) error {
	var text strings.Builder
	fmt.Fprintf(&text, "maestro %s: %s\n\nUsage:\n", entry.name, entry.summary)
	for _, line := range entry.usage {
		fmt.Fprintf(&text, "  %s\n", line)
	}
	if entry.details != "" {
		fmt.Fprintf(&text, "\n%s\n", entry.details)
	}
	_, err := io.WriteString(output, text.String())
	return err
}

// help answers "maestro help [command]".
func help(args []string, output io.Writer) error {
	switch len(args) {
	case 0:
		return printOverview(output)
	case 1:
		entry, ok := lookup(args[0])
		if !ok {
			return fmt.Errorf("unknown command: %s (run 'maestro help')", args[0])
		}
		return printCommand(output, entry)
	default:
		return fmt.Errorf("usage: maestro help [command]")
	}
}
