// Package state records per-activity sync progress so a re-run is idempotent:
// an activity is marked synced only after fitsel answers 202, and the record is
// persisted atomically so a crash mid-run resumes on the next run.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// State holds which activity IDs have been synced.
type State struct {
	synced map[string]bool
	path   string
}

// Load reads the state file, returning an empty state if it does not exist.
func Load(path string) (*State, error) {
	s := &State{synced: map[string]bool{}, path: path}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s.synced); err != nil {
		return nil, err
	}
	return s, nil
}

// Done reports whether the activity has already been synced.
func (s *State) Done(id string) bool { return s.synced[id] }

// MarkDone records that the activity is synced.
func (s *State) MarkDone(id string) { s.synced[id] = true }

// Save persists the state atomically (temp file + rename) so a crash mid-write
// cannot corrupt it.
func (s *State) Save() error {
	if s.path == "" {
		return nil
	}
	dir := filepath.Dir(s.path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.synced, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
