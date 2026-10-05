// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// project writes the layer files of a repository and returns its path.
func project(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadWithoutFilesGivesTheDefaults(t *testing.T) {
	config, err := Load(project(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if config.Budgets.MaxTurnsPerTask != 30 || config.Budgets.TurnTimeout.Duration != 20*time.Minute ||
		config.Budgets.InputWaitTimeout.Duration != 5*time.Minute || config.Budgets.TaskTimeout.Duration != 2*time.Hour {
		t.Fatalf("defaults %+v", config.Budgets)
	}
}

func TestLayersOverrideKeyByKey(t *testing.T) {
	config, err := Load(project(t, map[string]string{
		ProjectFile: `
[budgets]
max_turns_per_task = 12
turn_timeout = "30m"

[agents.reviewer]
driver = "openai-compatible"
base_url = "https://api.openai.com/v1"
model = "gpt-5.2"
api_key_env = "OPENAI_API_KEY"
max_turns_per_task = 2

[mcp.github]
command = "github-mcp-server"
scope = "shared"
`,
		LocalFile: `
[budgets]
turn_timeout = "45m"

[agents.reviewer]
model = "gpt-5.2-mini"

[agents.local-coder]
driver = "openai-compatible"
base_url = "http://127.0.0.1:11434/v1"
model = "qwen3.5-coder"
api_key_env = ""
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	budgets := config.Budgets
	if budgets.MaxTurnsPerTask != 12 || budgets.TurnTimeout.Duration != 45*time.Minute || budgets.TaskTimeout.Duration != 2*time.Hour {
		t.Fatalf("budgets %+v", budgets)
	}
	reviewer := config.Agents["reviewer"]
	if reviewer.Model != "gpt-5.2-mini" || reviewer.BaseURL != "https://api.openai.com/v1" ||
		reviewer.APIKeyEnv != "OPENAI_API_KEY" || reviewer.MaxTurnsPerTask != 2 {
		t.Fatalf("the local layer did not merge field by field: %+v", reviewer)
	}
	if _, ok := config.Agents["local-coder"]; !ok || config.MCP["github"].Scope != "shared" {
		t.Fatalf("config %+v", config)
	}
}

func TestLoadRefusesInvalidConfigurations(t *testing.T) {
	for name, content := range map[string]string{
		"unknown key":        "[budgets]\nmax_turns = 3\n",
		"unknown table":      "[telemetry]\nenabled = true\n",
		"syntax":             "[budgets\n",
		"secret key":         "[agents.reviewer]\napi_key = \"abc\"\n",
		"token key":          "[mcp.github]\ncommand = \"x\"\nscope = \"shared\"\ntoken = \"abc\"\n",
		"secret value":       "[mcp.github]\ncommand = \"x\"\nscope = \"shared\"\nargs = [\"--key\", \"sk-ant-FAKE0123456789abcdefFAKE\"]\n",
		"bad env name":       "[agents.reviewer]\napi_key_env = \"OPENAI KEY\"\n",
		"bad scope":          "[mcp.github]\ncommand = \"x\"\nscope = \"everyone\"\n",
		"missing command":    "[mcp.github]\nscope = \"shared\"\n",
		"bad duration":       "[budgets]\nturn_timeout = \"soon\"\n",
		"zero turns":         "[budgets]\nmax_turns_per_task = 0\n",
		"negative agent cap": "[agents.coder]\nmax_turns_per_task = -1\n",
		"bad agent name":     "[agents.Coder]\ndriver = \"x\"\n",
		"zero agent timeout": "[agents.coder]\nturn_timeout = \"0s\"\n",
	} {
		if _, err := Load(project(t, map[string]string{ProjectFile: content})); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: expected ErrInvalid, got %v", name, err)
		}
	}
	// The local layer is held to the same rules.
	_, err := Load(project(t, map[string]string{LocalFile: "[agents.coder]\npassword = \"x\"\n"}))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), LocalFile) {
		t.Fatalf("local layer: %v", err)
	}
}
