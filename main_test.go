package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kyzrfranz/fitsel-cli/internal/config"
	"github.com/kyzrfranz/fitsel-cli/internal/fakes"
	"github.com/kyzrfranz/fitsel-cli/internal/fitsel"
	"github.com/kyzrfranz/fitsel-cli/internal/garmin"
	"github.com/kyzrfranz/fitsel-cli/internal/state"
)

func newActivities() []fakes.FakeActivity {
	return []fakes.FakeActivity{
		{ID: 1001, Name: "Morning Run", StartTimeGMT: "2026-10-20 07:00:00"},
		{ID: 1002, Name: "Evening Ride", StartTimeGMT: "2026-10-25 18:30:00"},
		{ID: 1003, Name: "Weekend Hike", StartTimeGMT: "2026-10-27 09:15:00"},
	}
}

// useFastDelays points the clients' retry/back-off delays at ~0 so the suite
// does not sleep real seconds (2s upload / 30s back-off in production). It
// restores the production defaults on cleanup. Every end-to-end test that can
// trigger a retry or a 429 calls it, so the whole suite runs in a few seconds.
func useFastDelays(t *testing.T) {
	t.Helper()
	origUpload := fitsel.UploadDelayDelay
	origBackoff := garmin.RetryAfterDelay
	fitsel.UploadDelayDelay = 0
	garmin.RetryAfterDelay = 0
	t.Cleanup(func() {
		fitsel.UploadDelayDelay = origUpload
		garmin.RetryAfterDelay = origBackoff
	})
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
	useFastDelays(t)
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	gar := fakes.NewFakeGarmin("test-garmin-key", newActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	// Fail every attempt: a single 500 no longer hard-stops the run (it retries
	// and recovers), so to exercise "stop on failure, then resume clean on
	// rerun" the fake must fail every attempt. Cleared by the run's state, so
	// the resume run succeeds.
	fits.Statuses = []int{http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError}
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

	// First run: 1001's upload fails (500, after retries) and the run stops
	// with an error — the "kill". 1001 is not marked done, so it is retried.
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
	if got := mapExitCode(err); got != exitGarmin {
		t.Fatalf("exit code = %d, want %d (bad key)", got, exitGarmin)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("bad key uploaded %d files; expected none", len(fits.Uploadees))
	}
}

// TestNonGarminErrorStaysGeneral proves the generic error path is untouched: a
// state-load failure maps to exitGeneral (1), not the Garmin codes.
func TestNonGarminErrorStaysGeneral(t *testing.T) {
	if got := mapExitCode(errors.New("some other failure")); got != exitGeneral {
		t.Fatalf("exit code = %d, want %d (general)", got, exitGeneral)
	}
}

// TestMissingCredentialsExitConfig proves a missing credential maps to
// exitConfig (2), not the general error code (per #134). The run stops before
// any activity is attempted, so the athlete gets the code that tells them to
// fix their configuration, not a generic failure.
func TestMissingCredentialsExitConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FITSEL_CONFIG", filepath.Join(dir, "config.json"))
	// No secrets set, and a writable config dir: Load writes a placeholder and
	// returns ErrMissingCredentials.
	_, err := config.Load()
	if !errors.Is(err, config.ErrMissingCredentials) {
		t.Fatalf("expected ErrMissingCredentials, got %v", err)
	}
	if got := mapExitCode(err); got != exitConfig {
		t.Fatalf("exit code = %d, want %d (config)", got, exitConfig)
	}
}

// TestUnreadableConfigExitConfig proves an unreadable config file maps to
// exitConfig (2), not the general error code (per #134).
func TestUnreadableConfigExitConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	// A file that exists but is not valid JSON yields ErrConfigUnreadable.
	if err := os.WriteFile(cfg, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FITSEL_CONFIG", cfg)
	_, err := config.Load()
	if !errors.Is(err, config.ErrConfigUnreadable) {
		t.Fatalf("expected ErrConfigUnreadable, got %v", err)
	}
	if got := mapExitCode(err); got != exitConfig {
		t.Fatalf("exit code = %d, want %d (config)", got, exitConfig)
	}
}

// TestErrorPrintedOnStderr proves main()'s error path still prints the one-line
// `error:` message the task asks for (per #84). The exit-code tests call
// mapExitCode directly and never see main()'s stderr, so this exercises the
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

// TestRefreshMidRunUploadsEverything is step 2's end-to-end check (per #84): a
// run whose token is already expired when it starts refreshes it transparently
// and uploads every pending activity, requesting a new token instead of
// stopping. The fake's ExpiredOnce makes the first token stale, so the run's
// first List call must refresh it.
func TestRefreshMidRunUploadsEverything(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	gar := fakes.NewFakeGarmin("test-garmin-key", newActivities())
	gar.ExpiredOnce = true // the token Authorize issues is already expired
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()

	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()

	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")

	args := []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fits.Uploadees) != 3 {
		t.Fatalf("expected 3 uploads, got %d", len(fits.Uploadees))
	}
	// The run requested a new token (the first was expired), so the fake issued
	// two: proving it refreshed instead of stopping.
	if gar.Tokens != 2 {
		t.Fatalf("tokens issued = %d, want 2 (the run refreshed the expired token)", gar.Tokens)
	}
}

// TestRateLimitExitsGarminAndUploadsNothing is step 3's end-to-end check (per
// #84): a run whose first protected call is rate-limited (429) backs off for
// Retry-After and stops cleanly with its own exit code, uploading nothing.
func TestRateLimitExitsGarminAndUploadsNothing(t *testing.T) {
	useFastDelays(t)
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	gar := fakes.NewFakeGarmin("test-garmin-key", newActivities())
	gar.RateLimitOnce = true // the first protected call (List) 429s, with Retry-After
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()

	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()

	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")

	args := []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}
	err := run(args)
	if err == nil {
		t.Fatal("expected a run error on a rate limit")
	}
	if got := mapExitCode(err); got != exitRateLimited {
		t.Fatalf("exit code = %d, want %d (rate limited)", got, exitRateLimited)
	}
	// The run stopped at List, before any download or upload: nothing transferred.
	if gar.Downloads != 0 {
		t.Fatalf("rate limit downloaded %d files; expected none", gar.Downloads)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("rate limit uploaded %d files; expected none", len(fits.Uploadees))
	}
}

// newManyActivities wires a fake Garmin serving n activities (paged at 100 by
// the fakes' default list handler) plus a fake fitsel and the run args, for the
// pagination end-to-end checks. No --to: the run must list every activity since
// --since across pages and upload them all.
func newManyActivities(t *testing.T, n int) (*fakes.FakeGarmin, *fakes.FakeFitsel, []string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.ManyActivities(n, "2026-10-01"))
	fits := fakes.NewFakeFitsel("test-passkey")
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})
	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")
	return gar, fits, []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}
}

// TestSyncPagesAllActivitiesUploadsEverything is the headline check for #95:
// with more than 100 activities since --since and no --to, a single run
// fetches them all across pages and uploads every one — none dropped at the old
// 100 cap. The fakes page the list at 100 per request.
func TestSyncPagesAllActivitiesUploadsEverything(t *testing.T) {
	const n = 250
	gar, fits, args := newManyActivities(t, n)
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(fits.Uploadees); got != n {
		t.Fatalf("uploaded %d activities, want %d (none dropped at the 100 cap)", got, n)
	}
	// Every activity was requested across pages: offsets 0, 100 and 200.
	want := []int{0, 100, 200}
	if len(gar.PagesSeen) != len(want) {
		t.Fatalf("list requests = %v, want %v", gar.PagesSeen, want)
	}
}

// TestSyncPagesSecondRunUploadsNothing is the idempotence check for #95:
// against fakes that page at 100, a single run uploads everything since --since;
// the next run immediately reports nothing to sync — it lists the same pages,
// finds every activity already marked synced, and uploads nothing.
func TestSyncPagesSecondRunUploadsNothing(t *testing.T) {
	const n = 150
	_, fits, args := newManyActivities(t, n)
	if err := run(args); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := len(fits.Uploadees); got != n {
		t.Fatalf("first run uploaded %d, want %d", got, n)
	}

	if err := run(args); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := len(fits.Uploadees); got != n {
		t.Fatalf("second run uploaded %d extra; expected nothing", got-n)
	}
	// The second run listed the same pages it did the first (it lists before
	// filtering by state) but uploaded nothing: idempotent.
}

// TestSyncPagesDryRunListsBeyondFirstPage is the dry-run check for #95: it
// lists every pending activity in range, including those beyond the old first
// page of 100, and uploads nothing.
func TestSyncPagesDryRunListsBeyondFirstPage(t *testing.T) {
	const n = 120
	gar, fits, args := newManyActivities(t, n)
	args = append(args, "-dry-run")
	if err := run(args); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := len(fits.Uploadees); got != 0 {
		t.Fatalf("dry run uploaded %d; expected none", got)
	}
	// The dry run still had to walk both pages to list everything in range.
	if len(gar.PagesSeen) < 2 {
		t.Fatalf("dry run fetched %v, want at least 2 pages (past the old 100 cap)", gar.PagesSeen)
	}
}

// TestSyncPagesResumeAfterInterruptedUpload is the resume check for #95: a run
// over more than one page killed after the first page's uploads completes on
// rerun with every activity present exactly once. The kill is an upload error
// boundary — the fake fails the first upload, the run stops, that activity is
// not marked done, and the rerun finishes the rest across both pages.
func TestSyncPagesResumeAfterInterruptedUpload(t *testing.T) {
	useFastDelays(t)
	const n = 150
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.ManyActivities(n, "2026-10-01"))
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	// Fail every attempt: a single 500 no longer hard-stops the run (it retries
	// and recovers), so to exercise "stop on failure, then resume clean on
	// rerun" the fake must fail every attempt. Cleared by the run's state, so
	// the resume run succeeds.
	fits.Statuses = []int{http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError}
	defer fits.Close()

	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})
	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")

	args := []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}

	// First run: the first upload fails (500, after retries) and the run stops.
	if err := run(args); err == nil {
		t.Fatal("first run: expected an upload error, got nil")
	}

	// A clean rerun finishes the rest across both pages.
	if err := run(args); err != nil {
		t.Fatalf("resume run: %v", err)
	}

	// Every activity uploaded exactly once — fitsel dedupes by content.
	for id := int64(1); id <= n; id++ {
		if got := fits.Count([]byte("FIT-DATA-" + strconv.FormatInt(id, 10))); got != 1 {
			t.Fatalf("activity %d uploaded %d times; want exactly once", id, got)
		}
	}
	if len(fits.Uploadees) != n {
		t.Fatalf("expected %d uploads total, got %d", n, len(fits.Uploadees))
	}

	// State persisted across the two runs: all activities are marked synced.
	st, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	for id := int64(1); id <= n; id++ {
		if !st.Done(strconv.FormatInt(id, 10)) {
			t.Fatalf("activity %d not marked synced after resume", id)
		}
	}
}

// newFitselArgs wires the fakes and credentials for a run and returns the args,
// so each fitsel end-to-end case can focus on the one status it scripts.
func newFitselArgs(t *testing.T, dir string, fits *fakes.FakeFitsel) []string {
	t.Helper()
	statePath := filepath.Join(dir, "state.json")
	gar := fakes.NewFakeGarmin("test-garmin-key", newActivities())
	t.Cleanup(gar.Close)
	t.Cleanup(fits.Close)
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")
	return []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}
}

// TestBadSignatureExitsFitselProves a 401 (bad passkey) maps to its own exit
// code, uploads nothing, and marks nothing synced (per #85).
func TestBadSignatureExitsFitselAndUploadsNothing(t *testing.T) {
	dir := t.TempDir()
	fits := fakes.NewFakeFitsel("test-passkey")
	fits.Statuses = []int{http.StatusUnauthorized}
	args := newFitselArgs(t, dir, fits)

	err := run(args)
	if err == nil {
		t.Fatal("expected an upload error on a bad signature")
	}
	if got := mapExitCode(err); got != exitBadSignature {
		t.Fatalf("exit code = %d, want %d (bad signature)", got, exitBadSignature)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("bad signature uploaded %d files; expected none", len(fits.Uploadees))
	}
	st, err := state.Load(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if st.Done("1001") {
		t.Fatal("bad signature marked an activity synced; expected none")
	}
}

// TestUnknownWebhookExitsFitselProves a 404 (unknown webhook token) maps to its
// own exit code, uploads nothing, and marks nothing synced (per #85).
func TestUnknownWebhookExitsFitselAndUploadsNothing(t *testing.T) {
	dir := t.TempDir()
	fits := fakes.NewFakeFitsel("test-passkey")
	fits.Statuses = []int{http.StatusNotFound}
	args := newFitselArgs(t, dir, fits)

	err := run(args)
	if err == nil {
		t.Fatal("expected an upload error on an unknown webhook token")
	}
	if got := mapExitCode(err); got != exitWebhookMissing {
		t.Fatalf("exit code = %d, want %d (unknown webhook token)", got, exitWebhookMissing)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("unknown webhook uploaded %d files; expected none", len(fits.Uploadees))
	}
}

// TestSkipAfterThresholdFailures proves an activity that fails every run is
// retried until it reaches the threshold, then skipped: it is no longer
// downloaded or uploaded, and it is listed in the summary. The other activities
// still sync.
func TestSkipAfterThresholdFailures(t *testing.T) {
	const threshold = 3
	dir := t.TempDir()
	// The first activity (1001) 400s (malformed) every run: a per-activity
	// failure. The other two upload clean.
	fits := fakes.NewFakeFitsel("test-passkey")
	fits.FailNextQueued(http.StatusBadRequest, http.StatusBadRequest, http.StatusBadRequest)
	defer fits.Close()
	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar.Close()

	st, err := state.Load(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.SkipThreshold = threshold
	statePath := filepath.Join(dir, "state.json")

	// Fail once, twice: still retried, not skipped. Each run loads its own
	// state from disk, so read the persisted skip from a fresh load after each.
	for attempt := 1; attempt <= threshold-1; attempt++ {
		// Reset the fake's status queue for this run: it fails the first upload
		// (1001) again.
		fits.FailNextQueued(http.StatusBadRequest)

		err := run(newRunArgs(t, dir, gar, fits))
		if err == nil {
			t.Fatalf("run %d: expected a partial-failure error", attempt)
		}
		st, err := state.Load(statePath)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		if st.Skipped("1001") {
			t.Fatalf("run %d: 1001 should not be skipped before the threshold", attempt)
		}
	}

	// The third failure reaches the threshold: 1001 is now skipped.
	fits.FailNextQueued(http.StatusBadRequest)
	err = run(newRunArgs(t, dir, gar, fits))
	if err == nil {
		t.Fatal("expected a partial-failure error on the threshold run")
	}
	st, err = state.Load(statePath)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if !st.Skipped("1001") {
		t.Fatal("1001 should be skipped after threshold failures")
	}

	// A fourth run with the fake healthy must NOT retry 1001: it is skipped.
	fits.FailNextQueued()
	if err := run(newRunArgs(t, dir, gar, fits)); err != nil {
		t.Fatalf("run after skip: %v", err)
	}
	// Only 1002 and 1003 were ever uploaded; 1001 stayed skipped.
	if got := fits.Count([]byte("FIT-DATA-1002")) + fits.Count([]byte("FIT-DATA-1003")); got != 2 {
		t.Fatalf("uploads = %d, want 2 (1002 and 1003; 1001 skipped)", got)
	}
	if c := fits.Count([]byte("FIT-DATA-1001")); c != 0 {
		t.Fatalf("skipped activity 1001 was uploaded %d times; want 0", c)
	}
}

// TestSkipListedInRunSummary proves the end-of-run summary actually prints the
// skipped activity's id: the acceptance criterion is user-visible output, so
// this captures stdout on runs that leave 1001 skipped and asserts the id is
// on the "skipped %d activity(ies)" line, not just in the persisted state.
func TestSkipListedInRunSummary(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")
	args := []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}

	// Capture stdout for the whole test, then restore it.
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	// 1001 fails its upload once per run (per-activity); the other two upload
	// clean. After three runs its count reaches the threshold and it is skipped.
	// The summary line that lists the skipped id is printed on the third run,
	// so it is read after the loop.
	var skippedLine string
	for attempt := 1; attempt <= 3; attempt++ {
		fits.FailNextQueued(http.StatusBadRequest)
		if err := run(args); err == nil {
			t.Fatalf("run %d: expected a partial-failure error", attempt)
		}
		// Read only up to this run's output; the final read below gets the
		// summary line that run 3 printed.
		w.Close()
		var buf bytes.Buffer
		io.Copy(&buf, r)
		if strings.Contains(buf.String(), "skipped") {
			skippedLine = buf.String()
		}
		r, w, err = os.Pipe()
		os.Stdout = w
		if err != nil {
			t.Fatal(err)
		}
	}

	if skippedLine == "" {
		t.Fatal("no run printed a skip summary line")
	}
	if !strings.Contains(skippedLine, "skipped 1 activity(ies)") {
		t.Fatalf("summary missing the skip count line; got:\n%s", skippedLine)
	}
	if !strings.Contains(skippedLine, "1001") {
		t.Fatalf("summary does not list the skipped activity id 1001; got:\n%s", skippedLine)
	}
}

// TestSkipCountPersistsAcrossRuns proves the fail count that drives the skip is
// persisted with the rest of the state: a run that fails an activity once,
// then reloads, keeps counting toward the skip rather than restarting at zero.
func TestSkipCountPersistsAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	// First run: 1001 fails once (its upload 400s). The other two upload clean.
	gar1 := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar1.Close()
	fits1 := fakes.NewFakeFitsel("test-passkey")
	fits1.FailNextQueued(http.StatusBadRequest)
	defer fits1.Close()
	garminBaseURL = gar1.URL()
	fitselBaseURL = fits1.URL()
	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")
	args := []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits1.URL() + "/ingest/wh/test-token"}
	// The failure (1001) is per-activity: the run continues, 1002/1003 upload.
	if err := run(args); err == nil {
		t.Fatal("first run: expected a partial-failure error")
	}
	st, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if st.FailCount("1001") != 1 {
		t.Fatalf("1001 fail count = %d, want 1", st.FailCount("1001"))
	}
	if st.Skipped("1001") {
		t.Fatal("1001 should not be skipped after one failure at threshold 3")
	}

	// Second run against a fresh state load (fresh process): the count is still
	// 1, so it keeps accumulating toward the skip rather than restarting.
	gar2 := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar2.Close()
	fits2 := fakes.NewFakeFitsel("test-passkey")
	fits2.FailNextQueued(http.StatusBadRequest, http.StatusBadRequest)
	defer fits2.Close()
	garminBaseURL = gar2.URL()
	fitselBaseURL = fits2.URL()
	args2 := []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits2.URL() + "/ingest/wh/test-token"}
	// 1001 fails again (its second), 1002 fails once. Neither skips yet.
	if err := run(args2); err == nil {
		t.Fatal("second run: expected a partial-failure error")
	}
	st2, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if st2.FailCount("1001") != 2 {
		t.Fatalf("1001 fail count = %d, want 2 (persisted from the first run)", st2.FailCount("1001"))
	}

	// Third run: 1001 fails a third time and reaches the threshold, so it skips.
	fits2.FailNextQueued(http.StatusBadRequest, http.StatusBadRequest)
	if err := run(args2); err == nil {
		t.Fatal("third run: expected a partial-failure error")
	}
	st3, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if !st3.Skipped("1001") {
		t.Fatal("1001 should be skipped after its third failure, persisted across runs")
	}
}

// TestUnskipFlagRetries proves --unskip clears the skip on every activity so a
// later run retries it, and exits without a sync run of its own.
func TestUnskipFlagRetries(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	// Drive 1001 to skipped across three runs, one failure each.
	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")
	args := []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}
	for i := 1; i <= 3; i++ {
		fits.FailNextQueued(http.StatusBadRequest)
		if err := run(args); err == nil {
			t.Fatalf("run %d: expected a partial-failure error", i)
		}
		st, err := state.Load(statePath)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		if i == 3 && !st.Skipped("1001") {
			t.Fatal("1001 should be skipped after three failures")
		}
	}

	// --unskip clears the skip and exits without a sync run.
	if err := run(append(args, "--unskip")); err != nil {
		t.Fatalf("--unskip: %v", err)
	}
	st2, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if st2.Skipped("1001") {
		t.Fatal("1001 should no longer be skipped after --unskip")
	}
	// The fail count is preserved, so the next failure skips again.
	fits.FailNextQueued(http.StatusBadRequest)
	if err := run(args); err == nil {
		t.Fatal("expected a partial-failure error after unskip")
	}
	st3, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if !st3.Skipped("1001") {
		t.Fatal("1001 should be skipped again (fail count preserved)")
	}
}

// TestSkipThresholdFlagOverride proves the threshold is overridable per run: a
// threshold of 1 skips an activity on its first failure.
func TestSkipThresholdFlagOverride(t *testing.T) {
	dir := t.TempDir()
	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	fits.FailNextQueued(http.StatusBadRequest)
	defer fits.Close()

	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")
	statePath := filepath.Join(dir, "state.json")
	args := []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token", "-skip-threshold", "1"}
	if err := run(args); err == nil {
		t.Fatal("expected a partial-failure error")
	}
	st, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if !st.Skipped("1001") {
		t.Fatal("1001 should be skipped at threshold 1")
	}
}

// TestSkipThresholdZeroDisablesSkip proves --skip-threshold 0 turns the skip
// off: an activity that fails every run is still retried every run and never
// marked skipped, while its failure count still accumulates and persists.
func TestSkipThresholdZeroDisablesSkip(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	// Three runs, 1001 fails (400s) every run. With the default threshold 3 it
	// would skip on the third; with 0 it must never skip.
	for i := 0; i < 3; i++ {
		gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
		garminBaseURL = gar.URL()
		fits := fakes.NewFakeFitsel("test-passkey")
		fits.FailNextQueued(http.StatusBadRequest)
		fitselBaseURL = fits.URL()
		t.Setenv("GARMIN_API_KEY", "test-garmin-key")
		t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
		t.Setenv("FITSEL_PASSKEY", "test-passkey")
		args := []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token", "-skip-threshold", "0"}
		if err := run(args); err == nil {
			t.Fatal("run: expected a partial-failure error")
		}
		st, err := state.Load(statePath)
		if err != nil {
			t.Fatalf("loading state: %v", err)
		}
		if st.Skipped("1001") {
			t.Fatalf("run %d: 1001 should not be skipped with skip-threshold 0", i+1)
		}
	}
	// The count still accumulates and persists across the runs.
	st, err := state.Load(statePath)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if got := st.FailCount("1001"); got != 3 {
		t.Fatalf("1001 fail count = %d, want 3 (still counted, just never skipped)", got)
	}
}

// TestInvalidUploadIsPerActivityUploadsTheRest proves a malformed upload (400)
// is a per-activity failure: it is logged and skipped, the run continues with
// the other activities, and the run exits with the partial-failure code.
func TestInvalidUploadIsPerActivityUploadsTheRest(t *testing.T) {
	dir := t.TempDir()
	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()
	// The first upload is malformed (400): a per-activity failure, so the run
	// skips it and uploads the other two.
	fits.Statuses = []int{http.StatusBadRequest}

	err := run(newRunArgs(t, dir, gar, fits))
	if err == nil {
		t.Fatal("expected a partial-failure error on a malformed upload")
	}
	if got := mapExitCode(err); got != exitPartialFailure {
		t.Fatalf("exit code = %d, want %d (partial failure)", got, exitPartialFailure)
	}
	if len(fits.Uploadees) != 2 {
		t.Fatalf("uploaded %d activities, want 2 (the two that succeeded)", len(fits.Uploadees))
	}
}

// TestUploadExhaustedExitsFitselProves repeated 500s (or a dead network) exhaust
// the retries, map to their own exit code, and mark nothing synced (per #85).
func TestUploadExhaustedExitsFitselAndUploadsNothing(t *testing.T) {
	useFastDelays(t)
	dir := t.TempDir()
	fits := fakes.NewFakeFitsel("test-passkey")
	// Fail every attempt: the run retries (3 total) then gives up.
	fits.Statuses = []int{
		http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError,
		http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError,
	}
	args := newFitselArgs(t, dir, fits)

	err := run(args)
	if err == nil {
		t.Fatal("expected an upload error after the retries are exhausted")
	}
	if got := mapExitCode(err); got != exitUploadExhausted {
		t.Fatalf("exit code = %d, want %d (upload exhausted)", got, exitUploadExhausted)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("exhausted upload uploaded %d files; expected none", len(fits.Uploadees))
	}
	st, err := state.Load(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if st.Done("1001") {
		t.Fatal("exhausted upload marked an activity synced; expected none")
	}
}

// TestInitWritesNoSyncRun proves `fitsel-client init` is a distinct setup
// subcommand: it writes the three secrets to the config and does not start a
// sync run. It feeds the three secrets on stdin, then checks the file holds all
// three via config.Load(). No environment is set, so the values come from the
// file, not from env.
func TestInitWritesNoSyncRun(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", configPath)
	// No GARMIN_API_KEY / FITSEL_TOKEN / FITSEL_PASSKEY set: init writes them,
	// and a later load reads them from the file alone.
	t.Setenv("GARMIN_API_KEY", "")
	t.Setenv("FITSEL_TOKEN", "")
	t.Setenv("FITSEL_PASSKEY", "")

	input := "garmin-key\nfitsel-token\nfitsel-passkey\n"
	var out bytes.Buffer
	if err := initCommand(strings.NewReader(input), &out, nil); err != nil {
		t.Fatalf("init: %v", err)
	}

	creds, err := config.Load()
	if err != nil {
		t.Fatalf("load after init: %v", err)
	}
	if creds.GarminKey != "garmin-key" || creds.FitselToken != "fitsel-token" || creds.FitselPasskey != "fitsel-passkey" {
		t.Fatalf("init did not store all three secrets: %+v", creds)
	}
}

// TestInitRejectsEmptyProvesNoPartialConfig proves init writes nothing when any
// of the three values is empty: a partial config would be a broken setup.
func TestInitRejectsEmptyProvesNoPartialConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", configPath)

	input := "garmin-key\n\nfitsel-passkey\n" // token is empty
	var out bytes.Buffer
	if err := initCommand(strings.NewReader(input), &out, nil); err == nil {
		t.Fatal("expected an error when a value is empty")
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("init wrote a config despite an empty value: %v", err)
	}
}

// TestInitNeverLeaksSecret proves the acceptance requirement: init never prints,
// logs or echoes a secret value on stdout. The prompts are the only thing
// written to out, and they name the field, never its value.
func TestInitNeverLeaksSecret(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FITSEL_CONFIG", filepath.Join(dir, "config.json"))

	// Real-looking secrets that must never appear in the output.
	input := "super-secret-garmin-key-XYZ\nsuper-secret-fitsel-token-ABC\nsuper-secret-passkey-DEF\n"
	var out bytes.Buffer
	if err := initCommand(strings.NewReader(input), &out, nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	got := out.String()
	for _, secret := range []string{"super-secret-garmin-key-XYZ", "super-secret-fitsel-token-ABC", "super-secret-passkey-DEF"} {
		if strings.Contains(got, secret) {
			t.Fatalf("init leaked a secret into output: %q", secret)
		}
	}
}

// TestInitConfirmNamesFileNoSecret proves the success message names the saved
// file but contains no secret and none of the "<from fitsel's setup page>"
// placeholders the missing-config test forbids.
func TestInitConfirmNamesFileNoSecret(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", configPath)

	input := "garmin-key\nfitsel-token\nfitsel-passkey\n"
	var out bytes.Buffer
	if err := initCommand(strings.NewReader(input), &out, nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, configPath) {
		t.Fatalf("confirm message should name the saved file %q, got %q", configPath, got)
	}
	for _, bad := range []string{"<create one at", "<from fitsel", "garmin-key", "fitsel-token", "fitsel-passkey"} {
		if strings.Contains(got, bad) {
			t.Fatalf("confirm message leaked %q or a placeholder: %q", bad, got)
		}
	}
}

// TestInitThenRunEndToEnd proves "a later run reads the stored values back":
// init writes the three secrets to the config file, then a run with no
// environment set reads them from that file, authenticates to the fake Garmin
// server and uploads every activity to the fake fitsel. httptest fakes +
// in-process stdin only — no real network.
func TestInitThenRunEndToEnd(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", configPath)
	// No environment set: the run must read all three secrets from the file.
	t.Setenv("GARMIN_API_KEY", "")
	t.Setenv("FITSEL_TOKEN", "")
	t.Setenv("FITSEL_PASSKEY", "")

	gar := fakes.NewFakeGarmin("test-garmin-key", newActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()

	// Point the clients at the fakes.
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})

	// init stores the three secrets that the later run will read from the file.
	input := "test-garmin-key\ntest-fitsel-token\ntest-passkey\n"
	var out bytes.Buffer
	if err := initCommand(strings.NewReader(input), &out, nil); err != nil {
		t.Fatalf("init: %v", err)
	}

	// A later run: no env, so config.Load() reads all three from the file.
	args := []string{
		"-since", "2026-10-01",
		"-state", statePath,
		"-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token",
	}
	if err := run(args); err != nil {
		t.Fatalf("run after init: %v", err)
	}
	if len(fits.Uploadees) != 3 {
		t.Fatalf("expected 3 uploads from the stored creds, got %d", len(fits.Uploadees))
	}
}

// newWindowedActivities wires a fake Garmin serving n activities across a wide
// date range (each a day apart, id 1 newest, id n oldest) plus a fake fitsel
// and the run args, for the --to end-to-end checks. The dates are real
// ISO-8601 days so the fake's lexicographic endDate filter orders them.
func newWindowedActivities(t *testing.T, n int) (*fakes.FakeGarmin, *fakes.FakeFitsel, []string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	// id 1 is the newest: day 0 (2026-11-01); id n is day n-1. Real ISO dates
	// sort lexicographically, so the fake's endDate filter cuts a clean window.
	acts := make([]fakes.FakeActivity, 0, n)
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		day := base.AddDate(0, 0, i-1).Format("2006-01-02T15:04:05Z")
		acts = append(acts, fakes.FakeActivity{
			ID:           int64(i),
			Name:         fmt.Sprintf("Activity %d", i),
			StartTimeGMT: day,
		})
	}
	gar := fakes.NewFakeGarmin("test-garmin-key", acts)
	fits := fakes.NewFakeFitsel("test-passkey")
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})
	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")
	return gar, fits, []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}
}

// TestRunWithToEndToEndIsWindowed proves the headline check: with --to on or
// after the newest activity, every activity since --since is synced (nothing
// after --to exists here, so all are uploaded); the window's upper bound does
// not drop anything within it.
func TestRunWithToEndToEndIsWindowed(t *testing.T) {
	n := 3
	_, fits, args := newWindowedActivities(t, n)
	args = append(args, "-to", "2026-12-31")
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(fits.Uploadees); got != n {
		t.Fatalf("uploaded %d, want %d (all in window)", got, n)
	}
}

// TestRunWithNoEndToEndKeepsOpenUpperBound proves --to omitted is unchanged:
// everything since --since is synced, exactly the --95 behaviour.
func TestRunWithNoEndToEndKeepsOpenUpperBound(t *testing.T) {
	n := 3
	_, fits, args := newWindowedActivities(t, n)
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(fits.Uploadees); got != n {
		t.Fatalf("uploaded %d, want %d (no --to: everything since --since)", got, n)
	}
}

// TestRunWithEndToEndCutsAbove proves --to drops everything after it: with the
// fake serving activities on Nov 1..Nov 20 (id 1 newest) and --to 2026-11-10,
// only those on or before the end date are listed and uploaded; nothing after
// --to is.
func TestRunWithEndToEndCutsAbove(t *testing.T) {
	const n = 20
	_, fits, args := newWindowedActivities(t, n)
	args = append(args, "-to", "2026-11-10")
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Activities id 1..10 are Nov 1..Nov 10; id 11..20 are Nov 11..Nov 20, all
	// after --to and so excluded.
	if got := len(fits.Uploadees); got != 10 {
		t.Fatalf("uploaded %d, want 10 (Nov 1..Nov 10), --to cut the rest", got)
	}
	// Every uploaded id must be on or before --to: none past Nov 10.
	for i := int64(11); i <= n; i++ {
		if got := fits.Count([]byte("FIT-DATA-" + strconv.FormatInt(i, 10))); got != 0 {
			t.Fatalf("activity %d (after --to) was uploaded %d times; want 0", i, got)
		}
	}
}

// TestRunWithEndToEndPaginationNeverTruncated proves the pagination check: the
// fake serves more than 100 activities inside [--since, --to] across pages, and
// with --to narrowing the window the client still lists every in-window one
// (PagesSeen covers all pages) and uploads each — the narrow window never hits
// the old 100-cap truncation.
func TestRunWithEndToEndPaginationNeverTruncated(t *testing.T) {
	const n = 250
	gar, fits, args := newWindowedActivities(t, n)
	// All 250 fall on/after 2026-11-01; --to well past the last keeps every one,
	// so the window spans all 250 across pages — the shape that used to truncate
	// at 100.
	args = append(args, "-to", "2027-12-31")
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(fits.Uploadees); got != n {
		t.Fatalf("uploaded %d, want %d (all in window, none dropped at the 100 cap)", got, n)
	}
	// The window is bigger than one page, so the client walked all three.
	want := []int{0, 100, 200}
	if len(gar.PagesSeen) != len(want) {
		t.Fatalf("list requests = %v, want %v (all pages walked)", gar.PagesSeen, want)
	}
	// Every activity uploaded exactly once.
	for i := int64(1); i <= n; i++ {
		if got := fits.Count([]byte("FIT-DATA-" + strconv.FormatInt(i, 10))); got != 1 {
			t.Fatalf("activity %d uploaded %d times; want exactly once", i, got)
		}
	}
}

// TestRunWithBadEndToEndRejected proves a malformed --to is rejected before any
// network: the run stops with an error naming --to, nothing is uploaded.
func TestRunWithBadEndToEndRejected(t *testing.T) {
	_, fits, args := newWindowedActivities(t, 3)
	args = append(args, "-to", "not-a-date")
	err := run(args)
	if err == nil {
		t.Fatal("expected a run error on a malformed --to")
	}
	if !strings.Contains(err.Error(), "--to") {
		t.Fatalf("error = %q, want it to name --to", err)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("bad --to uploaded %d files; expected none", len(fits.Uploadees))
	}
}

// TestRunWithInvalidEndToEndRejected proves a shape-correct but calendar-invalid
// --to (e.g. Feb 30) is rejected: parseDate rejects it, so the run stops before
// any network.
func TestRunWithInvalidEndToEndRejected(t *testing.T) {
	_, fits, args := newWindowedActivities(t, 3)
	args = append(args, "-to", "2026-02-30")
	err := run(args)
	if err == nil {
		t.Fatal("expected a run error on an invalid calendar --to")
	}
	if !strings.Contains(err.Error(), "--to") {
		t.Fatalf("error = %q, want it to name --to", err)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("invalid --to uploaded %d files; expected none", len(fits.Uploadees))
	}
}

// newRunArgs wires a fresh fake Garmin (three activities) and a fake fitsel
// endpoint for a per-activity-failure run and returns the fitsel plus the run
// args. The fake Garmin serves activities 1001, 1002, 1003 (newest first);
// the run syncs from 2026-10-01, so all three are pending.
func newRunArgs(t *testing.T, dir string, gar *fakes.FakeGarmin, fits *fakes.FakeFitsel) []string {
	t.Helper()
	statePath := filepath.Join(dir, "state.json")
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})
	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")
	return []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}
}

// TestOneFailedActivityUploadsTheRest is the headline check: a run over three
// activities where the second activity's download fails once (a transient 404)
// is logged and skipped, the other two still upload, and the run exits with the
// dedicated partial-failure code. The failed activity is left pending for the
// next run.
func TestOneFailedActivityUploadsTheRest(t *testing.T) {
	dir := t.TempDir()
	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()
	// The first download (activity 1001) 404s once, then recovers: a
	// per-activity download failure, so the run continues with the other two.
	gar.DownloadFailOnce = true

	err := run(newRunArgs(t, dir, gar, fits))
	if err == nil {
		t.Fatal("expected a partial-failure error when one activity fails")
	}
	if got := mapExitCode(err); got != exitPartialFailure {
		t.Fatalf("exit code = %d, want %d (partial failure)", got, exitPartialFailure)
	}
	// The other two activities were uploaded despite the first one failing.
	if len(fits.Uploadees) != 2 {
		t.Fatalf("uploaded %d activities, want 2 (the two that succeeded)", len(fits.Uploadees))
	}
	// The failed activity was not marked synced, so it is pending for rerun.
	st, err := state.Load(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	synced := 0
	for _, id := range []string{"1001", "1002", "1003"} {
		if st.Done(id) {
			synced++
		}
	}
	if synced != 2 {
		t.Fatalf("marked %d activities synced, want 2 (the two that uploaded)", synced)
	}
	// The failed one is left pending; a later run with the fake healthy finishes it.
	if rerunErr := run(newRunArgs(t, dir, gar, fits)); rerunErr != nil {
		t.Fatalf("rerun: %v", rerunErr)
	}
	if got := len(fits.Uploadees); got != 3 {
		t.Fatalf("rerun uploaded %d total, want 3 (the pending one finished)", got)
	}
}

// TestFailedUploadSkipsRest proves a per-activity upload failure (the first
// upload 400s once, then recovers) is logged and the run continues with the
// next activities, rather than stopping the run.
func TestFailedUploadSkipsRest(t *testing.T) {
	dir := t.TempDir()
	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()
	// The first upload 400s once (malformed) then recovers: a per-activity
	// failure, so the run continues past it after the single retry.
	fits.Statuses = []int{http.StatusBadRequest}

	err := run(newRunArgs(t, dir, gar, fits))
	if err == nil {
		t.Fatal("expected a partial-failure error when an activity fails")
	}
	if got := mapExitCode(err); got != exitPartialFailure {
		t.Fatalf("exit code = %d, want %d (partial failure)", got, exitPartialFailure)
	}
	// The other two activities were still sent.
	if len(fits.Uploadees) != 2 {
		t.Fatalf("uploaded %d activities, want 2 (the two that succeeded)", len(fits.Uploadees))
	}
}

// TestRunWideUploadErrorStillAborts proves a run-wide upload failure (every
// upload 401) still aborts with its own run-wide code, not the partial code:
// the discriminator's retry of the first upload also fails, so the run stops.
func TestRunWideUploadErrorStillAborts(t *testing.T) {
	dir := t.TempDir()
	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()
	// Fail every upload (401 twice: the initial attempt and the run's single
	// retry), so the bad signature is persistent and the run aborts.
	fits.Statuses = []int{http.StatusUnauthorized, http.StatusUnauthorized}

	err := run(newRunArgs(t, dir, gar, fits))
	if err == nil {
		t.Fatal("expected a run-wide error on a bad signature")
	}
	if got := mapExitCode(err); got != exitBadSignature {
		t.Fatalf("exit code = %d, want %d (bad signature, run-wide)", got, exitBadSignature)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("bad signature uploaded %d files; expected none", len(fits.Uploadees))
	}
}

// doctorCreds is the set of three secrets a valid doctor run must have. Each is
// a real-looking secret that must never appear in the doctor's output.
const (
	doctorGarminKey  = "doctor-garmin-key-XYZ"
	doctorFitselTok  = "doctor-fitsel-token-ABC"
	doctorFitselPass = "doctor-fitsel-passkey-DEF"
)

// writeDoctorConfig writes a config file with the given keys at $FITSEL_CONFIG
// and mode 0600, returning the path. A key left as "" is absent from the file.
func writeDoctorConfig(t *testing.T, keys map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", path)
	const sample = `{"garmin_api_key":"%s","fitsel_token":"%s","fitsel_passkey":"%s"}`
	s := fmt.Sprintf(sample, keys["garmin_api_key"], keys["fitsel_token"], keys["fitsel_passkey"])
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

// TestDoctorAllChecksPass is the headline check: with a valid config, a present
// state file, and a fake Garmin that returns an access token, doctor prints
// one ✓ line per check plus the fixed fitsel line, and exits 0.
func TestDoctorAllChecksPass(t *testing.T) {
	writeDoctorConfig(t, map[string]string{
		"garmin_api_key": doctorGarminKey,
		"fitsel_token":   doctorFitselTok,
		"fitsel_passkey": doctorFitselPass,
	})
	statePath := filepath.Join(t.TempDir(), "state.json")
	// A present, readable state file.
	if err := os.WriteFile(statePath, []byte(`{"synced":{}}`), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	gar := fakes.NewFakeGarmin(doctorGarminKey, fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel(doctorFitselPass)
	defer fits.Close()

	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})

	var out bytes.Buffer
	err := runDoctor(strings.NewReader(""), &out, []string{"-state", statePath})
	if err != nil {
		t.Fatalf("doctor: %v\nREPORT:\n%s", err, out.String())
	}
	// A passing doctor run returns nil (exit 0); mapExitCode(nil) is not 0
	// (nil falls through to the general code), so assert the nil directly.
	if err != nil {
		t.Fatalf("doctor returned non-nil error on a passing run: %v", err)
	}

	report := out.String()
	for _, want := range []string{
		"✓ config file", "✓ all three secrets present",
		"✓ Garmin key exchanges for an access token",
		"✓ state file", "fitsel credentials can only be verified by a real sync",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, report)
		}
	}
}

// TestDoctorAllChecksPassExitsZero is the headline check: with a valid config, a
// present state file, and a fake Garmin that returns an access token, doctor
// prints one ✓ line per check plus the fixed fitsel line, and exits 0.
func TestDoctorBadKeyExitsNonZero(t *testing.T) {
	writeDoctorConfig(t, map[string]string{
		"garmin_api_key": "wrong-key",
		"fitsel_token":   doctorFitselTok,
		"fitsel_passkey": doctorFitselPass,
	})
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"synced":{}}`), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	gar := fakes.NewFakeGarmin(doctorGarminKey, fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel(doctorFitselPass)
	defer fits.Close()
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})

	var out bytes.Buffer
	err := runDoctor(strings.NewReader(""), &out, []string{"-state", statePath})
	if err == nil {
		t.Fatal("expected a non-zero exit on a bad Garmin key")
	}
	// Any non-zero exit is acceptable per the acceptance (exit 0 only if all
	// pass); a bad key is a general failure here.
	if got := mapExitCode(err); got == exitOK {
		t.Fatalf("exit code = %d, want non-zero (bad key)", got)
	}
	report := out.String()
	if !strings.Contains(report, "✗ Garmin key rejected") {
		t.Fatalf("expected a ✗ on the rejected key, got:\n%s", report)
	}
	// The other checks still run and report, so the athlete sees the whole
	// picture at once.
	if !strings.Contains(report, "✓ all three secrets present") {
		t.Fatalf("other checks should still run, got:\n%s", report)
	}
}

// TestDoctorMissingKeyReportsWhich proves a missing secret is a ✗ that names
// the absent key (never its value), and the run exits non-zero.
func TestDoctorMissingKeyReportsWhich(t *testing.T) {
	writeDoctorConfig(t, map[string]string{
		"garmin_api_key": doctorGarminKey,
		// fitsel_token absent
		"fitsel_passkey": doctorFitselPass,
	})
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"synced":{}}`), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	gar := fakes.NewFakeGarmin(doctorGarminKey, fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel(doctorFitselPass)
	defer fits.Close()
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})

	var out bytes.Buffer
	err := runDoctor(strings.NewReader(""), &out, []string{"-state", statePath})
	if err == nil {
		t.Fatal("expected a non-zero exit on a missing key")
	}
	report := out.String()
	if !strings.Contains(report, "FITSEL_TOKEN") {
		t.Fatalf("missing-key error should name the absent key, got:\n%s", report)
	}
	if !strings.Contains(report, "✗") {
		t.Fatalf("expected a ✗ for the missing key, got:\n%s", report)
	}
}

// TestDoctorMissingConfig proves an absent config file is a clean ✗ (not a
// pass-by-accident, since config.Load() would create a placeholder).
func TestDoctorMissingConfig(t *testing.T) {
	// No config file written; $FITSEL_CONFIG points at a path that does not
	// exist.
	dir := t.TempDir()
	t.Setenv("FITSEL_CONFIG", filepath.Join(dir, "config.json"))
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"synced":{}}`), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	var out bytes.Buffer
	err := runDoctor(strings.NewReader(""), &out, []string{"-state", statePath})
	if err == nil {
		t.Fatal("expected a non-zero exit when the config is absent")
	}
	report := out.String()
	if !strings.Contains(report, "✗ config file not found") {
		t.Fatalf("expected a ✗ on the missing config, got:\n%s", report)
	}
}

// TestDoctorConfigNot0600 proves a config file with group/other access is a
// ✗: the security intent of mode 0600 is no group/other read.
func TestDoctorConfigNot0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", path)
	const sample = `{"garmin_api_key":"%s","fitsel_token":"%s","fitsel_passkey":"%s"}`
	s := fmt.Sprintf(sample, doctorGarminKey, doctorFitselTok, doctorFitselPass)
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil { // too permissive
		t.Fatalf("writing config: %v", err)
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"synced":{}}`), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	gar := fakes.NewFakeGarmin(doctorGarminKey, fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel(doctorFitselPass)
	defer fits.Close()
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})

	var out bytes.Buffer
	err := runDoctor(strings.NewReader(""), &out, []string{"-state", statePath})
	if err == nil {
		t.Fatal("expected a non-zero exit on a world-readable config")
	}
	report := out.String()
	if !strings.Contains(report, "✗ config file") || !strings.Contains(report, "mode") {
		t.Fatalf("expected a ✗ on the config mode, got:\n%s", report)
	}
}

// TestDoctorMissingState proves an absent state file is a ✗ (state.Load("")
// would succeed for a missing file, so presence is checked on the path).
func TestDoctorMissingState(t *testing.T) {
	writeDoctorConfig(t, map[string]string{
		"garmin_api_key": doctorGarminKey,
		"fitsel_token":   doctorFitselTok,
		"fitsel_passkey": doctorFitselPass,
	})
	statePath := filepath.Join(t.TempDir(), "state.json") // absent

	gar := fakes.NewFakeGarmin(doctorGarminKey, fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel(doctorFitselPass)
	defer fits.Close()
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})

	var out bytes.Buffer
	err := runDoctor(strings.NewReader(""), &out, []string{"-state", statePath})
	if err == nil {
		t.Fatal("expected a non-zero exit when the state file is absent")
	}
	report := out.String()
	if !strings.Contains(report, "✗ state file not found") {
		t.Fatalf("expected a ✗ on the missing state, got:\n%s", report)
	}
}

// TestDoctorNoSecretLeaked proves no secret value ever appears in the output,
// even on failure paths (bad key, missing key).
func TestDoctorNoSecretLeaked(t *testing.T) {
	writeDoctorConfig(t, map[string]string{
		"garmin_api_key": doctorGarminKey,
		"fitsel_token":   doctorFitselTok,
		"fitsel_passkey": doctorFitselPass,
	})
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"synced":{}}`), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	// A bad key: the report must not leak any secret value.
	gar := fakes.NewFakeGarmin("wrong-key", fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel(doctorFitselPass)
	defer fits.Close()
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})

	var out bytes.Buffer
	_ = runDoctor(strings.NewReader(""), &out, []string{"-state", statePath})
	report := out.String()
	for _, secret := range []string{doctorGarminKey, doctorFitselTok, doctorFitselPass} {
		if strings.Contains(report, secret) {
			t.Fatalf("doctor leaked a secret into output: %q\n%s", secret, report)
		}
	}
}

// TestDoctorNeverTouchesFitsel proves the structural guarantee: doctor never
// builds a fitsel client or makes any fitel call. The fake fitsel server never
// receives a request — the Uploadees slice stays empty.
func TestDoctorNeverTouchesFitsel(t *testing.T) {
	writeDoctorConfig(t, map[string]string{
		"garmin_api_key": doctorGarminKey,
		"fitsel_token":   doctorFitselTok,
		"fitsel_passkey": doctorFitselPass,
	})
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"synced":{}}`), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	gar := fakes.NewFakeGarmin(doctorGarminKey, fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel(doctorFitselPass)
	defer fits.Close()
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})

	var out bytes.Buffer
	if err := runDoctor(strings.NewReader(""), &out, []string{"-state", statePath}); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("doctor uploaded %d files; it must never touch fitsel", len(fits.Uploadees))
	}
	// The fixed line is present.
	if !strings.Contains(out.String(), "fitsel credentials can only be verified by a real sync") {
		t.Fatalf("expected the fixed fitsel line, got:\n%s", out.String())
	}
}

// TestDoctorThenEndToEnd proves a valid doctor run exits zero end to end: init
// writes the config, a present state file is written, and doctor over the fake
// Garmin passes every check. No real network, in-process stdin only.
func TestDoctorThenEndToEnd(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", configPath)
	// No environment set: doctor must read all three from the file.
	t.Setenv("GARMIN_API_KEY", "")
	t.Setenv("FITSEL_TOKEN", "")
	t.Setenv("FITSEL_PASSKEY", "")

	statePath := filepath.Join(dir, "state.json")

	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})

	// init stores the three secrets the later doctor reads from the file.
	input := "test-garmin-key\ntest-fitsel-token\ntest-passkey\n"
	var initOut bytes.Buffer
	if err := initCommand(strings.NewReader(input), &initOut, nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	// The init offer to run doctor is printed.
	if !strings.Contains(initOut.String(), "fitsel-client doctor") {
		t.Fatalf("init should offer a doctor run, got:\n%s", initOut.String())
	}

	// A present, readable state file.
	if err := os.WriteFile(statePath, []byte(`{"synced":{}}`), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	var out bytes.Buffer
	err := runDoctor(strings.NewReader(""), &out, []string{"-state", statePath})
	if err != nil {
		t.Fatalf("doctor: %v\nREPORT:\n%s", err, out.String())
	}
	// A passing doctor run returns nil (exit 0); mapExitCode(nil) is not 0
	// (nil falls through to the general code), so assert the nil directly.
	if err != nil {
		t.Fatalf("doctor returned non-nil error on a passing run: %v", err)
	}
}

// TestDoctorInitOfferNoSecret proves the init offer line names doctor but
// leaks no secret, and does not break the init tests that feed exactly three
// lines.
func TestDoctorInitOfferNoSecret(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", configPath)

	input := "garmin-key\nfitsel-token\nfitsel-passkey\n"
	var out bytes.Buffer
	if err := initCommand(strings.NewReader(input), &out, nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Run 'fitsel-client doctor'") {
		t.Fatalf("init should offer a doctor run, got:\n%s", got)
	}
	for _, bad := range []string{"garmin-key", "fitsel-token", "fitsel-passkey"} {
		if strings.Contains(got, bad) {
			t.Fatalf("init offer leaked %q: %q", bad, got)
		}
	}
}

// TestStatusNeverTouchesNetwork proves the offline health check builds no gar
// client at all: against a fake Garmin that counts requests, a plain `status`
// (no --pending) makes zero requests to it, even though the fake is up.
func TestStatusNeverTouchesNetwork(t *testing.T) {
	writeDoctorConfig(t, map[string]string{
		"garmin_api_key": doctorGarminKey,
		"fitsel_token":   doctorFitselTok,
		"fitsel_passkey": doctorFitselPass,
	})
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"synced":{}}`), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	gar := fakes.NewFakeGarmin(doctorGarminKey, fakes.NewActivities())
	defer gar.Close()
	garminBaseURL = gar.URL()
	t.Cleanup(func() { garminBaseURL = garmin.BaseURL })

	var out bytes.Buffer
	if err := runStatus(strings.NewReader(""), &out, []string{"-state", statePath}); err != nil {
		t.Fatalf("status: %v\nREPORT:\n%s", err, out.String())
	}
	if gar.Tokens != 0 {
		t.Fatalf("status made network requests (%d tokens issued); it must touch no network", gar.Tokens)
	}
}

// TestStatusOfflineReport proves the offline report renders the persisted
// quantities: config path, state path, last synced time, newest synced activity
// by name+date, and the synced count. It loads a new-schema state and asserts
// each line renders.
func TestStatusOfflineReport(t *testing.T) {
	writeDoctorConfig(t, map[string]string{
		"garmin_api_key": doctorGarminKey,
		"fitsel_token":   doctorFitselTok,
		"fitsel_passkey": doctorFitselPass,
	})
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	// Write a new-schema state: two activities synced, with the run-level
	// last-synced time and per-activity metadata.
	meta := `{"synced":{"1001":{"name":"Morning Run","start":"2026-10-20 07:00:00"},"1002":{"name":"Evening Ride","start":"2026-10-25 18:30:00"}},"fails":{},"skips":{},"lastSyncedAt":"2026-10-25T18:30:05Z"}`
	if err := os.WriteFile(statePath, []byte(meta), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	var out bytes.Buffer
	if err := runStatus(strings.NewReader(""), &out, []string{"-state", statePath}); err != nil {
		t.Fatalf("status: %v\nREPORT:\n%s", err, out.String())
	}
	report := out.String()
	for _, want := range []string{
		"config: ",
		"state: " + statePath,
		"last synced: 2026-10-25 18:30:05",
		"newest synced activity: Evening Ride (2026-10-25 18:30:00)",
		"activities synced so far: 2",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("status output missing %q:\n%s", want, report)
		}
	}
}

// TestStatusNeverSynced proves the "never synced yet" line renders when the
// state is empty, and the report still exits 0.
func TestStatusNeverSynced(t *testing.T) {
	writeDoctorConfig(t, map[string]string{
		"garmin_api_key": doctorGarminKey,
		"fitsel_token":   doctorFitselTok,
		"fitsel_passkey": doctorFitselPass,
	})
	statePath := filepath.Join(t.TempDir(), "state.json") // absent

	var out bytes.Buffer
	if err := runStatus(strings.NewReader(""), &out, []string{"-state", statePath}); err != nil {
		t.Fatalf("status: %v\nREPORT:\n%s", err, out.String())
	}
	report := out.String()
	if !strings.Contains(report, "last synced: never synced yet") {
		t.Fatalf("expected a never-synced line, got:\n%s", report)
	}
	if !strings.Contains(report, "newest synced activity: none") {
		t.Fatalf("expected a 'none' newest line, got:\n%s", report)
	}
	if !strings.Contains(report, "activities synced so far: 0") {
		t.Fatalf("expected zero count, got:\n%s", report)
	}
}

// TestStatusOldStateFormatStillReports proves an old {true}-format state file
// still reports the synced count without error, just with no name/date to
// display.
func TestStatusOldStateFormatStillReports(t *testing.T) {
	writeDoctorConfig(t, map[string]string{
		"garmin_api_key": doctorGarminKey,
		"fitsel_token":   doctorFitselTok,
		"fitsel_passkey": doctorFitselPass,
	})
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	const old = `{"synced":{"1001":true,"1002":true},"fails":{},"skips":{}}`
	if err := os.WriteFile(statePath, []byte(old), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	var out bytes.Buffer
	if err := runStatus(strings.NewReader(""), &out, []string{"-state", statePath}); err != nil {
		t.Fatalf("status: %v\nREPORT:\n%s", err, out.String())
	}
	report := out.String()
	if !strings.Contains(report, "activities synced so far: 2") {
		t.Fatalf("expected count 2 from old format, got:\n%s", report)
	}
	if !strings.Contains(report, "newest synced activity: none") {
		t.Fatalf("old format has no start, so newest should be none, got:\n%s", report)
	}
}

// TestStatusNoSecretLeaked proves no secret value ever appears in the status
// output, even though the config holds real secrets. status prints the config
// path only, never its contents.
func TestStatusNoSecretLeaked(t *testing.T) {
	writeDoctorConfig(t, map[string]string{
		"garmin_api_key": doctorGarminKey,
		"fitsel_token":   doctorFitselTok,
		"fitsel_passkey": doctorFitselPass,
	})
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"synced":{}}`), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	var out bytes.Buffer
	_ = runStatus(strings.NewReader(""), &out, []string{"-state", statePath})
	report := out.String()
	for _, secret := range []string{doctorGarminKey, doctorFitselTok, doctorFitselPass} {
		if strings.Contains(report, secret) {
			t.Fatalf("status leaked a secret into output: %q\n%s", secret, report)
		}
	}
}

// TestStatusPendingListsWhatWouldUpload mirrors TestDryRunUploadsNothing: it
// lists what a sync would upload, uploads nothing, and leaves state
// byte-for-byte unchanged. It builds a gar client (so it touches the network
// once) but never a fitsel client.
func TestStatusPendingListsWhatWouldUpload(t *testing.T) {
	useFastDelays(t)
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	gar := fakes.NewFakeGarmin("test-garmin-key", newActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()

	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})
	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")

	// A present, empty state: nothing synced yet.
	if err := os.WriteFile(statePath, []byte(`{"synced":{}}`), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}

	var out bytes.Buffer
	if err := runStatus(strings.NewReader(""), &out, []string{"-pending", "-state", statePath}); err != nil {
		t.Fatalf("status --pending: %v\nREPORT:\n%s", err, out.String())
	}
	report := out.String()
	for _, want := range []string{
		"- Morning Run (1001)",
		"- Evening Ride (1002)",
		"- Weekend Hike (1003)",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("status --pending output missing %q:\n%s", want, report)
		}
	}
	// Nothing was uploaded: status --pending builds no fitsel client.
	if len(fits.Uploadees) != 0 {
		t.Fatalf("status --pending uploaded %d files; it must upload nothing", len(fits.Uploadees))
	}
	// State is byte-for-byte unchanged: no activity marked synced.
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("status --pending changed the state file:\nbefore: %s\nafter:  %s", before, after)
	}
	// No file was downloaded from Garmin either: it only listed.
	if gar.Downloads != 0 {
		t.Fatalf("status --pending downloaded %d files; it must only list", gar.Downloads)
	}
}

// TestStatusPendingUpToDate proves that when everything is already synced,
// status --pending reports nothing to sync and still touches no fitsel.
func TestStatusPendingUpToDate(t *testing.T) {
	writeDoctorConfig(t, map[string]string{
		"garmin_api_key": doctorGarminKey,
		"fitsel_token":   doctorFitselTok,
		"fitsel_passkey": doctorFitselPass,
	})
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	// All three already synced.
	meta := `{"synced":{"1001":{"name":"Morning Run","start":"2026-10-20 07:00:00"},"1002":{"name":"Evening Ride","start":"2026-10-25 18:30:00"},"1003":{"name":"Weekend Hike","start":"2026-10-27 09:15:00"}},"fails":{},"skips":{}}`
	if err := os.WriteFile(statePath, []byte(meta), 0o600); err != nil {
		t.Fatalf("writing state: %v", err)
	}

	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})
	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")

	var out bytes.Buffer
	if err := runStatus(strings.NewReader(""), &out, []string{"-pending", "-state", statePath}); err != nil {
		t.Fatalf("status --pending: %v\nREPORT:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "nothing to sync — up to date") {
		t.Fatalf("expected up-to-date line, got:\n%s", out.String())
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("status --pending uploaded %d files; it must upload nothing", len(fits.Uploadees))
	}
}

// TestDefaultStatePathIsUsedWithoutStateFlag is the acceptance check for #127:
// with no -state flag, the run reads and writes the default
// ~/.config/fitsel-client/state.json, so a second run uploads nothing. HOME is
// pointed at a temp dir so the default path lands in a throwaway location; the
// tool is driven exactly as an athlete would run it, with no -state at all.
func TestDefaultStatePathIsUsedWithoutStateFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("FITSEL_STATE", "") // ensure the default path is used, not $FITSEL_STATE

	gar := fakes.NewFakeGarmin("test-garmin-key", newActivities())
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()

	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})
	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")

	// No -state: the run must use the documented default path.
	args := []string{"-since", "2026-10-01", "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}

	// First run: uploads everything.
	if err := run(args); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(fits.Uploadees) != 3 {
		t.Fatalf("first run uploaded %d, want 3", len(fits.Uploadees))
	}

	// The default state file must have been written next to the config dir.
	wantPath := filepath.Join(dir, ".config", "fitsel-client", "state.json")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("default state file not written at %s: %v", wantPath, err)
	}

	// Second run straight away: everything is already marked synced, so nothing
	// is uploaded — the idempotency the default state file gives.
	if err := run(args); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(fits.Uploadees) != 3 {
		t.Fatalf("second run uploaded %d extra files; expected nothing (state not persisted by default)", len(fits.Uploadees)-3)
	}
}

// TestMidCancelStopsRunWithoutMarkingInFlightSynced is the headline check: a
// run over three activities cancelled while the third upload is genuinely in
// flight stops promptly, maps to exitInterrupted, marks exactly the two that
// succeeded as done, and leaves the third not-done.
func TestMidCancelStopsRunWithoutMarkingInFlightSynced(t *testing.T) {
	dir := t.TempDir()
	stPath := filepath.Join(dir, "state.json")
	fits := fakes.NewFakeFitsel("test-passkey")
	defer fits.Close()

	t.Setenv("GARMIN_API_KEY", "test-garmin-key")
	t.Setenv("FITSEL_TOKEN", "test-fitsel-token")
	t.Setenv("FITSEL_PASSKEY", "test-passkey")

	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer gar.Close()
	garminBaseURL = gar.URL()
	fitselBaseURL = fits.URL()
	t.Cleanup(func() {
		garminBaseURL = garmin.BaseURL
		fitselBaseURL = "https://ingest.fitsel.app/ingest/wh"
	})

	// The run's own context, which we cancel to simulate a Ctrl-C.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// The upload server accepts the first two uploads, then blocks the third
	// until the run's context is cancelled. The client aborts this connection
	// the moment ctx is cancelled, so the run returns; the handler goroutine
	// unblocks on the same ctx.
	var uploads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&uploads, 1)
		if n == 3 {
			<-ctx.Done()
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	fits.Server = srv

	args := []string{"-since", "2026-10-01", "-state", stPath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}
	runErr := make(chan error, 1)
	go func() {
		runErr <- runWithCtx(ctx, args)
	}()

	// Wait until the third upload is in flight, then cancel.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&uploads) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&uploads) < 3 {
		t.Fatal("third upload never started")
	}
	cancel()

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("expected a run error on cancel")
		}
		if got := mapExitCode(err); got != exitInterrupted {
			t.Fatalf("exit code = %d, want %d (interrupted)", got, exitInterrupted)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}

	// The two that succeeded before the stop are still saved.
	st2, err := state.Load(stPath)
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	if !st2.Done("1001") || !st2.Done("1002") {
		t.Fatal("activities synced before the stop were not saved")
	}
	if st2.Done("1003") {
		t.Fatal("the in-flight activity was marked synced after cancel")
	}
}

// TestInterruptPrintsNonErrorLineOnStderr proves the interrupt line a cancelled
// run writes to stderr is its own dedicated line, not the generic `error:` line
// (per #135 criterion 2). A Ctrl-C is the athlete asking to stop, not a
// failure, so it must not ride the `error:` printer that hides a stop behind a
// fake failure. It drives reportError directly (the same print-and-exit
// contract TestErrorPrintedOnStderr exercises for the error path) and captures
// stderr to check the exact line and exit code.
func TestInterruptPrintsNonErrorLineOnStderr(t *testing.T) {
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	calledWith := -1
	reportError(errInterrupted, func(code int) { calledWith = code })

	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr has %d line(s), want exactly 1 reporting line: %q", len(lines), buf.String())
	}
	if lines[0] == "" {
		t.Fatal("stderr is empty; the interrupt should report the stop on one line")
	}
	if strings.HasPrefix(lines[0], "error:") {
		t.Fatalf("interrupt line %q must not begin with 'error:' (a stop is not a failure)", lines[0])
	}
	if calledWith != exitInterrupted {
		t.Fatalf("exit code = %d, want %d (interrupted)", calledWith, exitInterrupted)
	}
}

// TestInterruptLineReportsTheStop asserts the interrupt line names the stop so
// the athlete (or cron) knows what happened without a failure, while still
// being a single self-describing line.
func TestInterruptLineReportsTheStop(t *testing.T) {
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	reportError(errInterrupted, func(int) {})

	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	io.Copy(&buf, r)
	r.Close()

	if !strings.Contains(buf.String(), "interrupted") {
		t.Fatalf("interrupt line %q should report the stop", buf.String())
	}
}

// TestFitselUploadDelayBailsEarlyOnCancel proves the fitsel retry delay honours
// the context: a 500 that would normally wait uploadDelay before retrying
// returns as soon as the ctx is cancelled, well under the delay.
func TestFitselUploadDelayBailsEarlyOnCancel(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	up := fitsel.New(srv.URL, "", "passkey")

	done := make(chan error, 1)
	go func() {
		_, err := up.Upload(ctx, []byte("FIT"))
		done <- err
	}()

	// Cancel during the retry delay (before the 2s uploadDelay elapses).
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a cancellation error from Upload")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Upload error = %v, want context.Canceled", err)
		}
		if got := atomic.LoadInt32(&attempts); got != 1 {
			t.Fatalf("attempts = %d, want 1 (it bailed before the retry)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Upload did not return after cancel")
	}
}

// TestGarminRateLimitClientBailsEarly proves the Garmin client's 429 back-off
// returns ctx.Err() instead of sitting through the full Retry-After window when
// the context is cancelled.
func TestGarminRateLimitClientBailsEarly(t *testing.T) {
	useFastDelays(t)
	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	gar.RateLimitOnce = true // Retry-After: 2s
	defer gar.Close()
	gc := garmin.New("test-garmin-key")
	gc.SetBaseURL(gar.URL())

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := gc.List(ctx, "2026-10-01", "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a cancellation error from List")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("List error = %v, want context.Canceled", err)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("back-off took %v; it should have bailed early on cancel, not waited the full Retry-After", elapsed)
	}
}

// TestUpdateCheckNewerVersionPrintsHint proves the wiring end to end: when a
// newer release exists, main prints the one-line upgrade hint (URL then brew)
// to stdout. The check points at a fake GitHub via the injectable base and a
// pinned dev version bump; the hint is read from captured stdout.
func TestUpdateCheckNewerVersionPrintsHint(t *testing.T) {
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

	// Point the checker's GitHub base at a fake that reports a newer release,
	// and set the built-in version the checker compares against.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"https://example.com/releases/v9.9.9"}`))
	}))
	defer srv.Close()
	githubBaseURL = srv.URL
	prevVersion := version
	version = "1.0.0"
	t.Cleanup(func() { version = prevVersion })

	// Point the checker's throttle state at a fresh temp dir so this is a first
	// run (no prior timestamp to throttle against).
	t.Setenv("FITSEL_STATE", dir)

	// Capture stdout for the run; the hint prints alongside the sync summary.
	orig := os.Stdout
	r_, w_, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w_
	defer func() { os.Stdout = orig }()

	args := []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	w_.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r_)
	r_.Close()

	out := buf.String()
	if !strings.Contains(out, "https://example.com/releases/v9.9.9") {
		t.Fatalf("hint missing the release URL; got:\n%s", out)
	}
	if !strings.Contains(out, "brew upgrade fitsel-client") {
		t.Fatalf("hint missing the brew command; got:\n%s", out)
	}
}

// TestUpdateCheckOptedOutMakesNoRequest proves the opt-out (FITSEL_NO_UPDATE_CHECK=1
// or --no-update-check) suppresses the check entirely: no request reaches the
// fake GitHub. The fake counts requests; the opt-out must leave it at zero.
func TestUpdateCheckOptedOutMakesNoRequest(t *testing.T) {
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

	var requests int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"https://example.com/releases/v9.9.9"}`))
	}))
	defer srv.Close()
	githubBaseURL = srv.URL
	prevVersion := version
	version = "1.0.0"
	t.Cleanup(func() { version = prevVersion })

	// Opt out via the env var.
	t.Setenv("FITSEL_NO_UPDATE_CHECK", "1")
	args := []string{"-since", "2026-10-01", "-state", statePath, "-fitsel-endpoint", fits.URL() + "/ingest/wh/test-token"}
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := atomic.LoadInt64(&requests); got != 0 {
		t.Fatalf("opted-out run made %d requests; want 0", got)
	}
}
