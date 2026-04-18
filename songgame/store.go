package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Snapshotter is implemented by anything that can be persisted and restored.
// Server implements it to combine game state with the admin token and the
// Spotify refresh token into a single state file.
type Snapshotter interface {
	Snapshot() StateSnapshot
	RestoreState(StateSnapshot)
}

// Store persists state to a JSON file. Writes are debounced via a
// dirty flag that a background loop flushes on an interval.
type Store struct {
	path string
	src  Snapshotter

	mu    sync.Mutex
	dirty atomic.Bool
}

func NewStore(path string, src Snapshotter) *Store {
	return &Store{path: path, src: src}
}

// MarkDirty signals that state has changed since the last save.
func (s *Store) MarkDirty() { s.dirty.Store(true) }

// Load reads the state file (if present) and applies it to the game.
// A missing file is not an error.
func (s *Store) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var snap StateSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	s.src.RestoreState(snap)
	return nil
}

// Save writes the current state atomically (temp file + rename). Mode 0600
// because the file contains a Spotify refresh token.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.src.Snapshot()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Run flushes dirty state to disk on the given interval. Blocks until stop is
// closed, then performs a final flush.
func (s *Store) Run(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if s.dirty.CompareAndSwap(true, false) {
				if err := s.Save(); err != nil {
					log.Printf("state save: %v", err)
					s.dirty.Store(true)
				}
			}
		case <-stop:
			if s.dirty.CompareAndSwap(true, false) {
				if err := s.Save(); err != nil {
					log.Printf("state save (final): %v", err)
				}
			}
			return
		}
	}
}
