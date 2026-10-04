// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/goabonga/maestro/internal/agent"
	"github.com/goabonga/maestro/internal/launcher"
)

// agentCommand dispatches the agent subcommands.
func agentCommand(ctx context.Context, args []string, output io.Writer) error {
	if len(args) != 1 || args[0] != "doctor" {
		return errors.New("usage: maestro agent doctor")
	}
	doctor := agent.Doctor{
		Registry: agent.Builtin(),
		Sandbox: func() error {
			_, err := launcher.New()
			return err
		},
	}
	checks := doctor.Run(ctx)
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "CHECK\tSTATUS\tDETAIL")
	failed := 0
	for _, check := range checks {
		status := "ok"
		if !check.OK {
			status = "refused"
			failed++
		}
		fmt.Fprintf(table, "%s\t%s\t%s\n", check.Name, status, check.Detail)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d checks refused", failed, len(checks))
	}
	return nil
}
