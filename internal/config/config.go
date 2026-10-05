// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

// Package config loads a project's configuration in layers — Maestro's
// defaults, the versioned .maestro.toml, then the unversioned
// .maestro.local.toml — refuses anything that looks like a secret, and
// freezes the result with the project's instruction files into an
// immutable snapshot identified by its config_id.
package config

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ErrInvalid wraps every configuration error.
var ErrInvalid = errors.New("invalid configuration")

// invalid builds a configuration error.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// Duration is a TOML string such as "20m".
type Duration struct {
	time.Duration
}

// UnmarshalText parses a Go duration.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

// MarshalText renders the duration back.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// Budgets are the project-wide defaults of a task's limits.
type Budgets struct {
	MaxTurnsPerTask  int      `toml:"max_turns_per_task" json:"max_turns_per_task"`
	TurnTimeout      Duration `toml:"turn_timeout" json:"turn_timeout"`
	InputWaitTimeout Duration `toml:"input_wait_timeout" json:"input_wait_timeout"`
	TaskTimeout      Duration `toml:"task_timeout" json:"task_timeout"`
}

// Agent configures one named agent. Limits set here apply on top of the
// project's budgets; they never replace the per-task total.
type Agent struct {
	Driver  string `toml:"driver" json:"driver,omitempty"`
	BaseURL string `toml:"base_url" json:"base_url,omitempty"`
	Model   string `toml:"model" json:"model,omitempty"`
	// APIKeyEnv names the environment variable holding the key; the
	// key itself never appears in configuration. Empty means none.
	APIKeyEnv       string    `toml:"api_key_env" json:"api_key_env,omitempty"`
	MaxTurnsPerTask int       `toml:"max_turns_per_task" json:"max_turns_per_task,omitempty"`
	TurnTimeout     *Duration `toml:"turn_timeout" json:"turn_timeout,omitempty"`
}

// MCP declares one MCP server, shared or reserved to an agent or role.
type MCP struct {
	Command string   `toml:"command" json:"command"`
	Args    []string `toml:"args" json:"args,omitempty"`
	// Scope is "shared", "agent:<name>" or "role:<name>".
	Scope string `toml:"scope" json:"scope"`
}

// Config is the effective configuration of a project.
type Config struct {
	Budgets Budgets          `toml:"budgets" json:"budgets"`
	Agents  map[string]Agent `toml:"agents" json:"agents,omitempty"`
	MCP     map[string]MCP   `toml:"mcp" json:"mcp,omitempty"`
}

// Defaults are Maestro's own values, the weakest layer.
func Defaults() Config {
	return Config{
		Budgets: Budgets{
			MaxTurnsPerTask:  30,
			TurnTimeout:      Duration{20 * time.Minute},
			InputWaitTimeout: Duration{5 * time.Minute},
			TaskTimeout:      Duration{2 * time.Hour},
		},
	}
}

var (
	name       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	mcpScope   = regexp.MustCompile(`^(shared|agent:[a-z0-9][a-z0-9-]{0,62}|role:[a-z0-9][a-z0-9-]{0,62})$`)
	secretKeys = regexp.MustCompile(`(?i)(^|_)(api_?key|token|secret|password|passwd|credential)s?$`)
)

// Validate checks the effective configuration.
func (c Config) Validate() error {
	budgets := c.Budgets
	if budgets.MaxTurnsPerTask < 1 {
		return invalid("budgets.max_turns_per_task must be at least 1")
	}
	for key, value := range map[string]Duration{
		"turn_timeout": budgets.TurnTimeout, "input_wait_timeout": budgets.InputWaitTimeout, "task_timeout": budgets.TaskTimeout,
	} {
		if value.Duration <= 0 {
			return invalid("budgets.%s must be positive", key)
		}
	}
	for agentName, agent := range c.Agents {
		if !name.MatchString(agentName) {
			return invalid("agent name %q must be lowercase letters, digits and dashes", agentName)
		}
		if agent.APIKeyEnv != "" && !envName.MatchString(agent.APIKeyEnv) {
			return invalid("agents.%s.api_key_env %q is not an environment variable name", agentName, agent.APIKeyEnv)
		}
		if agent.MaxTurnsPerTask < 0 {
			return invalid("agents.%s.max_turns_per_task cannot be negative", agentName)
		}
		if agent.TurnTimeout != nil && agent.TurnTimeout.Duration <= 0 {
			return invalid("agents.%s.turn_timeout must be positive", agentName)
		}
	}
	for serverName, server := range c.MCP {
		if !name.MatchString(serverName) {
			return invalid("MCP server name %q must be lowercase letters, digits and dashes", serverName)
		}
		if server.Command == "" {
			return invalid("mcp.%s.command is required", serverName)
		}
		if !mcpScope.MatchString(server.Scope) {
			return invalid(`mcp.%s.scope %q must be "shared", "agent:<name>" or "role:<name>"`, serverName, server.Scope)
		}
	}
	return nil
}
