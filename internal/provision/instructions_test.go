// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package provision

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goabonga/maestro/internal/config"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// isolate keeps the user's Git configuration out of the tests.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return home
}

func write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// repository creates a repository whose first commit tracks files.
func repository(t *testing.T, tracked map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main")
	run(t, dir, "git", "config", "user.name", "Test")
	run(t, dir, "git", "config", "user.email", "test@example.test")
	run(t, dir, "git", "config", "commit.gpgsign", "false")
	write(t, filepath.Join(dir, "README.md"), "project\n")
	run(t, dir, "git", "add", "README.md")
	for name, content := range tracked {
		write(t, filepath.Join(dir, name), content)
		run(t, dir, "git", "add", "-f", name)
	}
	run(t, dir, "git", "commit", "-q", "-m", "feat: initial")
	return dir
}

// workerWorktree checks a repository out as a linked worktree of a bare
// clone, the way worker repositories hold task worktrees.
func workerWorktree(t *testing.T, source string) string {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "worker.git")
	run(t, base, "git", "clone", "-q", "--bare", "--no-local", source, bare)
	worktree := filepath.Join(base, "task")
	run(t, bare, "git", "worktree", "add", "-q", worktree, "main")
	return worktree
}

func clean(t *testing.T, worktree string) {
	t.Helper()
	if status := run(t, worktree, "git", "status", "--porcelain", "--ignored=no"); status != "" {
		t.Fatalf("worktree not clean:\n%s", status)
	}
}

var claudeFiles = map[string]string{
	"agents/claude-code/CLAUDE.md":                     "kind instructions\n",
	"agents/claude-code/.claude/skills/fmt/SKILL.md":   "kind skill\n",
	"agents/claude-code/roles/review/CLAUDE.md":        "review instructions\n",
	"agents/claude-code/roles/coding/.claude/notes.md": "coding notes\n",
	"agents/codex/AGENTS.md":                           "codex instructions\n",
	"config/unrelated.md":                              "ignored\n",
}

func TestInstructionsMaterializesTheKindFiles(t *testing.T) {
	isolate(t)
	worktree := workerWorktree(t, repository(t, nil))

	paths, err := Instructions(worktree, KindClaudeCode, "", claudeFiles)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".claude/skills/fmt/SKILL.md", "CLAUDE.md"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths=%v", paths)
	}
	if got := read(t, filepath.Join(worktree, "CLAUDE.md")); got != "kind instructions\n" {
		t.Fatalf("CLAUDE.md=%q", got)
	}
	if got := read(t, filepath.Join(worktree, ".claude/skills/fmt/SKILL.md")); got != "kind skill\n" {
		t.Fatalf("SKILL.md=%q", got)
	}
	if _, err := os.Stat(filepath.Join(worktree, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("AGENTS.md provisioned for claude-code: %v", err)
	}
	clean(t, worktree)

	codex := workerWorktree(t, repository(t, nil))
	paths, err = Instructions(codex, KindCodex, "review", claudeFiles)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"AGENTS.md"}) || read(t, filepath.Join(codex, "AGENTS.md")) != "codex instructions\n" {
		t.Fatalf("paths=%v", paths)
	}
	clean(t, codex)
}

func TestInstructionsAppliesTheRoleOverlay(t *testing.T) {
	isolate(t)
	worktree := workerWorktree(t, repository(t, nil))

	paths, err := Instructions(worktree, KindClaudeCode, "review", claudeFiles)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".claude/skills/fmt/SKILL.md", "CLAUDE.md"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths=%v", paths)
	}
	if got := read(t, filepath.Join(worktree, "CLAUDE.md")); got != "review instructions\n" {
		t.Fatalf("CLAUDE.md=%q", got)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".claude/notes.md")); !os.IsNotExist(err) {
		t.Fatalf("another role's file was provisioned: %v", err)
	}
}

func TestInstructionsComeFromTheSnapshotNotTheDisk(t *testing.T) {
	isolate(t)
	source := repository(t, nil)
	write(t, filepath.Join(source, "maestro/agents/codex/AGENTS.md"), "frozen\n")
	snapshot, err := config.Take(source)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(source, "maestro/agents/codex/AGENTS.md"), "edited later\n")

	worktree := workerWorktree(t, source)
	if _, err := Instructions(worktree, KindCodex, "", snapshot.Instructions); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(worktree, "AGENTS.md")); got != "frozen\n" {
		t.Fatalf("AGENTS.md=%q", got)
	}
}

func TestInstructionsKeepsATrackedClaudeFile(t *testing.T) {
	isolate(t)
	worktree := workerWorktree(t, repository(t, map[string]string{"CLAUDE.md": "the project's own\n"}))

	paths, err := Instructions(worktree, KindClaudeCode, "", claudeFiles)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".claude/skills/fmt/SKILL.md", "CLAUDE.local.md"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths=%v", paths)
	}
	if got := read(t, filepath.Join(worktree, "CLAUDE.md")); got != "the project's own\n" {
		t.Fatalf("tracked CLAUDE.md changed: %q", got)
	}
	if got := read(t, filepath.Join(worktree, "CLAUDE.local.md")); got != "kind instructions\n" {
		t.Fatalf("CLAUDE.local.md=%q", got)
	}
	clean(t, worktree)
	// The tracked file stays visible to Git: it is never ignored.
	write(t, filepath.Join(worktree, "CLAUDE.md"), "edited\n")
	if status := run(t, worktree, "git", "status", "--porcelain"); status != "M CLAUDE.md" {
		t.Fatalf("status=%q", status)
	}
}

func TestInstructionsRefusesToReplaceATrackedFile(t *testing.T) {
	isolate(t)
	for _, scenario := range []struct {
		kind    string
		tracked map[string]string
		path    string
	}{
		{KindCodex, map[string]string{"AGENTS.md": "own\n"}, "AGENTS.md"},
		{KindClaudeCode, map[string]string{"CLAUDE.md": "own\n", "CLAUDE.local.md": "own\n"}, "CLAUDE.local.md"},
		{KindClaudeCode, map[string]string{".claude/skills/fmt/SKILL.md": "own\n"}, ".claude/skills/fmt/SKILL.md"},
	} {
		worktree := workerWorktree(t, repository(t, scenario.tracked))
		_, err := Instructions(worktree, scenario.kind, "", claudeFiles)
		if !errors.Is(err, ErrPathTaken) || !strings.Contains(err.Error(), scenario.path+" is tracked") {
			t.Fatalf("%s: error %v", scenario.path, err)
		}
		for name, content := range scenario.tracked {
			if got := read(t, filepath.Join(worktree, name)); got != content {
				t.Fatalf("%s changed: %q", name, got)
			}
		}
		clean(t, worktree)
		// Nothing was written before the refusal.
		if _, err := os.Stat(filepath.Join(worktree, ".claude/skills/fmt")); scenario.kind == KindClaudeCode && scenario.path != ".claude/skills/fmt/SKILL.md" && !os.IsNotExist(err) {
			t.Fatalf("partial provisioning: %v", err)
		}
	}
}

func TestInstructionsRefusesAFileItDidNotProvision(t *testing.T) {
	isolate(t)
	worktree := workerWorktree(t, repository(t, nil))
	write(t, filepath.Join(worktree, "AGENTS.md"), "agent's own\n")

	_, err := Instructions(worktree, KindCodex, "", claudeFiles)
	if !errors.Is(err, ErrPathTaken) || !strings.Contains(err.Error(), "not provisioned by Maestro") {
		t.Fatalf("error %v", err)
	}
	if got := read(t, filepath.Join(worktree, "AGENTS.md")); got != "agent's own\n" {
		t.Fatalf("AGENTS.md=%q", got)
	}
}

func TestInstructionsRefusesALinkedDirectory(t *testing.T) {
	isolate(t)
	worktree := workerWorktree(t, repository(t, nil))
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(worktree, ".claude")); err != nil {
		t.Fatal(err)
	}
	_, err := Instructions(worktree, KindClaudeCode, "", claudeFiles)
	if !errors.Is(err, ErrPathTaken) || !strings.Contains(err.Error(), ".claude is not a directory") {
		t.Fatalf("error %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote through the link: %v", entries)
	}
}

func TestInstructionsIsIdempotentAndDropsStaleFiles(t *testing.T) {
	isolate(t)
	worktree := workerWorktree(t, repository(t, nil))
	first, err := Instructions(worktree, KindClaudeCode, "", claudeFiles)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Instructions(worktree, KindClaudeCode, "", claudeFiles)
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatalf("again=%v err=%v", again, err)
	}
	excludes := read(t, ExcludesFile(filepath.Join(run(t, worktree, "git", "rev-parse", "--absolute-git-dir"))))
	if strings.Count(excludes, "/CLAUDE.md\n") != 1 {
		t.Fatalf("excludes:\n%s", excludes)
	}

	reduced := map[string]string{"agents/claude-code/CLAUDE.md": "new\n"}
	paths, err := Instructions(worktree, KindClaudeCode, "", reduced)
	if err != nil || !reflect.DeepEqual(paths, []string{"CLAUDE.md"}) {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
	if got := read(t, filepath.Join(worktree, "CLAUDE.md")); got != "new\n" {
		t.Fatalf("CLAUDE.md=%q", got)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("stale .claude kept: %v", err)
	}
	clean(t, worktree)
}

func TestInstructionsExcludesThroughTheWorktreeConfig(t *testing.T) {
	home := isolate(t)
	write(t, filepath.Join(home, ".config/git/ignore"), "*.log")
	source := repository(t, nil)
	worktree := workerWorktree(t, source)

	if _, err := Instructions(worktree, KindCodex, "", claudeFiles); err != nil {
		t.Fatal(err)
	}
	gitDir := run(t, worktree, "git", "rev-parse", "--absolute-git-dir")
	excludes := run(t, worktree, "git", "config", "--worktree", "core.excludesFile")
	if excludes != ExcludesFile(gitDir) || strings.HasPrefix(excludes, worktree+string(filepath.Separator)) {
		t.Fatalf("core.excludesFile=%s", excludes)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "info", "exclude")); !os.IsNotExist(err) {
		t.Fatalf("per-worktree info/exclude used: %v", err)
	}
	// Git's default excludes still apply alongside Maestro's paths.
	write(t, filepath.Join(worktree, "debug.log"), "x\n")
	clean(t, worktree)
	if got := run(t, worktree, "git", "check-ignore", "AGENTS.md", "debug.log"); got != "AGENTS.md\ndebug.log" {
		t.Fatalf("check-ignore=%q", got)
	}
	// The setting is scoped to this worktree: the bare worker repository
	// keeps working and holds no excludes setting of its own.
	common := run(t, worktree, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if out, err := exec.Command("git", "-C", common, "config", "--local", "core.excludesFile").Output(); err == nil {
		t.Fatalf("repository-wide core.excludesFile=%s", out)
	}
	if bare := run(t, common, "git", "rev-parse", "--is-bare-repository"); bare != "true" {
		t.Fatalf("bare=%s", bare)
	}
}

func TestInstructionsCopiesTheConfiguredExcludesFile(t *testing.T) {
	isolate(t)
	worktree := workerWorktree(t, repository(t, nil))
	custom := filepath.Join(t.TempDir(), "excludes")
	write(t, custom, "*.tmp\n")
	common := run(t, worktree, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	run(t, common, "git", "config", "core.excludesFile", custom)

	for range 2 {
		if _, err := Instructions(worktree, KindCodex, "", claudeFiles); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(worktree, "scratch.tmp"), "x\n")
	clean(t, worktree)
	excludes := read(t, ExcludesFile(run(t, worktree, "git", "rev-parse", "--absolute-git-dir")))
	if !strings.HasPrefix(excludes, "*.tmp\n") || strings.Count(excludes, "*.tmp") != 1 {
		t.Fatalf("excludes:\n%s", excludes)
	}
}

func TestInstructionsRefusesInvalidInput(t *testing.T) {
	isolate(t)
	worktree := workerWorktree(t, repository(t, nil))
	if _, err := Instructions(worktree, "llm", "", claudeFiles); !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("kind: %v", err)
	}
	if _, err := Instructions(worktree, KindCodex, "../x", claudeFiles); !errors.Is(err, ErrInvalidInstructions) {
		t.Fatalf("role: %v", err)
	}
	for _, name := range []string{"agents/codex/README.md", "agents/claude-code/.claude/.git/config", "agents/codex/roles/review/CLAUDE.md"} {
		_, err := Instructions(worktree, map[bool]string{true: KindCodex, false: KindClaudeCode}[strings.Contains(name, "codex")], "review", map[string]string{name: "x\n"})
		if !errors.Is(err, ErrInvalidInstructions) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := Instructions(filepath.Join(worktree, ".."), KindCodex, "", claudeFiles); err == nil {
		t.Fatal("provisioned outside a worktree")
	}
	clean(t, worktree)
}

func TestEscapePatternMatchesLiterally(t *testing.T) {
	if got := escapePattern(".claude/skills/a*b [x]/!#?.md"); got != `.claude/skills/a\*b\ \[x]/\!\#\?.md` {
		t.Fatalf("escaped=%s", got)
	}
}

func TestInstructionsWorksInAMainWorktree(t *testing.T) {
	isolate(t)
	worktree := repository(t, nil)
	if _, err := Instructions(worktree, KindCodex, "", claudeFiles); err != nil {
		t.Fatal(err)
	}
	clean(t, worktree)
	if bare := run(t, worktree, "git", "rev-parse", "--is-bare-repository"); bare != "false" {
		t.Fatalf("bare=%s", bare)
	}
}
