package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"garmin-sync/internal/fakes"
	"garmin-sync/internal/state"
)

func newActivities() []fakes.FakeActivity {
	return []fakes.FakeActivity{
		{ID: 1001, Name: "Morning Run", StartTimeGMT: "2026-10-20 07:00:00"},
		{ID: 1002, Name: "Evening Ride", StartTimeGMT: "2026-10-25 18:30:00"},
		{ID: 1003, Name: "Weekend Hike", StartTimeGMT: "2026-10-27 09:15:00"},
	}
}

// TestFirstRunUploadsEverything, SecondRunUploadsNothing is the walking skeleton's
// headline check: against the fakes, a first run uploads every activity since
// --since; a second run straight away uploads nothing.
func TestFirstRunUploadsEverythingSecondRunUploadsNothing(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	gar := fakes.NewFakeGarmin("test-garmin-key", newActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()

	args := []string{
		"-since", "2026-10-01",
		"-state", statePath,
		"-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token",
	}
	// Point the clients at the fakes.
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()

	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")

	if err := run(args); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(fits.Uploadees) != 3 {
		t.Fatalf("expected 3 uploads, got %d", len(fits.Uploadees))
	}

	// Second run straight away: nothing new since --since, everything already
	// marked synced.
	if err := run(args); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(fits.Uploadees) != 3 {
		t.Fatalf("second run uploaded %d extra files; expected nothing", len(fits.Uploadees)-3)
	}
}

// TestDryRunUploadsNothing checks --dry-run lists pending activities but sends
// nothing to fitsel.
func TestDryRunUploadsNothing(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	gar := fakes.NewFakeGarmin("test-garmin-key", newActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()

	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()

	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")

	args := []string{"-since", "2026-10-01", "-dry-run", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}
	if err := run(args); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("dry run uploaded %d files; expected none", len(fits.Uploadees))
	}
	// Under dry-run, state is unchanged: no activity marked synced.
	st, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if st.Done("1001") || st.Done("1002") || st.Done("1003") {
		t.Fatalf("dry-run marked activities synced; expected none")
	}
	// Under dry-run, no file was downloaded from Garmin either: a preview
	// performs no transfers at all, not just no uploads.
	if gar.Downloads != 0 {
		t.Fatalf("dry-run downloaded %d files; expected none", gar.Downloads)
	}
}

// TestResumeAfterInterruptedSync is the task's own check (the reason this story
// exists): a run over several activities, killed after the first few, completes
// on rerun with no errors and every activity present exactly once. Here the
// kill is an error boundary — the first run stops on an upload error after the
// first upload is accepted and marked done — rather than a real signal.
func TestResumeAfterInterruptedSync(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	gar := fakes.NewFakeGarmin("test-garmin-key", newActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	fits.FailOnce = true // kill the run on the second activity's upload
	defer fits.Close()

	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()

	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")

	args := []string{
		"-since", "2026-10-01",
		"-state", statePath,
		"-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token",
	}

	// First run: 1001 uploads (202, marked done, saved), then 1002's upload
	// fails and the run stops with an error — the "kill".
	if err := run(args); err == nil {
		t.Fatal("first run: expected an upload error, got nil")
	}

	// A clean rerun finishes the rest.
	if err := run(args); err != nil {
		t.Fatalf("resume run: %v", err)
	}

	// Every activity uploaded exactly once — fitsel dedupes by content, so a
	// resend would show a second upload of the same bytes.
	for _, id := range []string{"1001", "1002", "1003"} {
		if got := fits.Count([]byte("FIT-DATA-" + id)); got != 1 {
			t.Fatalf("activity %s uploaded %d times; want exactly once", id, got)
		}
	}
	if len(fits.Uploadees) != 3 {
		t.Fatalf("expected 3 uploads total, got %d", len(fits.Uploadees))
	}

	// State persisted across the two runs: all three are marked synced.
	st, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	for _, id := range []string{"1001", "1002", "1003"} {
		if !st.Done(id) {
			t.Fatalf("activity %s not marked synced after resume", id)
		}
	}
}

// TestBadKeyExitsGarmin proves a rejected Garmin key maps to exitGarmin (3),
// not the generic error code (per #84). The run stops at Authorize, before any
// activity is listed, so nothing is uploaded.
func TestBadKeyExitsGarmin(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	gar := fakes.NewFakeGarmin("test-garmin-key", newActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()

	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()

	t.Setenv("GARMIN_API_KEY", "wrong-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")

	args := []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}
	err := run(args)
	if err == nil {
		t.Fatal("expected a run error on a bad key")
	}
	if got := mapGarminExit(err); got != exitGarmin {
		t.Fatalf("exit code = %d, want %d (bad key)", got, exitGarmin)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("bad key uploaded %d files; expected none", len(fits.Uploadees))
	}
}

// TestNonGarminErrorStaysGeneral proves the generic error path is untouched: a
// state-load failure maps to exitGeneral (1), not the Garmin codes.
func TestNonGarminErrorStaysGeneral(t *testing.T) {
	if got := mapGarminExit(errors.New("some other failure")); got != exitGeneral {
		t.Fatalf("exit code = %d, want %d (general)", got, exitGeneral)
	}
}

// TestErrorPrintedOnStderr proves main()'s error path still prints the one-line
// `error:` message the task asks for (per #84). The exit-code tests call
// mapGarminExit directly and never see main()'s stderr, so this exercises the
// print-and-exit contract directly: it must print `error: <msg>` and exit with
// the mapped code. A swallowed error print would be a silent failure an athlete
// can't act on. exitError takes its exit function as an argument, so we can
// pass a fake one and read stderr without os.Exit killing the test process.
func TestErrorPrintedOnStderr(t *testing.T) {
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	calledWith := -1
	exitError(errors.New("boom"), func(code int) { calledWith = code })

	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	if !strings.Contains(buf.String(), "error: boom") {
		t.Fatalf("expected an 'error: boom' line on stderr, got %q", buf.String())
	}
	if calledWith != exitGeneral {
		t.Fatalf("exit code = %d, want %d (general)", calledWith, exitGeneral)
	}
}
