// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package provision

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/goabonga/maestro/internal/config"
)

// ClaudeMCPFile is the project-relative path of Claude's MCP file.
const ClaudeMCPFile = ".mcp.json"

var (
	// ErrUnknownKind reports an agent kind with no MCP translation.
	ErrUnknownKind = errors.New("unknown agent kind")
	// ErrInvalidMCP reports a declaration that cannot be translated.
	ErrInvalidMCP = errors.New("invalid MCP declaration")
)

// bareKey matches a TOML key that needs no quoting.
var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// MCPConfig is the native MCP configuration of one agent.
type MCPConfig struct {
	// Kind is the agent kind the configuration was rendered for.
	Kind string
	// File is the project-relative file to write for KindClaudeCode; it is
	// empty for KindCodex, whose Content is a fragment of Codex's own
	// configuration.
	File string
	// Content is the rendered configuration, byte-stable for the same
	// input.
	Content []byte
	// Servers lists the translated server names, sorted.
	Servers []string
}

// Applies reports whether a declaration's scope covers an agent playing
// a role. An empty role matches no "role:" scope.
func Applies(server config.MCP, agent, role string) (bool, error) {
	switch scope := server.Scope; {
	case scope == "shared":
		return true, nil
	case strings.HasPrefix(scope, "agent:") && len(scope) > len("agent:"):
		return scope[len("agent:"):] == agent, nil
	case strings.HasPrefix(scope, "role:") && len(scope) > len("role:"):
		return role != "" && scope[len("role:"):] == role, nil
	default:
		return false, fmt.Errorf("%w: scope %q", ErrInvalidMCP, scope)
	}
}

// Applicable returns the sorted names of the servers that apply to an
// agent playing a role.
func Applicable(servers map[string]config.MCP, agent, role string) ([]string, error) {
	names := make([]string, 0, len(servers))
	for name, server := range servers {
		ok, err := Applies(server, agent, role)
		if err != nil {
			return nil, fmt.Errorf("mcp.%s: %w", name, err)
		}
		if ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// TranslateMCP renders the declarations that apply to the named agent of
// the given kind playing role into that kind's native configuration.
// Unknown kinds are refused with ErrUnknownKind.
func TranslateMCP(servers map[string]config.MCP, agent, kind, role string) (MCPConfig, error) {
	if kind != KindClaudeCode && kind != KindCodex {
		return MCPConfig{}, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	names, err := Applicable(servers, agent, role)
	if err != nil {
		return MCPConfig{}, err
	}
	for _, name := range names {
		if err := checkServer(name, servers[name]); err != nil {
			return MCPConfig{}, err
		}
	}
	result := MCPConfig{Kind: kind, Servers: names}
	if kind == KindClaudeCode {
		result.File = ClaudeMCPFile
		result.Content, err = renderClaude(servers, names)
	} else {
		result.Content = renderCodex(servers, names)
	}
	if err != nil {
		return MCPConfig{}, err
	}
	return result, nil
}

// checkServer refuses what neither format can carry faithfully.
func checkServer(name string, server config.MCP) error {
	if name == "" {
		return fmt.Errorf("%w: empty server name", ErrInvalidMCP)
	}
	if server.Command == "" {
		return fmt.Errorf("%w: mcp.%s.command is required", ErrInvalidMCP, name)
	}
	for _, value := range append([]string{name, server.Command}, server.Args...) {
		if !utf8.ValidString(value) {
			return fmt.Errorf("%w: mcp.%s holds invalid UTF-8", ErrInvalidMCP, name)
		}
	}
	return nil
}

// claudeServer is one entry of Claude's mcpServers object.
type claudeServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// renderClaude renders {"mcpServers": {...}}; encoding/json sorts the
// map keys, which keeps the output byte-stable.
func renderClaude(servers map[string]config.MCP, names []string) ([]byte, error) {
	entries := make(map[string]claudeServer, len(names))
	for _, name := range names {
		args := servers[name].Args
		if args == nil {
			args = []string{}
		}
		entries[name] = claudeServer{Command: servers[name].Command, Args: args}
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(struct {
		MCPServers map[string]claudeServer `json:"mcpServers"`
	}{entries}); err != nil {
		return nil, fmt.Errorf("render %s: %w", ClaudeMCPFile, err)
	}
	return out.Bytes(), nil
}

// renderCodex renders one [mcp_servers.<name>] table per server, in
// name order, separated by a blank line.
func renderCodex(servers map[string]config.MCP, names []string) []byte {
	var out strings.Builder
	for i, name := range names {
		if i > 0 {
			out.WriteByte('\n')
		}
		server := servers[name]
		out.WriteString("[mcp_servers." + tomlKey(name) + "]\n")
		out.WriteString("command = " + tomlString(server.Command) + "\n")
		quoted := make([]string, len(server.Args))
		for j, arg := range server.Args {
			quoted[j] = tomlString(arg)
		}
		out.WriteString("args = [" + strings.Join(quoted, ", ") + "]\n")
	}
	return []byte(out.String())
}

// tomlKey renders a key bare when TOML allows it, quoted otherwise.
func tomlKey(key string) string {
	if bareKey.MatchString(key) {
		return key
	}
	return tomlString(key)
}

// tomlString renders a TOML basic string. The value must be valid UTF-8.
func tomlString(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&out, `\u%04X`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return out.String()
}
