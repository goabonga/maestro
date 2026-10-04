// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Chris <goabonga@pm.me>

package scheduler

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestNewCapacityValidatesCeilings(t *testing.T) {
	if _, err := NewCapacity(0, 1, 3); err == nil || !strings.Contains(err.Error(), "at least 1") {
		t.Fatalf("error %v", err)
	}
	if _, err := NewCapacity(3, 1, 3); err != nil {
		t.Fatal(err)
	}
}

func TestReserveStopsAtTheCeiling(t *testing.T) {
	capacity, err := NewCapacity(2, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	first, err := capacity.Reserve(Sessions, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capacity.Reserve(Sessions, "p2"); err != nil {
		t.Fatal(err)
	}
	// The third session is refused whatever the project: no
	// overallocation across projects.
	if _, err := capacity.Reserve(Sessions, "p3"); !errors.Is(err, ErrFull) {
		t.Fatalf("expected ErrFull, got %v", err)
	}
	// Other kinds have their own ceiling.
	if _, err := capacity.Reserve(Tests, "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := capacity.Reserve(Tests, "p1"); !errors.Is(err, ErrFull) {
		t.Fatalf("expected ErrFull, got %v", err)
	}

	first.Release()
	if _, err := capacity.Reserve(Sessions, "p3"); err != nil {
		t.Fatalf("release did not free the slot: %v", err)
	}
	// Releasing twice frees nothing further.
	first.Release()
	if _, err := capacity.Reserve(Sessions, "p4"); !errors.Is(err, ErrFull) {
		t.Fatalf("double release freed a slot: %v", err)
	}
}

func TestReserveRejectsAnUnknownKind(t *testing.T) {
	capacity, err := NewCapacity(1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capacity.Reserve(Kind("gpus"), "p1"); err == nil || !strings.Contains(err.Error(), "unknown capacity kind") {
		t.Fatalf("error %v", err)
	}
}

func TestUsageReportsTotalsAndPerProjectDetail(t *testing.T) {
	capacity, err := NewCapacity(3, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capacity.Reserve(Sessions, "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := capacity.Reserve(Sessions, "p1"); err != nil {
		t.Fatal(err)
	}
	slot, err := capacity.Reserve(Commands, "p2")
	if err != nil {
		t.Fatal(err)
	}

	usage := capacity.Usage()
	if usage[Sessions].Used != 2 || usage[Sessions].Limit != 3 || usage[Sessions].ByProject["p1"] != 2 {
		t.Fatalf("sessions usage %+v", usage[Sessions])
	}
	if usage[Commands].Used != 1 || usage[Commands].ByProject["p2"] != 1 {
		t.Fatalf("commands usage %+v", usage[Commands])
	}
	if usage[Tests].Used != 0 || usage[Tests].Limit != 1 {
		t.Fatalf("tests usage %+v", usage[Tests])
	}

	slot.Release()
	if usage := capacity.Usage(); usage[Commands].Used != 0 || len(usage[Commands].ByProject) != 0 {
		t.Fatalf("commands usage after release %+v", usage[Commands])
	}
}

func TestReserveIsAtomicUnderContention(t *testing.T) {
	capacity, err := NewCapacity(5, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	granted := make(chan *Slot, 64)
	for worker := 0; worker < 64; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if slot, err := capacity.Reserve(Sessions, "p1"); err == nil {
				granted <- slot
			}
		}()
	}
	wg.Wait()
	close(granted)
	if len(granted) != 5 {
		t.Fatalf("granted %d slots for a ceiling of 5", len(granted))
	}
}
