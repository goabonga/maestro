// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package provision

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Agent kinds whose instruction files have a native location.
const (
	KindClaudeCode = "claude-code"
	KindCodex      = "codex"
)

// AgentsDir is the directory, relative to the snapshot's instruction
// root (the project's maestro/ directory), holding one subdirectory per
// agent kind. Under maestro/agents/<kind>/, a file sits at its native
// path relative to the worktree; maestro/agents/<kind>/roles/<role>/
// holds the role overlay, whose files replace the kind's files at the
// same native path.
const AgentsDir = "agents"

// RolesDir names the overlay directory inside an agent kind directory.
const RolesDir = "roles"

// claudeLocal is where a Claude Code CLAUDE.md goes when the repository
// tracks its own CLAUDE.md.
const claudeLocal = "CLAUDE.local.md"

// stateDir is the directory, inside the worktree's private Git
// directory, holding Maestro's provisioning state.
const stateDir = "maestro"

// Errors returned by Instructions, wrapped with details.
var (
	// ErrUnsupportedKind reports an agent kind without native
	// instruction locations.
	ErrUnsupportedKind = errors.New("unsupported agent kind")
	// ErrInvalidInstructions reports an instruction file Maestro cannot
	// place, or an invalid role.
	ErrInvalidInstructions = errors.New("invalid instruction files")
	// ErrPathTaken reports a native location already held by a file
	// tracked by the repository, or by a file Maestro did not provision.
	// Maestro never replaces it.
	ErrPathTaken = errors.New("instruction path already taken")
)

// roleName constrains role names to safe path components.
var roleName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// nativePath reports whether an agent kind reads a file at name.
func nativePath(kind, name string) bool {
	switch kind {
	case KindClaudeCode:
		return name == "CLAUDE.md" || strings.HasPrefix(name, ".claude/")
	case KindCodex:
		return name == "AGENTS.md"
	}
	return false
}

// Instructions materializes, into the root of a task worktree, the
// instruction files of one agent kind and role taken from a
// configuration snapshot's instructions (paths relative to maestro/).
// For claude-code, CLAUDE.md and the .claude/ tree (skills under
// .claude/skills/); for codex, AGENTS.md. A role overlay file replaces
// the kind's file at the same native path; an empty role applies none.
//
// A file the repository tracks is never replaced: a tracked CLAUDE.md
// sends Claude Code's file to CLAUDE.local.md when that path is free,
// and any other collision fails with ErrPathTaken before anything is
// written. The provisioned paths are excluded from Git through a
// worktree-scoped core.excludesFile kept in the worktree's private Git
// directory, which first copies the repository's effective excludes.
//
// Provisioning again is idempotent and removes the files a previous
// provisioning left that the new one no longer carries. It returns the
// provisioned paths, relative to the worktree and sorted.
func Instructions(worktree, kind, role string, instructions map[string]string) ([]string, error) {
	files, err := selectFiles(kind, role, instructions)
	if err != nil {
		return nil, err
	}
	repo, err := openWorktree(worktree)
	if err != nil {
		return nil, err
	}
	previous, err := repo.readManifest()
	if err != nil {
		return nil, err
	}
	targets, err := repo.resolve(kind, files, previous)
	if err != nil {
		return nil, err
	}
	provisioned := make([]string, 0, len(targets))
	for target := range targets {
		provisioned = append(provisioned, target)
	}
	sort.Strings(provisioned)

	// Exclude both sets while files change, so no state of the worktree
	// shows Maestro's files to Git, then settle on the new set.
	if err := repo.recordAndExclude(union(previous, provisioned)); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(repo.top)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if err := repo.removeStale(root, previous, targets); err != nil {
		return nil, err
	}
	for _, target := range provisioned {
		if err := writeFile(root, target, targets[target]); err != nil {
			return nil, err
		}
	}
	if err := repo.recordAndExclude(provisioned); err != nil {
		return nil, err
	}
	return provisioned, nil
}

// selectFiles returns the native files of a kind with the role overlay
// applied, by native path.
func selectFiles(kind, role string, instructions map[string]string) (map[string]string, error) {
	if kind != KindClaudeCode && kind != KindCodex {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedKind, kind)
	}
	if role != "" && !roleName.MatchString(role) {
		return nil, fmt.Errorf("%w: invalid role %q", ErrInvalidInstructions, role)
	}
	prefix := AgentsDir + "/" + kind + "/"
	overlay := RolesDir + "/" + role + "/"
	base, overlaid := map[string]string{}, map[string]string{}
	for name, content := range instructions {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		target := base
		if strings.HasPrefix(rest, RolesDir+"/") {
			if role == "" || !strings.HasPrefix(rest, overlay) {
				continue
			}
			rest, target = strings.TrimPrefix(rest, overlay), overlaid
		}
		if err := validPath(rest); err != nil {
			return nil, fmt.Errorf("%w: %s/%s: %v", ErrInvalidInstructions, "maestro", name, err)
		}
		if !nativePath(kind, rest) {
			return nil, fmt.Errorf("%w: maestro/%s: %s does not read %s", ErrInvalidInstructions, name, kind, rest)
		}
		target[rest] = content
	}
	for name, content := range overlaid {
		base[name] = content
	}
	return base, nil
}

// validPath checks that a native path is a clean relative path Git and
// the excludes file can represent.
func validPath(name string) error {
	if !fs.ValidPath(name) || name == "." {
		return errors.New("not a clean relative path")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("control character in path")
		}
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") {
			return errors.New("path inside .git")
		}
	}
	return nil
}

// worktreeRepo locates one worktree and its Git directories.
type worktreeRepo struct {
	top    string // worktree root
	gitDir string // the worktree's private Git directory
	common string // the repository's common Git directory
}

// openWorktree checks that path is the root of a Git worktree.
func openWorktree(worktree string) (worktreeRepo, error) {
	out, err := gitIn(worktree, "rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return worktreeRepo{}, err
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		return worktreeRepo{}, fmt.Errorf("unexpected rev-parse output for %s", worktree)
	}
	abs, err := filepath.Abs(worktree)
	if err != nil {
		return worktreeRepo{}, err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if filepath.Clean(lines[0]) != abs {
		return worktreeRepo{}, fmt.Errorf("%s is not the root of its worktree (%s)", worktree, lines[0])
	}
	return worktreeRepo{top: lines[0], gitDir: lines[1], common: lines[2]}, nil
}

// resolve places every native file, falling back to CLAUDE.local.md for
// a taken CLAUDE.md, and fails before any write on a taken path.
func (r worktreeRepo) resolve(kind string, files map[string]string, previous []string) (map[string]string, error) {
	candidates := make([]string, 0, len(files)+1)
	for name := range files {
		candidates = append(candidates, name)
	}
	candidates = append(candidates, claudeLocal)
	tracked, err := r.tracked(candidates)
	if err != nil {
		return nil, err
	}
	ours := map[string]bool{}
	for _, name := range previous {
		ours[name] = true
	}
	free := func(name string) error {
		if tracked[name] {
			return fmt.Errorf("%w: %s is tracked by the repository", ErrPathTaken, name)
		}
		return r.checkPlace(name, ours[name])
	}
	targets := map[string]string{}
	for name, content := range files {
		target := name
		if err := free(name); err != nil {
			if kind != KindClaudeCode || name != "CLAUDE.md" {
				return nil, err
			}
			if fallback := free(claudeLocal); fallback != nil {
				return nil, fmt.Errorf("%v, and %w", err, fallback)
			}
			target = claudeLocal
		}
		targets[target] = content
	}
	return targets, nil
}

// checkPlace verifies that a path can be written without following a
// link and without replacing a file Maestro did not provision.
func (r worktreeRepo) checkPlace(name string, ours bool) error {
	parts := strings.Split(name, "/")
	current := r.top
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		last := i == len(parts)-1
		switch {
		case !last && !info.IsDir():
			return fmt.Errorf("%w: %s is not a directory", ErrPathTaken, path.Join(parts[:i+1]...))
		case last && !info.Mode().IsRegular():
			return fmt.Errorf("%w: %s is not a regular file", ErrPathTaken, name)
		case last && !ours:
			return fmt.Errorf("%w: %s exists and was not provisioned by Maestro", ErrPathTaken, name)
		}
	}
	return nil
}

// tracked returns which of the paths the repository's index tracks.
func (r worktreeRepo) tracked(names []string) (map[string]bool, error) {
	args := []string{"ls-files", "-z", "--"}
	for _, name := range names {
		args = append(args, ":(literal)"+name)
	}
	out, err := gitIn(r.top, args...)
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, name := range strings.Split(out, "\x00") {
		if name != "" {
			tracked[name] = true
		}
	}
	return tracked, nil
}

// removeStale deletes the files of a previous provisioning that the new
// one does not carry, unless the repository has started tracking them.
func (r worktreeRepo) removeStale(root *os.Root, previous []string, targets map[string]string) error {
	var stale []string
	for _, name := range previous {
		if _, kept := targets[name]; !kept {
			stale = append(stale, name)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	tracked, err := r.tracked(stale)
	if err != nil {
		return err
	}
	for _, name := range stale {
		if tracked[name] {
			continue
		}
		if info, err := root.Lstat(name); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := root.Remove(name); err != nil {
			return err
		}
		// Drop the directories the file leaves empty; Remove refuses
		// a directory that still holds anything.
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if root.Remove(dir) != nil {
				break
			}
		}
	}
	return nil
}

// writeFile writes one provisioned file inside the worktree, leaving it
// untouched when it already holds the content.
func writeFile(root *os.Root, name, content string) error {
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	if current, err := root.ReadFile(name); err == nil && bytes.Equal(current, []byte(content)) {
		return nil
	}
	return root.WriteFile(name, []byte(content), 0o600)
}

// union returns the sorted union of two path lists.
func union(a, b []string) []string {
	seen := map[string]bool{}
	var all []string
	for _, name := range append(append([]string{}, a...), b...) {
		if !seen[name] {
			seen[name] = true
			all = append(all, name)
		}
	}
	sort.Strings(all)
	return all
}

// manifestPath holds the paths of the last provisioning, one per line.
func (r worktreeRepo) manifestPath() string {
	return filepath.Join(r.gitDir, stateDir, "instructions")
}

// ExcludesFile returns the worktree-scoped excludes file Maestro keeps
// for a worktree's Git directory: it lives outside the worktree sources.
func ExcludesFile(gitDir string) string {
	return filepath.Join(gitDir, stateDir, "excludes")
}

// readManifest returns the paths the last provisioning wrote.
func (r worktreeRepo) readManifest() ([]string, error) {
	content, err := os.ReadFile(r.manifestPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Split(string(content), "\n") {
		if name != "" && validPath(name) == nil {
			names = append(names, name)
		}
	}
	return names, nil
}

// recordAndExclude records the provisioned paths and excludes them from
// Git for this worktree only.
func (r worktreeRepo) recordAndExclude(names []string) error {
	if err := os.MkdirAll(filepath.Join(r.gitDir, stateDir), 0o750); err != nil {
		return err
	}
	if err := writeAtomic(r.manifestPath(), []byte(strings.Join(names, "\n")+"\n")); err != nil {
		return err
	}
	if err := r.enableWorktreeConfig(); err != nil {
		return err
	}
	excludes := ExcludesFile(r.gitDir)
	inherited, err := r.inheritedExcludes(excludes)
	if err != nil {
		return err
	}
	var content bytes.Buffer
	content.Write(inherited)
	if len(inherited) > 0 && !bytes.HasSuffix(inherited, []byte("\n")) {
		content.WriteByte('\n')
	}
	content.WriteString("# Paths provisioned by Maestro\n")
	for _, name := range names {
		content.WriteString("/" + escapePattern(name) + "\n")
	}
	if err := writeAtomic(excludes, content.Bytes()); err != nil {
		return err
	}
	_, err = gitIn(r.top, "config", "--worktree", "core.excludesFile", excludes)
	return err
}

// enableWorktreeConfig turns extensions.worktreeConfig on. Git then
// reads core.bare and core.worktree from the common configuration for
// every worktree, so they move to the main worktree's config.worktree
// first, as Git documents.
func (r worktreeRepo) enableWorktreeConfig() error {
	common := filepath.Join(r.common, "config")
	main := filepath.Join(r.common, "config.worktree")
	if value, _ := gitIn(r.top, "config", "--file", common, "--type=bool", "--get", "extensions.worktreeConfig"); value == "true" {
		return nil
	}
	for _, key := range []string{"core.bare", "core.worktree"} {
		value, err := gitIn(r.top, "config", "--file", common, "--get", key)
		if err != nil {
			continue
		}
		if _, err := gitIn(r.top, "config", "--file", main, "--get", key); err != nil {
			if _, err := gitIn(r.top, "config", "--file", main, key, value); err != nil {
				return err
			}
		}
		if _, err := gitIn(r.top, "config", "--file", common, "--unset", key); err != nil {
			return err
		}
	}
	_, err := gitIn(r.top, "config", "--file", common, "extensions.worktreeConfig", "true")
	return err
}

// inheritedExcludes returns the content of the excludes file Git would
// use without Maestro: the effective core.excludesFile other than
// Maestro's own, or Git's default user excludes file.
func (r worktreeRepo) inheritedExcludes(own string) ([]byte, error) {
	source := ""
	out, err := gitIn(r.top, "config", "--type=path", "--get-all", "core.excludesFile")
	if err == nil {
		for _, value := range strings.Split(out, "\n") {
			if value != "" && filepath.Clean(value) != filepath.Clean(own) {
				source = value
			}
		}
	}
	if source == "" {
		source = defaultExcludes()
	} else if !filepath.IsAbs(source) {
		source = filepath.Join(r.top, source)
	}
	if source == "" {
		return nil, nil
	}
	content, err := os.ReadFile(source) // #nosec G304 -- the path comes from the user's own Git configuration, read as Git itself reads it
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return content, err
}

// defaultExcludes returns Git's default user excludes file.
func defaultExcludes() string {
	if config := os.Getenv("XDG_CONFIG_HOME"); config != "" {
		return filepath.Join(config, "git", "ignore")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "git", "ignore")
	}
	return ""
}

// escapePattern quotes a path so gitignore matches it literally.
func escapePattern(name string) string {
	var escaped strings.Builder
	for _, r := range name {
		if strings.ContainsRune(`\*?[ !#`, r) {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(r)
	}
	return escaped.String()
}

// writeAtomic replaces a file through a temporary sibling.
func writeAtomic(name string, content []byte) error {
	temporary := name + ".tmp"
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, name)
}

// gitIn runs one git command in a directory.
func gitIn(dir string, args ...string) (string, error) {
	command := exec.Command("git", args...) // #nosec G204 -- fixed verbs; paths are validated snapshot paths or Maestro's own
	command.Dir = dir
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(string(output), "\n"), nil
}
