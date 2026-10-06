// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package provision

import (
	"os"
	"strings"
)

// repositoryVariables are the variables through which Git locates a
// repository, its index, its objects or per-invocation configuration
// (git rev-parse --local-env-vars). Inherited from a git-spawned caller
// (a hook, rebase --exec), they would redirect a command aimed at an
// explicit directory to the caller's repository.
var repositoryVariables = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_PARAMETERS":            true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_IMPLICIT_WORK_TREE":           true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_INDEX_FILE":                   true,
	"GIT_NO_REPLACE_OBJECTS":           true,
	"GIT_REPLACE_REF_BASE":             true,
	"GIT_PREFIX":                       true,
	"GIT_SHALLOW_FILE":                 true,
	"GIT_COMMON_DIR":                   true,
}

// gitEnvironment returns the process environment without the variables
// that select another repository, for the git commands of the package.
func gitEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !repositoryVariables[name] {
			env = append(env, entry)
		}
	}
	return env
}
