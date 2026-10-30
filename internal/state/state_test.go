package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIdempotentMarkAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Done("1001") {
		t.Fatal("fresh state should not have 1001")
	}
	s.MarkDone("1001", "Morning Run", "2026-10-20 07:00:00")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	// Reload: 1001 should now be synced.
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Done("1001") {
		t.Fatal("after reload, 1001 should be synced")
	}
}

func TestEmptyPathNoError(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if s.Done("1") {
		t.Fatal("empty state should have nothing synced")
	}
	if s.SkipThreshold != 3 {
		t.Fatalf("default skip threshold = %d, want 3", s.SkipThreshold)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("saving empty state should be a no-op, got %v", err)
	}
}

// TestFailCountPersistsAcrossRuns proves the per-activity failure count is
// persisted with the rest of the state: a count reached in one state is still
// there after reload, so a run continues counting toward the skip rather than
// restarting at zero.
func TestFailCountPersistsAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Two failures so far.
	s.Fail("1001")
	s.Fail("1001")
	if s.Skipped("1001") {
		t.Fatal("2 failures should not skip at threshold 3")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	// Reload: the count must still be 2, and the third failure skips.
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.fails["1001"] != 2 {
		t.Fatalf("persisted fail count = %d, want 2", s2.fails["1001"])
	}
	if !s2.Fail("1001") {
		t.Fatal("third failure should skip at threshold 3")
	}
	if !s2.Skipped("1001") {
		t.Fatal("1001 should be skipped after threshold failures")
	}
}

// TestSkipReachedAtThreshold proves an activity is skipped exactly when its
// fail count reaches the threshold, not before.
func TestSkipReachedAtThreshold(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 3; i++ {
		if s.Fail("1001") {
			t.Fatalf("failure %d should not skip at threshold 3", i)
		}
		if s.Skipped("1001") {
			t.Fatalf("failure %d should not mark skipped at threshold 3", i)
		}
	}
	if !s.Fail("1001") {
		t.Fatal("third failure should skip at threshold 3")
	}
	if !s.Skipped("1001") {
		t.Fatal("1001 should be skipped at threshold 3")
	}
}

// TestSkippedActivityNotCountedOnceSynced proves a synced activity stops being
// counted: its fail count is frozen, so a later Fail does not skip it (it is no
// longer pending, so this path is defensive).
func TestSkippedActivityNotCountedOnceSynced(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	s.MarkDone("1001", "Morning Run", "2026-10-20 07:00:00")
	if s.Fail("1001") {
		t.Fatal("a synced activity should not accrue fail counts")
	}
}

// TestUnskipRestartsRetry proves unskipping clears the skip so an activity is
// retried again, while leaving its fail count intact (a still-broken activity
// is skipped again immediately on the next failure).
func TestUnskipRestartsRetry(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	s.Fail("1001")
	s.Fail("1001")
	s.Fail("1001")
	if !s.Skipped("1001") {
		t.Fatal("1001 should be skipped at threshold 3")
	}
	s.Unskip("1001")
	if s.Skipped("1001") {
		t.Fatal("1001 should no longer be skipped after unskip")
	}
	// The fail count is preserved, so the next failure skips again.
	if !s.Fail("1001") {
		t.Fatal("a failure after unskip should skip again (count preserved)")
	}
}

// TestSkipThresholdOverride proves the threshold is a policy a run carries, not
// something the state file records: a custom threshold is honoured on load.
func TestSkipThresholdOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SkipThreshold = 2
	s.Fail("1001")
	if s.Skipped("1001") {
		t.Fatal("1 failure should not skip at threshold 2")
	}
	if !s.Fail("1001") {
		t.Fatal("2 failures should skip at threshold 2")
	}
}

// TestSkipsListsSkipped proves Skips returns the ids of the activities currently
// marked skipped.
func TestSkipsListsSkipped(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	s.Fail("1001")
	s.Fail("1001")
	s.Fail("1001")
	s.Fail("2002")
	s.Fail("2002")
	s.Fail("2002")
	got := map[string]bool{}
	for _, id := range s.Skips() {
		got[id] = true
	}
	if !got["1001"] || !got["2002"] {
		t.Fatalf("Skips() = %v, want both 1001 and 2002", s.Skips())
	}
	if len(got) != 2 {
		t.Fatalf("Skips() = %v, want exactly 2 entries", s.Skips())
	}
}

// TestMarkDoneStoresMetadataAndLastSynced proves the new shape: MarkDone records
// the activity's name and start, and the first MarkDone of a run stamps the
// run-level last-synced time. status reports these offline.
func TestMarkDoneStoresMetadataAndLastSynced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.lastSyncedAt.IsZero() {
		t.Fatal("fresh state should have no last-synced time")
	}
	s.MarkDone("1001", "Morning Run", "2026-10-20 07:00:00")
	s.MarkDone("1002", "Evening Ride", "2026-10-25 18:30:00")

	id, meta := s.NewestSynced()
	if id != "1002" {
		t.Fatalf("newest synced = %s, want 1002 (latest start)", id)
	}
	if meta.Name != "Evening Ride" {
		t.Fatalf("newest name = %q, want Evening Ride", meta.Name)
	}
	if meta.Start != "2026-10-25 18:30:00" {
		t.Fatalf("newest start = %q, want 2026-10-25 18:30:00", meta.Start)
	}
	if s.LastSyncedAt().IsZero() {
		t.Fatal("after the first MarkDone, last-synced time should be set")
	}
	if got := s.SyncedCount(); got != 2 {
		t.Fatalf("SyncedCount() = %d, want 2", got)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
}

// TestLastSyncedStampsOncePerRun proves the run-level last-synced time is
// stamped once per run (on the first MarkDone), not on every activity.
func TestLastSyncedStampsOncePerRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.MarkDone("1001", "Morning Run", "2026-10-20 07:00:00")
	first := s.LastSyncedAt()
	time.Sleep(2 * time.Millisecond)
	s.MarkDone("1002", "Evening Ride", "2026-10-25 18:30:00")
	if !s.LastSyncedAt().Equal(first) {
		t.Fatal("last-synced time should be stamped once per run, not per activity")
	}
}

// TestBackwardCompatOldBoolShape proves an old state file with bare-boolean
// synced entries ({"1001": true}) still loads and counts toward the total,
// just with no name/date to display.
func TestBackwardCompatOldBoolShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	const old = `{"synced":{"1001":true,"1002":true},"fails":{},"skips":{}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("old bool shape should load without error: %v", err)
	}
	if !s.Done("1001") || !s.Done("1002") {
		t.Fatal("old bool entries should count as done")
	}
	if got := s.SyncedCount(); got != 2 {
		t.Fatalf("SyncedCount() = %d, want 2", got)
	}
	// No start recorded, so NewestSynced returns no name/date to display.
	id, meta := s.NewestSynced()
	if id != "" {
		t.Fatalf("old shape has no start, so newest id should be empty, got %q", id)
	}
	if meta.Name != "" || meta.Start != "" {
		t.Fatalf("old shape has no metadata, got %+v", meta)
	}
}

// TestNewestSyncedNoStartSortsLast proves an activity with no recorded start is
// never reported as newest over one that has a date.
func TestNewestSyncedNoStartSortsLast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	const old = `{"synced":{"1001":{"name":"No Start","start":""},"1002":{"name":"With Start","start":"2026-10-25 18:30:00"}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := Load(path)
	id, meta := s.NewestSynced()
	if id != "1002" {
		t.Fatalf("newest = %q (%s), want 1002 (With Start)", id, meta.Name)
	}
}
