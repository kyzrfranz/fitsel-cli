// Package state records per-activity sync progress so a re-run is idempotent:
// an activity is marked synced only after fitsel answers 202, and the record is
// persisted atomically so a crash mid-run resumes on the next run.
//
// It also records how many times each activity has failed to sync and stops
// retrying one that keeps failing (per #132): an activity that has failed as
// many times as SkipThreshold is marked skipped and left alone until unskipped.
// Both the synced set and the failure counts are persisted together, so the
// skip decision survives across runs.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// stateDir is the directory the default state file lives in, mirroring the
// config file's home-dir default. It is the single place the default path is
// defined, so both the sync -state flag's documented default and doctor's
// "state present" check resolve to the same file.
func stateDir() string {
	if p := os.Getenv("FITSEL_STATE"); p != "" {
		return filepath.Dir(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "garmin-sync")
	}
	return filepath.Join(home, ".config", "garmin-sync")
}

// Path returns the location of the default state file, honoring $FITSEL_STATE
// and, unset, the home-dir default. A caller can stat it to tell whether a
// state file exists before loading it: Load("") returns success for a missing
// file (an empty state), so presence must be checked on the path, not inferred
// from a successful Load.
func Path() string {
	if p := os.Getenv("FITSEL_STATE"); p != "" {
		return p
	}
	return filepath.Join(stateDir(), "state.json")
}

// State holds which activity IDs have been synced and how many times each has
// failed, so a re-run is idempotent and stops chasing one that keeps failing.
type State struct {
	synced map[string]bool
	fails  map[string]int
	skips  map[string]bool
	path   string

	// SkipThreshold is how many failures before an activity is skipped. It is
	// not persisted: it is a policy a run carries (a flag, default 3), not
	// something the state file records. The persisted thing is the per-activity
	// fail count, from which the threshold decides.
	SkipThreshold int
}

// skipThreshold is the default number of failures after which an activity is
// skipped. A run overrides it with --skip-threshold.
const skipThreshold = 3

// Load reads the state file, returning an empty state if it does not exist.
//
// An empty path is not "no state": it resolves to the default location, so a
// run that never passes --state still reads and, on Save, writes the documented
// ~/.config/garmin-sync/state.json. That is what makes repeated runs idempotent
// by default (per #127): before this, Load("") returned a state whose Save() was
// a no-op, so the state file was never written and every run re-uploaded.
func Load(path string) (*State, error) {
	if path == "" {
		path = Path()
	}
	s := &State{synced: map[string]bool{}, fails: map[string]int{}, skips: map[string]bool{}, path: path, SkipThreshold: skipThreshold}
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
	// The state file is a single object holding the synced set, the per-activity
	// failure counts, and the skip set, so all three persist and reload
	// together. The skip set is the actual decision (which activities to leave
	// alone); the count is what drives it. Persisting both means a changed
	// --skip-threshold between runs still restores the right skips.
	var data struct {
		Synced map[string]bool `json:"synced"`
		Fails  map[string]int  `json:"fails"`
		Skips  map[string]bool `json:"skips"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	s.synced = data.Synced
	s.fails = data.Fails
	s.skips = data.Skips
	return s, nil
}

// Done reports whether the activity has already been synced.
func (s *State) Done(id string) bool { return s.synced[id] }

// MarkDone records that the activity is synced.
func (s *State) MarkDone(id string) { s.synced[id] = true }

// Fail records that the activity failed to sync this run, returning whether it
// has now reached SkipThreshold and must be skipped. A synced activity is not
// counted: once synced it is out of the pending set, so its count never matters.
// A SkipThreshold of 0 disables the skip: the activity is never skipped.
func (s *State) Fail(id string) bool {
	if s.synced[id] {
		return false
	}
	if s.SkipThreshold <= 0 {
		// The skip is disabled: still record the failure count (it is
		// persisted and reported), but never mark the activity skipped.
		s.fails[id]++
		return false
	}
	s.fails[id]++
	if s.fails[id] >= s.SkipThreshold {
		s.skips[id] = true
		return true
	}
	return false
}

// Skipped reports whether the activity is marked skipped (has failed
// SkipThreshold times and is no longer retried until unskipped).
func (s *State) Skipped(id string) bool { return s.skips[id] }

// Unskip clears the skip so the activity is retried again. It never clears the
// fail count, so a still-broken activity is skipped again immediately.
func (s *State) Unskip(id string) { delete(s.skips, id) }

// Skips returns the ids of the activities currently marked skipped, so a run can
// list them in its summary. The order is not significant.
func (s *State) Skips() []string {
	out := make([]string, 0, len(s.skips))
	for id := range s.skips {
		out = append(out, id)
	}
	return out
}

// FailCount returns how many times the activity has failed to sync. It is
// persisted with the rest of the state, so a test (or the summary) can read the
// count that drove the skip.
func (s *State) FailCount(id string) int { return s.fails[id] }

// Save persists the state atomically (temp file + rename) so a crash mid-write
// cannot corrupt it. It writes both the synced set and the failure counts so
// neither is lost across runs.
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
	data := struct {
		Synced map[string]bool `json:"synced"`
		Fails  map[string]int  `json:"fails"`
		Skips  map[string]bool `json:"skips"`
	}{Synced: s.synced, Fails: s.fails, Skips: s.skips}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
