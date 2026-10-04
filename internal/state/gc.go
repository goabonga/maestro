// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// GCCandidate is one object a garbage collection would remove.
type GCCandidate struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Reason string `json:"reason"`
}

// GCPlan is the reviewed deletion plan of one garbage collection.
type GCPlan struct {
	CreatedAt  time.Time     `json:"created_at"`
	Candidates []GCCandidate `json:"candidates"`
}

// PlanGC lists the removable objects of the data directory with their
// sizes and reasons, deleting nothing. Today's only candidates are the
// database backups superseded by a newer one; quarantines are never
// planned implicitly.
func PlanGC(base string) (GCPlan, error) {
	plan := GCPlan{CreatedAt: time.Now().UTC()}
	backups, err := filepath.Glob(filepath.Join(base, "maestro.db.backup-*"))
	if err != nil {
		return GCPlan{}, err
	}
	sort.Strings(backups) // the suffix is a nanosecond timestamp
	if len(backups) > 1 {
		for _, path := range backups[:len(backups)-1] {
			info, err := os.Stat(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return GCPlan{}, err
			}
			relative, err := filepath.Rel(base, path)
			if err != nil {
				return GCPlan{}, err
			}
			plan.Candidates = append(plan.Candidates, GCCandidate{
				Path:   relative,
				Size:   info.Size(),
				Reason: "database backup superseded by a newer one",
			})
		}
	}
	return plan, nil
}

// ApplyGC re-plans under the locks, applies only the intersection with
// the reviewed plan, refuses nothing silently — an object that left the
// plan is simply kept — and journals the deletions. A missing candidate
// is skipped, so an interrupted collection can be re-applied.
func ApplyGC(base string, reviewed GCPlan) (string, error) {
	locks, err := holdDataLocks(base)
	if err != nil {
		return "", err
	}
	defer locks()

	current, err := PlanGC(base)
	if err != nil {
		return "", err
	}
	planned := make(map[string]bool, len(current.Candidates))
	for _, candidate := range current.Candidates {
		planned[candidate.Path] = true
	}

	journal := GCPlan{CreatedAt: time.Now().UTC()}
	for _, candidate := range reviewed.Candidates {
		if !planned[candidate.Path] || !filepath.IsLocal(candidate.Path) {
			continue
		}
		path := filepath.Join(base, candidate.Path)
		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", err
		}
		journal.Candidates = append(journal.Candidates, candidate)
	}

	if err := os.MkdirAll(filepath.Join(base, "gc"), 0o700); err != nil {
		return "", err
	}
	encoded, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return "", err
	}
	journalPath := filepath.Join(base, "gc", fmt.Sprintf("%d.json", journal.CreatedAt.UnixNano()))
	if err := os.WriteFile(journalPath, append(encoded, '\n'), 0o600); err != nil {
		return "", err
	}
	return journalPath, nil
}
