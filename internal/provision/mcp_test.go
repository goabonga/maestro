// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package provision

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/goabonga/maestro/internal/config"
)

// declarations covers every scope.
func declarations() map[string]config.MCP {
	return map[string]config.MCP{
		"github":   {Command: "github-mcp", Args: []string{"stdio"}, Scope: "shared"},
		"docs":     {Command: "docs-mcp", Scope: "shared"},
		"db":       {Command: "db-mcp", Args: []string{"--read-only"}, Scope: "agent:coder"},
		"browser":  {Command: "browser-mcp", Scope: "role:reviewer"},
		"other-db": {Command: "db-mcp", Scope: "agent:planner"},
	}
}

func TestApplicableFiltersByScope(t *testing.T) {
	names, err := Applicable(declarations(), "coder", "implementer")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"db", "docs", "github"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("coder/implementer: %v, want %v", names, want)
	}
	names, err = Applicable(declarations(), "planner", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"browser", "docs", "github", "other-db"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("planner/reviewer: %v, want %v", names, want)
	}
	names, err = Applicable(map[string]config.MCP{"x": {Command: "x", Scope: "role:"}}, "a", "")
	if !errors.Is(err, ErrInvalidMCP) || names != nil {
		t.Fatalf("empty role scope: %v %v", names, err)
	}
}

func TestApplicableRefusesUnknownScopes(t *testing.T) {
	_, err := Applicable(map[string]config.MCP{"x": {Command: "x", Scope: "team:a"}}, "a", "r")
	if !errors.Is(err, ErrInvalidMCP) {
		t.Fatalf("unknown scope: %v", err)
	}
}

func TestEmptyRoleMatchesNoRoleScope(t *testing.T) {
	ok, err := Applies(config.MCP{Command: "x", Scope: "role:reviewer"}, "coder", "")
	if err != nil || ok {
		t.Fatalf("empty role: %v %v", ok, err)
	}
}

func TestTranslateClaude(t *testing.T) {
	got, err := TranslateMCP(declarations(), "coder", KindClaudeCode, "implementer")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "mcpServers": {
    "db": {
      "command": "db-mcp",
      "args": [
        "--read-only"
      ]
    },
    "docs": {
      "command": "docs-mcp",
      "args": []
    },
    "github": {
      "command": "github-mcp",
      "args": [
        "stdio"
      ]
    }
  }
}
`
	if string(got.Content) != want {
		t.Fatalf("content:\n%s", got.Content)
	}
	if got.File != ClaudeMCPFile || got.Kind != KindClaudeCode || !reflect.DeepEqual(got.Servers, []string{"db", "docs", "github"}) {
		t.Fatalf("result %+v", got)
	}
}

func TestTranslateClaudeKeepsValuesVerbatim(t *testing.T) {
	servers := map[string]config.MCP{"x": {Command: `a<b>&"c"`, Args: []string{"line\nbreak", `back\slash`}, Scope: "shared"}}
	got, err := TranslateMCP(servers, "a", KindClaudeCode, "")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(got.Content, &parsed); err != nil {
		t.Fatal(err)
	}
	if server := parsed.MCPServers["x"]; server.Command != servers["x"].Command || !reflect.DeepEqual(server.Args, servers["x"].Args) {
		t.Fatalf("round trip %+v", server)
	}
	if !bytes.Contains(got.Content, []byte(`a<b>&`)) {
		t.Fatalf("HTML characters were escaped:\n%s", got.Content)
	}
}

func TestTranslateClaudeWithoutServers(t *testing.T) {
	got, err := TranslateMCP(nil, "a", KindClaudeCode, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Content) != "{\n  \"mcpServers\": {}\n}\n" || len(got.Servers) != 0 {
		t.Fatalf("content %q", got.Content)
	}
}

func TestTranslateCodex(t *testing.T) {
	got, err := TranslateMCP(declarations(), "planner", KindCodex, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	want := `[mcp_servers.browser]
command = "browser-mcp"
args = []

[mcp_servers.docs]
command = "docs-mcp"
args = []

[mcp_servers.github]
command = "github-mcp"
args = ["stdio"]

[mcp_servers.other-db]
command = "db-mcp"
args = []
`
	if string(got.Content) != want {
		t.Fatalf("content:\n%s", got.Content)
	}
	if got.File != "" || got.Kind != KindCodex {
		t.Fatalf("result %+v", got)
	}
}

func TestTranslateCodexEscapesBasicStrings(t *testing.T) {
	tricky := []string{`quote " here`, `back\slash`, "tab\tnew\nline\r", "bell\x07 del\x7f ff\f bs\b", "unicode é ✓"}
	servers := map[string]config.MCP{
		"x":         {Command: `C:\tools\"mcp"`, Args: tricky, Scope: "shared"},
		"odd.name":  {Command: "y", Scope: "shared"},
		"no-args_1": {Command: "z", Scope: "shared"},
	}
	got, err := TranslateMCP(servers, "a", KindCodex, "")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		MCPServers map[string]struct {
			Command string   `toml:"command"`
			Args    []string `toml:"args"`
		} `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(string(got.Content), &parsed); err != nil {
		t.Fatalf("%v in:\n%s", err, got.Content)
	}
	if len(parsed.MCPServers) != 3 {
		t.Fatalf("servers %v", parsed.MCPServers)
	}
	for name, server := range servers {
		decoded := parsed.MCPServers[name]
		if decoded.Command != server.Command || len(decoded.Args) != len(server.Args) {
			t.Fatalf("%s: %+v", name, decoded)
		}
		for i := range server.Args {
			if decoded.Args[i] != server.Args[i] {
				t.Fatalf("%s arg %d: %q, want %q", name, i, decoded.Args[i], server.Args[i])
			}
		}
	}
	if !bytes.Contains(got.Content, []byte(`[mcp_servers."odd.name"]`)) || !bytes.Contains(got.Content, []byte(`\u0007`)) {
		t.Fatalf("content:\n%s", got.Content)
	}
}

func TestTranslateCodexWithoutServers(t *testing.T) {
	got, err := TranslateMCP(nil, "a", KindCodex, "")
	if err != nil || len(got.Content) != 0 {
		t.Fatalf("%q %v", got.Content, err)
	}
}

func TestTranslateIsByteStable(t *testing.T) {
	for _, kind := range []string{KindClaudeCode, KindCodex} {
		first, err := TranslateMCP(declarations(), "coder", kind, "reviewer")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			again, err := TranslateMCP(declarations(), "coder", kind, "reviewer")
			if err != nil || !bytes.Equal(again.Content, first.Content) {
				t.Fatalf("%s run %d differs: %v", kind, i, err)
			}
		}
	}
}

func TestTranslateRefusesUnknownKinds(t *testing.T) {
	for _, kind := range []string{"", "claude", "openai", "jev"} {
		if _, err := TranslateMCP(declarations(), "coder", kind, ""); !errors.Is(err, ErrUnknownKind) {
			t.Fatalf("kind %q: %v", kind, err)
		}
	}
}

func TestTranslateRefusesUntranslatableDeclarations(t *testing.T) {
	cases := map[string]config.MCP{
		"no command":   {Scope: "shared"},
		"invalid utf8": {Command: "x", Args: []string{"\xff"}, Scope: "shared"},
	}
	for label, server := range cases {
		if _, err := TranslateMCP(map[string]config.MCP{"s": server}, "a", KindCodex, ""); !errors.Is(err, ErrInvalidMCP) {
			t.Fatalf("%s: %v", label, err)
		}
	}
	// A broken declaration that does not apply is not translated.
	servers := map[string]config.MCP{"s": {Scope: "agent:other"}}
	if _, err := TranslateMCP(servers, "a", KindClaudeCode, ""); err != nil {
		t.Fatalf("inapplicable declaration: %v", err)
	}
}
