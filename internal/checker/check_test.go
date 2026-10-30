package checker

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// setVersion points the built-in version at a known value for the duration of a
// test, restoring the original on cleanup. The default is "0.0.0", so a test
// that does not call it compares against that.
func setVersion(t *testing.T, v string) {
	t.Helper()
	orig := version
	version = v
	t.Cleanup(func() { version = orig })
}

// fakeGitHub starts an httptest server that serves /releases/latest with the
// given status and body, and counts how many requests it received. A test
// points Base at its URL and reads requests to assert exactly one call (or
// zero, on the skip paths).
func fakeGitHub(t *testing.T, status int, body string) (*httptest.Server, *int64) {
	t.Helper()
	var requests int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requests, 1)
		// The one header the acceptance pins: the GitHub media type.
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept header = %q, want application/vnd.github+json", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// testOptions returns Options pointed at the fake GitHub with a temp state dir,
// so the once-per-day timestamp is written to a throwaway location and never
// touches the athlete's real state.
func testOptions(t *testing.T, base string) Options {
	t.Helper()
	return Options{
		Base:     base,
		StateDir: t.TempDir(),
		Now:      time.Now(),
	}
}

// TestNewerVersionReturnsHint is the headline check: against a fake GitHub that
// reports a newer release, Run returns the one-line hint carrying both tokens
// the acceptance pins — the release URL and the brew command.
func TestNewerVersionReturnsHint(t *testing.T) {
	setVersion(t, "0.1.0")
	srv, requests := fakeGitHub(t, http.StatusOK, `{"tag_name":"v0.2.0","html_url":"https://github.com/kyzrfranz/fitsel-cli/releases/tag/v0.2.0"}`)

	hint, err := Run(testOptions(t, srv.URL))
	if err != nil {
		t.Fatalf("Run returned an error on the success path: %v", err)
	}
	if hint == nil {
		t.Fatal("expected a hint for a newer release, got nil")
	}
	if !strings.Contains(hint.URL, "https://github.com/kyzrfranz/fitsel-cli/releases/tag/v0.2.0") {
		t.Fatalf("hint.URL = %q, want the release URL", hint.URL)
	}
	if hint.BrewCmd != "brew upgrade fitsel-client" {
		t.Fatalf("hint.BrewCmd = %q, want brew upgrade fitsel-client", hint.BrewCmd)
	}
	// Both tokens on one line, exact wording from the design.
	if want := `Update available — https://github.com/kyzrfranz/fitsel-cli/releases/tag/v0.2.0 — run "brew upgrade fitsel-client"`; hint.Text != want {
		t.Fatalf("hint.Text = %q, want %q", hint.Text, want)
	}
	if got := atomic.LoadInt64(requests); got != 1 {
		t.Fatalf("expected exactly one request, got %d", got)
	}
}

// TestEqualOrOlderReturnsNoHint proves the check is silent when the latest
// release is equal to or older than the built-in version: no hint, no crash.
func TestEqualOrOlderReturnsNoHint(t *testing.T) {
	cases := []struct {
		name string
		cur  string
		tag  string
	}{
		{"equal", "0.1.0", "v0.1.0"},
		{"older", "0.2.0", "v0.1.0"},
		{"olderMinor", "1.2.0", "v1.1.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setVersion(t, tc.cur)
			srv, requests := fakeGitHub(t, http.StatusOK, `{"tag_name":"`+tc.tag+`","html_url":"https://github.com/kyzrfranz/fitsel-cli/releases/tag/`+tc.tag+`"}`)
			hint, err := Run(testOptions(t, srv.URL))
			if err != nil {
				t.Fatalf("no error expected for equal/older, got %v", err)
			}
			if hint != nil {
				t.Fatalf("expected no hint for %s <= %s, got %q", tc.cur, tc.tag, hint.Text)
			}
			if got := atomic.LoadInt64(requests); got != 1 {
				t.Fatalf("expected exactly one request, got %d", got)
			}
		})
	}
}

// TestDevBuildSkipsRequest proves a dev build (version contains "-dev") makes no
// request at all, even on the very first run with no timestamp.
func TestDevBuildSkipsRequest(t *testing.T) {
	setVersion(t, "0.2.0-dev")
	srv, requests := fakeGitHub(t, http.StatusOK, `{"tag_name":"v0.3.0","html_url":"https://example.com"}`)

	hint, err := Run(testOptions(t, srv.URL))
	if !errors.Is(err, ErrDevBuild) {
		t.Fatalf("expected ErrDevBuild, got %v", err)
	}
	if hint != nil {
		t.Fatalf("dev build should not return a hint, got %q", hint.Text)
	}
	if got := atomic.LoadInt64(requests); got != 0 {
		t.Fatalf("dev build should make no request, got %d", got)
	}
}

// TestOptedOutSkipsRequest proves the opt-out (--no-update-check) makes no
// request, even on the first run.
func TestOptedOutSkipsRequest(t *testing.T) {
	setVersion(t, "0.1.0")
	srv, requests := fakeGitHub(t, http.StatusOK, `{"tag_name":"v0.2.0","html_url":"https://example.com"}`)

	opts := testOptions(t, srv.URL)
	opts.NoUpdateCheck = true
	hint, err := Run(opts)
	if !errors.Is(err, ErrOptedOut) {
		t.Fatalf("expected ErrOptedOut, got %v", err)
	}
	if hint != nil {
		t.Fatalf("opt-out should not return a hint, got %q", hint.Text)
	}
	if got := atomic.LoadInt64(requests); got != 0 {
		t.Fatalf("opt-out should make no request, got %d", got)
	}
}

// TestThrottleSkipsSecondRunWithin24h proves the once-per-day limit: a second run
// within 24h of the saved timestamp makes no request, while the first run does.
func TestThrottleSkipsSecondRunWithin24h(t *testing.T) {
	setVersion(t, "0.1.0")
	srv, requests := fakeGitHub(t, http.StatusOK, `{"tag_name":"v0.2.0","html_url":"https://example.com"}`)
	opts := testOptions(t, srv.URL)

	// First run: no timestamp yet, so it checks (one request) and records now.
	if _, err := Run(opts); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := atomic.LoadInt64(requests); got != 1 {
		t.Fatalf("first run: expected one request, got %d", got)
	}

	// Second run 1h later: throttled, no request.
	opts.Now = opts.Now.Add(time.Hour)
	if _, err := Run(opts); !errors.Is(err, ErrThrottled) {
		t.Fatalf("second run: expected ErrThrottled, got %v", err)
	}
	if got := atomic.LoadInt64(requests); got != 1 {
		t.Fatalf("throttled run should make no additional request, got %d", got)
	}

	// Third run 23h after the second (24h after the first): just past the
	// window, checks again.
	opts.Now = opts.Now.Add(23 * time.Hour)
	if _, err := Run(opts); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if got := atomic.LoadInt64(requests); got != 2 {
		t.Fatalf("third run: expected a second request, got %d", got)
	}
}

// TestThrottleFirstRunAfterWindow proves a timestamp older than 24h is not
// throttled: the check runs again.
func TestThrottleFirstRunAfterWindow(t *testing.T) {
	setVersion(t, "0.1.0")
	srv, requests := fakeGitHub(t, http.StatusOK, `{"tag_name":"v0.2.0","html_url":"https://example.com"}`)
	opts := testOptions(t, srv.URL)

	// Write a timestamp 25h in the past directly.
	path := filepath.Join(opts.StateDir, "update-check.last")
	if err := os.WriteFile(path, []byte(time.Now().Add(-25*time.Hour).UTC().Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(opts); err != nil {
		t.Fatalf("run after window: %v", err)
	}
	if got := atomic.LoadInt64(requests); got != 1 {
		t.Fatalf("expected one request after the window, got %d", got)
	}
}

// TestFailureSwallowed covers every failure the acceptance lists: a 500, a
// malformed body, a dropped connection, and a server that is too slow to answer
// within the 2s timeout. Each must return a swallowed error (no hint, no panic)
// and still make exactly one request.
func TestFailureSwallowed(t *testing.T) {
	t.Run("500", func(t *testing.T) {
		setVersion(t, "0.1.0")
		srv, requests := fakeGitHub(t, http.StatusInternalServerError, `{"tag_name":"v0.2.0","html_url":"https://example.com"}`)
		hint, err := Run(testOptions(t, srv.URL))
		assertSwallowed(t, err, hint, requests)
	})

	t.Run("malformed body", func(t *testing.T) {
		setVersion(t, "0.1.0")
		srv, requests := fakeGitHub(t, http.StatusOK, `{not valid json`)
		hint, err := Run(testOptions(t, srv.URL))
		assertSwallowed(t, err, hint, requests)
	})

	t.Run("connection closed", func(t *testing.T) {
		setVersion(t, "0.1.0")
		var requests int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&requests, 1)
			r.Body.Close() // drop the connection before writing a response
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)
		hint, err := Run(testOptions(t, srv.URL))
		assertSwallowed(t, err, hint, &requests)
	})

	t.Run("slow server times out", func(t *testing.T) {
		setVersion(t, "0.1.0")
		var requests int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&requests, 1)
			// Sleep longer than the 2s client timeout so the request is
			// aborted by the deadline, exercising the >2s timeout path.
			time.Sleep(3 * time.Second)
		}))
		t.Cleanup(srv.Close)
		start := time.Now()
		hint, err := Run(testOptions(t, srv.URL))
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("check took %s, the 2s timeout was not honoured", elapsed)
		}
		assertSwallowed(t, err, hint, &requests)
	})
}

// assertSwallowed proves a failure path made exactly one request, returned a
// swallowed error (not a nil), and produced no hint. The error is wrapped in
// the "no request made" outcome, so it branches as a skip, never as a reported
// failure.
func assertSwallowed(t *testing.T, err error, hint *Hint, requests *int64) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a swallowed error on the failure path, got nil")
	}
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("swallowed failure should be wrapped in ErrDisabled, got %v", err)
	}
	if hint != nil {
		t.Fatalf("failure path should not return a hint, got %q", hint.Text)
	}
	if got := atomic.LoadInt64(requests); got != 1 {
		t.Fatalf("expected exactly one request on the failure path, got %d", got)
	}
}

// TestAcceptHeaderProven is a standalone assertion that the request the checker
// makes carries the exact GitHub media type, so the header contract is pinned
// independently of the response handling.
func TestAcceptHeaderProven(t *testing.T) {
	setVersion(t, "0.1.0")
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"tag_name":"v0.1.0","html_url":"https://example.com"}`)
	}))
	t.Cleanup(srv.Close)
	if _, err := Run(testOptions(t, srv.URL)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Fatalf("Accept header = %q, want application/vnd.github+json", gotAccept)
	}
}

// TestBaseIsInjectable proves the base URL is used as-is for the GET, so a test
// (and a future internal mirror) can point the check at any host.
func TestBaseIsInjectable(t *testing.T) {
	setVersion(t, "0.1.0")
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"tag_name":"v0.2.0","html_url":"https://example.com"}`)
	}))
	t.Cleanup(srv.Close)
	opts := testOptions(t, srv.URL)
	if _, err := Run(opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/releases/latest" {
		t.Fatalf("request path = %q, want /releases/latest", gotPath)
	}
}

// TestStripV proves the leading-v strip on both sides of the compare.
func TestStripV(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v0.1.0", "0.1.0"},
		{"V0.1.0", "0.1.0"},
		{"0.1.0", "0.1.0"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := stripV(tc.in); got != tc.want {
			t.Fatalf("stripV(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCompareComponentWise proves the numeric compare handles the components
// independently: a newer minor with equal major wins, a newer major wins, an
// older patch loses even with a newer minor is not possible (older is older).
func TestCompareComponentWise(t *testing.T) {
	setVersion(t, "1.2.0")
	cases := []struct {
		tag    string
		wantOK bool
	}{
		{"v1.2.1", true},  // newer patch
		{"v1.3.0", true},  // newer minor
		{"v2.0.0", true},  // newer major
		{"v1.2.0", false}, // equal
		{"v1.1.9", false}, // older minor
		{"v0.9.9", false}, // older major
	}
	for _, tc := range cases {
		t.Run(tc.tag, func(t *testing.T) {
			rel := githubRelease{TagName: tc.tag, HTMLURL: "https://example.com/releases/" + tc.tag}
			hint := compare(rel)
			if tc.wantOK && hint == nil {
				t.Fatalf("compare(%q) = nil, want a hint", tc.tag)
			}
			if !tc.wantOK && hint != nil {
				t.Fatalf("compare(%q) = %q, want no hint", tc.tag, hint.Text)
			}
		})
	}
}
