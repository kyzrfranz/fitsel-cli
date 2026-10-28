package state

import (
	"path/filepath"
	"testing"
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
	s.MarkDone("1001")
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
	if err := s.Save(); err != nil {
		t.Fatalf("saving empty state should be a no-op, got %v", err)
	}
}
