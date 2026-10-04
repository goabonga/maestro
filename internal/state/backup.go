// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Manifest describes one data backup: the schema version of its
// database snapshot and the fingerprint of every file.
type Manifest struct {
	SchemaVersion int               `json:"schema_version"`
	CreatedAt     time.Time         `json:"created_at"`
	Files         map[string]string `json:"files"`
}

// manifestName is the manifest file inside a backup.
const manifestName = "manifest.json"

// BackupData snapshots the data directory at base into destination, a
// directory that must not exist yet. The daemon must be stopped: both
// locks are taken for the whole copy. The database is snapshotted with
// VACUUM INTO, so the copy is consistent even though WAL files are
// skipped. The manifest records the schema version and a SHA-256 per
// file.
func BackupData(base, destination string) (Manifest, error) {
	locks, err := holdDataLocks(base)
	if err != nil {
		return Manifest{}, err
	}
	defer locks()

	if _, err := os.Stat(destination); err == nil {
		return Manifest{}, fmt.Errorf("destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{CreatedAt: time.Now().UTC(), Files: map[string]string{}}

	err = filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if relative == "." {
				return nil
			}
			return os.MkdirAll(filepath.Join(destination, relative), 0o700)
		}
		if !entry.Type().IsRegular() || skipInBackup(relative) {
			return nil
		}
		digest, err := copyFile(path, filepath.Join(destination, relative))
		if err != nil {
			return err
		}
		manifest.Files[relative] = digest
		return nil
	})
	if err != nil {
		_ = os.RemoveAll(destination)
		return Manifest{}, err
	}

	// Snapshot the database consistently instead of copying its files.
	databasePath := filepath.Join(base, "maestro.db")
	if _, err := os.Stat(databasePath); err == nil {
		db, err := Open(databasePath)
		if err != nil {
			_ = os.RemoveAll(destination)
			return Manifest{}, err
		}
		snapshot := filepath.Join(destination, "maestro.db")
		_, execErr := db.Exec("VACUUM INTO ?", snapshot)
		version, versionErr := db.Version()
		_ = db.Close()
		if execErr != nil || versionErr != nil {
			_ = os.RemoveAll(destination)
			return Manifest{}, errors.Join(execErr, versionErr)
		}
		digest, err := fileDigest(snapshot)
		if err != nil {
			_ = os.RemoveAll(destination)
			return Manifest{}, err
		}
		manifest.Files["maestro.db"] = digest
		manifest.SchemaVersion = version
	}

	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		_ = os.RemoveAll(destination)
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(destination, manifestName), append(encoded, '\n'), 0o600); err != nil {
		_ = os.RemoveAll(destination)
		return Manifest{}, err
	}
	return manifest, nil
}

// RestoreData restores a backup into a new data directory. It never
// replaces active data: the target must not exist or must be empty.
// Every file is verified against the manifest, the database must pass
// its integrity check, and a failed restore removes the partial copy.
func RestoreData(backup, destination string) (Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(backup, manifestName)) // #nosec G304 -- the operator names the backup to restore
	if err != nil {
		return Manifest{}, fmt.Errorf("not a backup (missing %s): %w", manifestName, err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, err
	}
	if entries, err := os.ReadDir(destination); err == nil && len(entries) > 0 {
		return Manifest{}, fmt.Errorf("destination is not empty: %s", destination)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return Manifest{}, err
	}

	restore := func() error {
		paths := make([]string, 0, len(manifest.Files))
		for path := range manifest.Files {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, relative := range paths {
			if !filepath.IsLocal(relative) {
				return fmt.Errorf("manifest escapes the backup: %s", relative)
			}
			target := filepath.Join(destination, relative)
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			digest, err := copyFile(filepath.Join(backup, relative), target)
			if err != nil {
				return err
			}
			if digest != manifest.Files[relative] {
				return fmt.Errorf("fingerprint mismatch for %s", relative)
			}
		}
		databasePath := filepath.Join(destination, "maestro.db")
		if _, err := os.Stat(databasePath); err == nil {
			db, err := Open(databasePath)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			var verdict string
			if err := db.QueryRow("PRAGMA integrity_check").Scan(&verdict); err != nil || verdict != "ok" {
				return fmt.Errorf("database integrity check failed: %s (%v)", verdict, err)
			}
			version, err := db.Version()
			if err != nil {
				return err
			}
			if version != manifest.SchemaVersion {
				return fmt.Errorf("schema version %d does not match the manifest (%d)", version, manifest.SchemaVersion)
			}
		}
		return nil
	}
	if err := restore(); err != nil {
		_ = os.RemoveAll(destination)
		return Manifest{}, err
	}
	return manifest, nil
}

// holdDataLocks takes the daemon and user locks, proving the daemon is
// stopped, and returns their release.
func holdDataLocks(base string) (func(), error) {
	daemon, err := Acquire(filepath.Join(base, "daemon.lock"))
	if err != nil {
		return nil, fmt.Errorf("stop the daemon first: %w", err)
	}
	user, err := Acquire(filepath.Join(base, "lock"))
	if err != nil {
		_ = daemon.Release()
		return nil, err
	}
	return func() {
		_ = user.Release()
		_ = daemon.Release()
	}, nil
}

// skipInBackup filters volatile files out of a backup.
func skipInBackup(relative string) bool {
	name := filepath.Base(relative)
	if name == "lock" || name == "daemon.lock" || name == "svc.log" {
		return filepath.Dir(relative) == "."
	}
	if strings.HasPrefix(name, "maestro.db") {
		// The database is snapshotted separately; WAL and SHM files
		// and previous backups never enter a backup.
		return filepath.Dir(relative) == "."
	}
	return false
}

// copyFile copies one file with owner-only permissions and returns its
// SHA-256.
func copyFile(source, target string) (string, error) {
	in, err := os.Open(source) // #nosec G304 -- paths stay inside the data directory or the named backup
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- same bound
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, digest), in); err != nil {
		_ = out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// fileDigest returns the SHA-256 of a file.
func fileDigest(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304 -- paths stay inside the data directory or the named backup
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
