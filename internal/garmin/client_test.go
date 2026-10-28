package garmin

import (
	"context"
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
