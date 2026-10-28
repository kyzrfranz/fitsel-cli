package garmin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

	acts, err := c.List(context.Background(), tok, "2026-10-01")
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
	tok, err := c.Authorize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	acts, err := c.List(context.Background(), tok, "2027-01-01")
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
	tok, err := c.Authorize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fit, err := c.Download(context.Background(), tok, 1001)
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
		t.Fatal("expected 401 for the corrected header against a server expecting the old one")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401 error, got %v", err)
	}
}
