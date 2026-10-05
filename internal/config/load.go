// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package config

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Layer file names, from the weakest to the strongest.
const (
	// ProjectFile is versioned and shared with the repository.
	ProjectFile = ".maestro.toml"
	// LocalFile overrides it on one machine and is never versioned.
	LocalFile = ".maestro.local.toml"
)

// secretValues are value shapes that never belong in configuration.
var secretValues = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`),
}

// Load reads a project's configuration: Maestro's defaults, then
// .maestro.toml, then .maestro.local.toml, each present key of a
// stronger layer replacing the weaker value. Unknown keys and anything
// that looks like a secret are refused; secrets live in the environment
// only, named by api_key_env.
func Load(repository string) (Config, error) {
	merged := map[string]any{}
	for _, file := range []string{ProjectFile, LocalFile} {
		layer, err := readLayer(filepath.Join(repository, file))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Config{}, err
		}
		if err := refuseSecrets(file, layer, nil); err != nil {
			return Config{}, err
		}
		merge(merged, layer)
	}

	config := Defaults()
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(merged); err != nil {
		return Config{}, err
	}
	metadata, err := toml.Decode(encoded.String(), &config)
	if err != nil {
		return Config{}, invalid("%v", err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		return Config{}, invalid("unknown keys: %s", strings.Join(keys, ", "))
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// readLayer decodes one TOML file into a generic tree.
func readLayer(path string) (map[string]any, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- one of the two layer files of the repository
	if err != nil {
		return nil, err
	}
	layer := map[string]any{}
	if _, err := toml.Decode(string(data), &layer); err != nil {
		return nil, invalid("%s: %v", filepath.Base(path), err)
	}
	return layer, nil
}

// merge copies a stronger layer onto the merged tree, table by table.
func merge(into, layer map[string]any) {
	for key, value := range layer {
		table, isTable := value.(map[string]any)
		existing, hasTable := into[key].(map[string]any)
		if isTable && hasTable {
			merge(existing, table)
			continue
		}
		if isTable {
			value = maps.Clone(table)
		}
		into[key] = value
	}
}

// refuseSecrets walks a layer: no key may name a secret (except the
// *_env keys that name a variable) and no string may look like one.
func refuseSecrets(file string, tree map[string]any, path []string) error {
	for key, value := range tree {
		where := strings.Join(append(append([]string(nil), path...), key), ".")
		if secretKeys.MatchString(key) {
			return invalid("%s: %s looks like a secret; name its environment variable with an *_env key instead", file, where)
		}
		if err := refuseSecretValue(file, where, value); err != nil {
			return err
		}
	}
	return nil
}

// refuseSecretValue checks one value, recursing into tables and arrays.
func refuseSecretValue(file, where string, value any) error {
	switch typed := value.(type) {
	case map[string]any:
		return refuseSecrets(file, typed, strings.Split(where, "."))
	case []any:
		for _, item := range typed {
			if err := refuseSecretValue(file, where, item); err != nil {
				return err
			}
		}
	case []map[string]any:
		for _, item := range typed {
			if err := refuseSecrets(file, item, strings.Split(where, ".")); err != nil {
				return err
			}
		}
	case string:
		for _, pattern := range secretValues {
			if pattern.MatchString(typed) {
				return invalid("%s: %s holds a value shaped like a secret; keep secrets in the environment", file, where)
			}
		}
	}
	return nil
}
