package garmin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"garmin-sync/internal/fakes"
)

func TestAuthorizeAndList(t *testing.T) {
	g := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer g.Close()
	c := New("test-garmin-key")
	c.SetBaseURL(g.URL())

	tok, err := c.Authorize(context.Background())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if !tok.Valid() {
		t.Fatal("expected a valid token")
	}

	acts, err := c.List(context.Background(), "2026-10-01")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(acts) != 3 {
		t.Fatalf("expected 3 activities, got %d", len(acts))
	}
	if acts[0].ID != 1001 {
		t.Fatalf("expected first activity id 1001, got %d", acts[0].ID)
	}
}

func TestListEmpty(t *testing.T) {
	g := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer g.Close()
	c := New("test-garmin-key")
	c.SetBaseURL(g.URL())
	if _, err := c.Authorize(context.Background()); err != nil {
		t.Fatal(err)
	}
	acts, err := c.List(context.Background(), "2027-01-01")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(acts) != 0 {
		t.Fatalf("expected 0 activities, got %d", len(acts))
	}
}

func TestDownloadUnwrapsZip(t *testing.T) {
	g := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer g.Close()
	c := New("test-garmin-key")
	c.SetBaseURL(g.URL())
	if _, err := c.Authorize(context.Background()); err != nil {
		t.Fatal(err)
	}
	fit, err := c.Download(context.Background(), 1001)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if len(fit) == 0 {
		t.Fatal("expected extracted FIT bytes")
	}
}

// TestAuthorizeUsesApiKeyHeader proves the auth header is "ApiKey <key>", not
// the old "ApiKey ***<key>" (the "***" was a read-masking artifact, per #90).
func TestAuthorizeUsesApiKeyHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		// Plain-text body, as the real endpoint returns.
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "the-token")
	}))
	defer srv.Close()

	c := New("secret-key")
	c.SetBaseURL(srv.URL)
	_, err := c.Authorize(context.Background())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if gotAuth != "ApiKey secret-key" {
		t.Fatalf("auth header = %q, want %q", gotAuth, "ApiKey secret-key")
	}
}

// TestAuthorizeReadsPlainTextBody proves the token is read as the whole plain-text
// body (the real endpoint returns text/plain), trimmed of surrounding whitespace.
func TestAuthorizeReadsPlainTextBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		// A real token response can carry trailing newline/whitespace.
		fmt.Fprint(w, "\n  the-token  \n")
	}))
	defer srv.Close()

	c := New("secret-key")
	c.SetBaseURL(srv.URL)
	tok, err := c.Authorize(context.Background())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if tok.token != "the-token" {
		t.Fatalf("token = %q, want %q (plain-text body should be trimmed)", tok.token, "the-token")
	}
}

// TestAuthorizeRejectsWrongHeader proves the old "ApiKey ***<key>" header fails.
func TestAuthorizeRejectsWrongHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New("secret-key")
	c.SetBaseURL(srv.URL)
	// The client now sends "ApiKey secret-key"; a server that only accepts the
	// old "ApiKey ***secret-key" must reject this.
	_, err := c.Authorize(context.Background())
	if err == nil {
		t.Fatal("expected a 401 for the corrected header against a server expecting the old one")
	}
	// Per #84 a rejected key surfaces as ErrBadKey, not a raw status string.
	if !errors.Is(err, ErrBadKey) {
		t.Fatalf("expected errors.Is(err, ErrBadKey), got %v", err)
	}
}

// TestAuthorizeBadKeyWrapsErrBadKey proves a 401 at the token endpoint surfaces
// as garmin.ErrBadKey, so main can give a bad key its own exit code (per #84).
func TestAuthorizeBadKeyWrapsErrBadKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New("secret-key")
	c.SetBaseURL(srv.URL)
	_, err := c.Authorize(context.Background())
	if !errors.Is(err, ErrBadKey) {
		t.Fatalf("expected errors.Is(err, ErrBadKey), got %v", err)
	}
}

// TestAuthorizeNon401DoesNotWrapErrBadKey proves only a 401 is a bad key: a
// different transport failure (500) must not masquerade as one.
func TestAuthorizeNon401DoesNotWrapErrBadKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New("secret-key")
	c.SetBaseURL(srv.URL)
	_, err := c.Authorize(context.Background())
	if errors.Is(err, ErrBadKey) {
		t.Fatal("500 must not be treated as a bad key")
	}
}

// TestRefreshOnExpiry proves the client asks for a new token when the old one
// has expired, instead of stopping: a token that is already past its expiry is
// refreshed transparently and the run continues.
func TestRefreshOnExpiry(t *testing.T) {
	g := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	defer g.Close()
	c := New("test-garmin-key")
	c.SetBaseURL(g.URL())

	// Authorize once (fresh), then force it stale so the next List refreshes.
	if _, err := c.Authorize(context.Background()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	c.token.expires = time.Now().Add(-time.Hour) // expired

	acts, err := c.List(context.Background(), "2026-10-01")
	if err != nil {
		t.Fatalf("list after expiry: %v", err)
	}
	if len(acts) != 3 {
		t.Fatalf("expected 3 activities after refresh, got %d", len(acts))
	}
	if g.Tokens != 2 {
		t.Fatalf("tokens issued = %d, want 2 (a new token was requested)", g.Tokens)
	}
}

// TestRefreshOn401 proves the client refreshes once when Garmin rejects the
// token mid-run (a 401 on a protected endpoint), continuing after the new one
// is accepted.
func TestRefreshOn401(t *testing.T) {
	g := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	g.UnauthorizedOnce = true // the first protected call 401s, then recovers
	defer g.Close()
	c := New("test-garmin-key")
	c.SetBaseURL(g.URL())

	if _, err := c.Authorize(context.Background()); err != nil {
		t.Fatalf("authorize: %v", err)
	}

	acts, err := c.List(context.Background(), "2026-10-01")
	if err != nil {
		t.Fatalf("list after 401: %v", err)
	}
	if len(acts) != 3 {
		t.Fatalf("expected 3 activities after refresh, got %d", len(acts))
	}
	if g.Tokens != 2 {
		t.Fatalf("tokens issued = %d, want 2 (the 401 triggered a refresh)", g.Tokens)
	}
}

// TestRefreshStill401IsBadKey proves a refresh that 401s again is a bad key,
// not a transient token problem: the key itself is rejected.
func TestRefreshStill401IsBadKey(t *testing.T) {
	g := fakes.NewFakeGarmin("test-garmin-key", fakes.NewActivities())
	g.RefreshFails = true // refresh also 401s
	defer g.Close()
	c := New("test-garmin-key")
	c.SetBaseURL(g.URL())

	if _, err := c.Authorize(context.Background()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	c.token.expires = time.Now().Add(-time.Hour) // force a refresh

	if _, err := c.List(context.Background(), "2026-10-01"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("expected ErrBadKey when the refresh itself 401s, got %v", err)
	}
}

// TestRateLimitedWrapsErrRateLimited proves a 429 at a protected endpoint
// surfaces as garmin.ErrRateLimited, so main can give a rate limit its own
// exit code (per #84).
func TestRateLimitedWrapsErrRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "token") {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "the-token")
			return
		}
		w.Header().Set("Retry-After", "2")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New("secret-key")
	c.SetBaseURL(srv.URL)
	if _, err := c.Authorize(context.Background()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if _, err := c.List(context.Background(), "2026-10-01"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected errors.Is(err, ErrRateLimited), got %v", err)
	}
}

// TestRateLimitedHonoursRetryAfter proves the client waits for the Retry-After
// window before giving up: a 2s header means the back-off took at least that
// long, rather than returning immediately.
func TestRateLimitedHonoursRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "token") {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "the-token")
			return
		}
		w.Header().Set("Retry-After", "2")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New("secret-key")
	c.SetBaseURL(srv.URL)
	if _, err := c.Authorize(context.Background()); err != nil {
		t.Fatalf("authorize: %v", err)
	}

	start := time.Now()
	if _, err := c.List(context.Background(), "2026-10-01"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("back-off took %v, want >= 2s (honoured Retry-After)", elapsed)
	}
}

// TestRateLimitedDoesNotRetry proves the client stops after a single 429 —
// no retry loop that would keep hammering a rate-limited API.
func TestRateLimitedDoesNotRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "token") {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "the-token")
			return
		}
		// Only ever answer the first call; a retry would hit an unclosed
		// server and panic. If we get here twice the client retried.
		if atomic.AddInt32(&calls, 1) > 1 {
			t.Error("client retried a 429; expected to stop after one")
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New("secret-key")
	c.SetBaseURL(srv.URL)
	if _, err := c.Authorize(context.Background()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if _, err := c.List(context.Background(), "2026-10-01"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}
