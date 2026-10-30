// Package state records per-activity sync progress so a re-run is idempotent:
// an activity is marked synced only after fitsel answers 202, and the record is
// persisted atomically so a crash mid-run resumes on the next run.
//
// It also records how many times each activity has failed to sync and stops
// retrying one that keeps failing (per #132): an activity that has failed as
// many times as SkipThreshold is marked skipped and left alone until unskipped.
// Both the synced set and the failure counts are persisted together, so the
// skip decision survives across runs.
//
// Each synced activity also carries the metadata status reports offline — its
// name and Garmin's start time (see SyncMeta) — and each successful run records
// a run-level last-synced time, so `status` can report "last synced" and the
// newest synced activity without touching the network.
package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
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
		return "fitsel-client"
	}
	return filepath.Join(home, ".config", "fitsel-client")
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

// SyncMeta is the per-activity metadata status reports offline: the activity's
// name and Garmin's start time (startTimeGMT, verbatim — status displays it,
// no reformatting). It is persisted alongside the synced key so status can pick
// the newest synced activity by date and show its name without the network.
type SyncMeta struct {
	Name  string `json:"name"`
	Start string `json:"start"`
}

// StateVersion is the on-disk format this package writes. It is bumped when the
// shape changes so Load can tell a pre-#127 plain-ID-map file from a versioned
// one (see Load).
const StateVersion = 1

// State holds which activity IDs have been synced and how many times each has
// failed, so a re-run is idempotent and stops chasing one that keeps failing.
type State struct {
	synced map[string]SyncMeta
	fails  map[string]int
	skips  map[string]bool
	path   string

	// SkipThreshold is how many failures before an activity is skipped. It is
	// not persisted: it is a policy a run carries (a flag, default 3), not
	// something the state file records. The persisted thing is the per-activity
	// fail count, from which the threshold decides.
	SkipThreshold int

	// lastSyncedAt is the run-level timestamp of the most recent successful
	// run, stamped on the first MarkDone of the run. It is not persisted in
	// the struct (it is a derived value), but it is persisted to the file as
	// lastSyncedAt so status can report it offline on the next run.
	lastSyncedAt time.Time

	// start is the sync point the tool lists from on a later --since-less run:
	// the start date of the newest activity synced so far. It is "" when
	// unknown (a brand-new state, or a pre-#127 file that migrated in place
	// with no known point), which run() treats exactly like a first run.
	start string

	// dirty is set when a loaded file still holds the pre-#127 plain-ID-map
	// shape; the next Save rewrites it in the versioned form once, then clears
	// the flag so a migrated file stays migrated on disk.
	dirty bool
}

// skipThreshold is the default number of failures after which an activity is
// skipped. A run overrides it with --skip-threshold.
const skipThreshold = 3

// Load reads the state file, returning an empty state if it does not exist.
//
// An empty path is not "no state": it resolves to the default location, so a
// run that never passes --state still reads and, on Save, writes the documented
// ~/.config/fitsel-client/state.json. That is what makes repeated runs idempotent
// by default (per #127): before this, Load("") returned a state whose Save() was
// a no-op, so the state file was never written and every run re-uploaded.
//
// The synced entries carry their metadata (name + start) in the new shape. An
// old file written before the metadata change holds bare booleans
// ({"1001": true}); Load accepts both shapes, treating a bare boolean as an
// entry with no name or start (still counted toward the total, just with no
// name/date to display). This is done in one place so the core idempotency
// logic never sees the raw shape.
func Load(path string) (*State, error) {
	if path == "" {
		path = Path()
	}
	s := &State{synced: map[string]SyncMeta{}, fails: map[string]int{}, skips: map[string]bool{}, path: path, SkipThreshold: skipThreshold}
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
	// The state file is a single object holding the synced set (with metadata),
	// the per-activity failure counts, and the skip set, so all persist and
	// reload together. The skip set is the actual decision (which activities to
	// leave alone); the count is what drives it. Persisting both means a changed
	// --skip-threshold between runs still restores the right skips.
	//
	// It also carries the sync point (start_time) so a later --since-less run
	// can list from it instead of re-listing all history, and a version so a
	// pre-#127 plain-ID-map file can be told from a versioned one.
	var data struct {
		Version      *int                       `json:"version"`
		StartTime    string                     `json:"start_time"`
		Synced       map[string]json.RawMessage `json:"synced"`
		Fails        map[string]int             `json:"fails"`
		Skips        map[string]bool            `json:"skips"`
		LastSyncedAt string                     `json:"lastSyncedAt"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	// A file with no "version" is the pre-#127 plain-ID-map shape. Keep its
	// synced/fails/skips maps but leave the sync point unknown ("") and mark
	// the state dirty so the next Save rewrites it in the versioned form.
	// Otherwise read version + start_time.
	if data.Version == nil {
		s.dirty = true
	} else {
		s.start = data.StartTime
	}
	// Each synced entry is either the new object shape ({"name":..,"start":..})
	// or the old bare boolean (true). A bare boolean cannot unmarshal into
	// SyncMeta, so treat it as the zero value (no name, no start) — still
	// counted toward the total. Anything else is a malformed file and errors.
	s.synced = make(map[string]SyncMeta, len(data.Synced))
	for id, raw := range data.Synced {
		trimmed := bytes.TrimSpace(raw)
		if bytes.Equal(trimmed, []byte("true")) || bytes.Equal(trimmed, []byte("false")) {
			s.synced[id] = SyncMeta{} // old shape: no metadata to display
			continue
		}
		var meta SyncMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, err
		}
		s.synced[id] = meta
	}
	s.fails = data.Fails
	s.skips = data.Skips
	if data.LastSyncedAt != "" {
		if t, err := time.Parse(time.RFC3339, data.LastSyncedAt); err == nil {
			s.lastSyncedAt = t
		}
	}
	return s, nil
}

// Done reports whether the activity has already been synced.
func (s *State) Done(id string) bool { _, ok := s.synced[id]; return ok }

// MarkDone records that the activity is synced, storing its name and Garmin's
// start time so status can report the newest synced activity by date. The first
// MarkDone of a run also stamps the run-level last-synced time.
func (s *State) MarkDone(id string, name, start string) {
	if _, ok := s.synced[id]; !ok {
		// The first activity synced in this run stamps the run-level time, so
		// "last synced" reflects the most recent successful run, not the first
		// ever.
		if s.lastSyncedAt.IsZero() {
			s.lastSyncedAt = time.Now().UTC()
		}
	}
	s.synced[id] = SyncMeta{Name: name, Start: start}
}

// Fail records that the activity failed to sync this run, returning whether it
// has now reached SkipThreshold and must be skipped. A synced activity is not
// counted: once synced it is out of the pending set, so its count never matters.
// A SkipThreshold of 0 disables the skip: the activity is never skipped.
func (s *State) Fail(id string) bool {
	if _, ok := s.synced[id]; ok {
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

// SyncedCount returns how many activities have been synced so far. status
// reports it as "activities synced so far".
func (s *State) SyncedCount() int { return len(s.synced) }

// LastSyncedAt returns the run-level timestamp of the most recent successful
// run, or the zero time if nothing has ever synced. status reports the zero
// time as "never synced yet".
func (s *State) LastSyncedAt() time.Time { return s.lastSyncedAt }

// NewestSynced returns the synced activity with the latest start time, and its
// id, so status can report "newest synced activity by name and date". Entries
// with no recorded start sort last. It returns an empty id when nothing is
// synced or no entry has a recorded start (the old bool shape).
func (s *State) NewestSynced() (id string, meta SyncMeta) {
	for k, m := range s.synced {
		if m.Start == "" {
			continue
		}
		if id == "" || m.Start > meta.Start {
			id, meta = k, m
		}
	}
	return id, meta
}

// StartTime returns the stored sync point: the start date of the newest
// activity synced so far, as a YYYY-MM-DD string, or "" when unknown. run()
// treats "" exactly like a first run — without --since it stops with the
// original message rather than listing all history since the start of time.
func (s *State) StartTime() string { return s.start }

// AdvanceStartIfLater locks the sync point to d (a YYYY-MM-DD date) only when d
// is later than the current non-empty point — so a run never lowers it. It is
// called after a real sync of at least one activity, with the newest activity's
// start date seen this run (List returns newest first, so that is acts[0]).
func (s *State) AdvanceStartIfLater(d string) {
	if d == "" || s.start == "" {
		return
	}
	if d > s.start {
		s.start = d
	}
}

// Save persists the state atomically (temp file + rename) so a crash mid-write
// cannot corrupt it. It writes the synced set (with metadata), the failure
// counts, the skip set, and the run-level last-synced time, so none is lost
// across runs.
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
		Version      *int                `json:"version"`
		StartTime    string              `json:"start_time"`
		Synced       map[string]SyncMeta `json:"synced"`
		Fails        map[string]int      `json:"fails"`
		Skips        map[string]bool     `json:"skips"`
		LastSyncedAt string              `json:"lastSyncedAt"`
	}{
		Version:   intPtr(StateVersion),
		StartTime: s.start,
		Synced:    s.synced,
		Fails:     s.fails,
		Skips:     s.skips,
	}
	if !s.lastSyncedAt.IsZero() {
		data.LastSyncedAt = s.lastSyncedAt.Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	// A file that migrated in place (pre-#127 shape) is now rewritten in the
	// versioned form; clear the flag so a later Save does not rewrite it again.
	s.dirty = false
	return nil
}

// intPtr returns a pointer to v, so a version field is always written (never
// omitted) in the versioned state file.
func intPtr(v int) *int { return &v }
