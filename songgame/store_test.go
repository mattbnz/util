package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTripPlayersAndDurations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Populate a game, persist it.
	src := NewGame()
	src.AddOrUpdatePlayer("a", "Alice")
	src.AddOrUpdatePlayer("b", "Bob")
	src.Players["a"].Score = 3
	src.Players["b"].Score = 1
	src.Number = 5
	src.SetDurations(45*time.Second, 10*time.Second)

	store := NewStore(path, src)
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written: %v", err)
	}

	// Load into a fresh game.
	dst := NewGame()
	if err := NewStore(path, dst).Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if dst.Players["a"] == nil || dst.Players["a"].Score != 3 || dst.Players["a"].Name != "Alice" {
		t.Errorf("alice not restored correctly: %+v", dst.Players["a"])
	}
	if dst.Players["b"] == nil || dst.Players["b"].Score != 1 {
		t.Errorf("bob not restored correctly: %+v", dst.Players["b"])
	}
	if dst.Number != 5 {
		t.Errorf("round number: got %d, want 5", dst.Number)
	}
	g, r := dst.Durations()
	if g != 45*time.Second || r != 10*time.Second {
		t.Errorf("durations: got grace=%v results=%v, want 45s/10s", g, r)
	}
}

func TestStoreLoadMissingFileIsNotError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")
	store := NewStore(path, NewGame())
	if err := store.Load(); err != nil {
		t.Fatalf("Load should ignore missing file: %v", err)
	}
}

func TestSetDurationsClampsOutOfRange(t *testing.T) {
	g := NewGame()
	g.SetDurations(1*time.Second, 9999*time.Second)
	grace, results := g.Durations()
	if grace != minDuration {
		t.Errorf("grace below min should be clamped to %v, got %v", minDuration, grace)
	}
	if results != maxDuration {
		t.Errorf("results above max should be clamped to %v, got %v", maxDuration, results)
	}
}

func TestOnChangeFiresForPlayerAdd(t *testing.T) {
	g := NewGame()
	fired := make(chan struct{}, 1)
	g.SetChangeCallback(func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	g.AddOrUpdatePlayer("a", "Alice")
	select {
	case <-fired:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("onChange was not invoked for AddOrUpdatePlayer")
	}
}
