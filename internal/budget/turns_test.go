// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package budget

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goabonga/maestro/internal/config"
	"github.com/goabonga/maestro/internal/state"
	"github.com/goabonga/maestro/internal/task"
)

// clock is a manual clock.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// open opens and migrates the database at path.
func open(t *testing.T, path string) *state.DB {
	t.Helper()
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(state.Migrations); err != nil {
		t.Fatal(err)
	}
	return db
}

// fixture opens a migrated store and creates one task on a snapshot of
// cfg.
func fixture(t *testing.T, cfg config.Config) (Store, *clock, string) {
	t.Helper()
	db := open(t, filepath.Join(t.TempDir(), "maestro.db"))
	configID, err := config.Persist(db, config.Snapshot{Config: cfg, Instructions: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{at: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	created, err := task.Store{DB: db, Now: c.now}.Create("project-1", "budget test task", configID, strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	return Store{DB: db, Now: c.now}, c, created.ID
}

// caps is the default configuration with a total cap and agent caps.
func caps(total int, agents map[string]int) config.Config {
	cfg := config.Defaults()
	cfg.Budgets.MaxTurnsPerTask = total
	cfg.Agents = map[string]config.Agent{}
	for name, limit := range agents {
		cfg.Agents[name] = config.Agent{MaxTurnsPerTask: limit}
	}
	return cfg
}

func TestReserveCountsTurnsPerTaskAndAgent(t *testing.T) {
	store, _, taskID := fixture(t, caps(5, map[string]int{"reviewer": 2}))
	for i, agent := range []string{"coder", "reviewer", "coder"} {
		reservation, err := store.Reserve(taskID, agent, fmt.Sprintf("send-%d", i))
		if err != nil || reservation.Replayed || reservation.Agent != agent {
			t.Fatalf("reserve %d: %+v %v", i, reservation, err)
		}
	}
	turns, err := store.Turns(taskID)
	if err != nil || turns.Task != 3 || turns.Agents["coder"] != 2 || turns.Agents["reviewer"] != 1 {
		t.Fatalf("turns %+v %v", turns, err)
	}
}

func TestReserveEnforcesTheAgentCap(t *testing.T) {
	store, _, taskID := fixture(t, caps(5, map[string]int{"reviewer": 2}))
	for i := range 2 {
		if _, err := store.Reserve(taskID, "reviewer", fmt.Sprintf("review-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := store.Reserve(taskID, "reviewer", "review-2")
	var exceeded *ExceededError
	if !errors.Is(err, ErrExceeded) || !errors.As(err, &exceeded) || exceeded.Bound != "agents.reviewer.max_turns_per_task" ||
		exceeded.Limit != "2" || exceeded.Used != "2" {
		t.Fatalf("third review: %v", err)
	}
	// The agent cap stops the agent only: the task total still has room.
	if _, err := store.Reserve(taskID, "coder", "code-0"); err != nil {
		t.Fatal(err)
	}
	if turns, _ := store.Turns(taskID); turns.Task != 3 {
		t.Fatalf("a refused reservation was counted: %+v", turns)
	}
}

func TestReserveEnforcesTheTaskTotalOverAnAgentCap(t *testing.T) {
	// An agent cap above the total never replaces it.
	store, _, taskID := fixture(t, caps(2, map[string]int{"coder": 10}))
	if _, err := store.Reserve(taskID, "coder", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve(taskID, "reviewer", "b"); err != nil {
		t.Fatal(err)
	}
	_, err := store.Reserve(taskID, "coder", "c")
	var exceeded *ExceededError
	if !errors.As(err, &exceeded) || exceeded.Bound != BoundTaskTurns || exceeded.Limit != "2" ||
		!strings.Contains(err.Error(), "budgets.max_turns_per_task") {
		t.Fatalf("third turn: %v", err)
	}
}

func TestReserveIsIdempotentPerKey(t *testing.T) {
	store, c, taskID := fixture(t, caps(1, nil))
	first, err := store.Reserve(taskID, "coder", "send-1")
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	// A retry of the same send, even with the cap reached, returns the
	// first reservation without consuming a turn.
	again, err := store.Reserve(taskID, "coder", "send-1")
	if err != nil || !again.Replayed || !again.ReservedAt.Equal(first.ReservedAt) {
		t.Fatalf("retry: %+v %v", again, err)
	}
	if turns, _ := store.Turns(taskID); turns.Task != 1 {
		t.Fatalf("a retry consumed a turn: %+v", turns)
	}
	if _, err := store.Reserve(taskID, "reviewer", "send-1"); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("same key for another agent: %v", err)
	}
}

func TestReserveIsAtomicUnderConcurrency(t *testing.T) {
	store, _, taskID := fixture(t, caps(4, nil))
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Reserve(taskID, "coder", fmt.Sprintf("send-%d", i))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	reserved, exceeded := 0, 0
	for err := range results {
		switch {
		case err == nil:
			reserved++
		case errors.Is(err, ErrExceeded):
			exceeded++
		default:
			t.Fatal(err)
		}
	}
	if reserved != 4 || exceeded != 8 {
		t.Fatalf("reserved=%d exceeded=%d", reserved, exceeded)
	}
}

func TestReservationsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.db")
	db := open(t, path)
	configID, err := config.Persist(db, config.Snapshot{Config: caps(2, nil), Instructions: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := task.Store{DB: db}.Create("project-1", "budget test task", configID, strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{DB: db}).Reserve(created.ID, "coder", "send-1"); err != nil {
		t.Fatal(err)
	}
	// The daemon crashes before the prompt is sent: the turn stays
	// consumed in a new process.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store := Store{DB: open(t, path)}
	if turns, err := store.Turns(created.ID); err != nil || turns.Task != 1 {
		t.Fatalf("turns after restart %+v %v", turns, err)
	}
	if again, err := store.Reserve(created.ID, "coder", "send-1"); err != nil || !again.Replayed {
		t.Fatalf("retry after restart: %+v %v", again, err)
	}
	if _, err := store.Reserve(created.ID, "coder", "send-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve(created.ID, "coder", "send-3"); !errors.Is(err, ErrExceeded) {
		t.Fatalf("third turn after restart: %v", err)
	}
}

func TestReserveRefusesUnknownTasksAndMissingFields(t *testing.T) {
	store, _, taskID := fixture(t, caps(2, nil))
	if _, err := store.Reserve("missing", "coder", "k"); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
	if _, err := store.Reserve(taskID, "", "k"); err == nil {
		t.Fatal("a reservation without agent was accepted")
	}
	if _, err := store.Reserve(taskID, "coder", ""); err == nil {
		t.Fatal("a reservation without key was accepted")
	}
}
