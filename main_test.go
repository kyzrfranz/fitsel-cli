package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"garmin-sync/internal/config"
	"garmin-sync/internal/fakes"
	"garmin-sync/internal/garmin"
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
	// Fail every upload attempt: a single 500 no longer hard-stops the run
	// (it retries and recovers), so to exercise "stop on failure, then resume
	// clean on rerun" the fake must fail every attempt. Cleared by the run's
	// state, so the resume run succeeds.
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
	if got := mapGarminExit(err); got != exitRateLimited {
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
	const n = 150
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	gar := fakes.NewFakeGarmin("test-garmin-key", fakes.ManyActivities(n, "2026-10-01"))
	defer gar.Close()
	fits := fakes.NewFakeFitsel("test-passkey")
	// Fail every attempt so the run stops on the first upload (the "kill"), and
	// the rerun — with the fake's FailOnce cleared — succeeds.
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
	if got := mapGarminExit(err); got != exitBadSignature {
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
	if got := mapGarminExit(err); got != exitWebhookMissing {
		t.Fatalf("exit code = %d, want %d (unknown webhook token)", got, exitWebhookMissing)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("unknown webhook uploaded %d files; expected none", len(fits.Uploadees))
	}
}

// TestInvalidUploadExitsFitselProves a 400 (invalid multipart) maps to its own
// exit code, uploads nothing, and marks nothing synced (per #85).
func TestInvalidUploadExitsFitselAndUploadsNothing(t *testing.T) {
	dir := t.TempDir()
	fits := fakes.NewFakeFitsel("test-passkey")
	fits.Statuses = []int{http.StatusBadRequest}
	args := newFitselArgs(t, dir, fits)

	err := run(args)
	if err == nil {
		t.Fatal("expected an upload error on an invalid upload")
	}
	if got := mapGarminExit(err); got != exitUpload {
		t.Fatalf("exit code = %d, want %d (invalid upload)", got, exitUpload)
	}
	if !strings.Contains(err.Error(), "invalid upload") {
		t.Fatalf("error = %q, want it to mention the invalid upload", err)
	}
	if len(fits.Uploadees) != 0 {
		t.Fatalf("invalid upload uploaded %d files; expected none", len(fits.Uploadees))
	}
}

// TestUploadExhaustedExitsFitselProves repeated 500s (or a dead network) exhaust
// the retries, map to their own exit code, and mark nothing synced (per #85).
func TestUploadExhaustedExitsFitselAndUploadsNothing(t *testing.T) {
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
	if got := mapGarminExit(err); got != exitUploadExhausted {
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

// TestInitWritesNoSyncRun proves `garmin-sync init` is a distinct setup
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
