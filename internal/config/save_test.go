package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveThenLoadRoundTrips checks that Save writes all three secrets to the
// home-dir config and Load reads them back unchanged — the write primitive the
// interactive init command relies on.
func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("FITSEL_CONFIG", path)

	orig := Credentials{
		GarminKey:     "garmin-key",
		FitselToken:   "fitsel-token",
		FitselPasskey: "fitsel-passkey",
	}
	if err := orig.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != orig {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, orig)
	}
}

// TestSaveHonorsFITSELConfig checks Save writes to the $FITSEL_CONFIG path.
func TestSaveHonorsFITSELConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("FITSEL_CONFIG", path)
	c := Credentials{GarminKey: "k", FitselToken: "t", FitselPasskey: "p"}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config was not written to %s: %v", path, err)
	}
}

// TestSaveCreatesDir0700File0600 checks the directory is created at 0700 and
// the file at 0600, matching the existing default's hygiene.
func TestSaveCreatesDir0700File0600(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "garmin-sync")
	path := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", path)
	c := Credentials{GarminKey: "k", FitselToken: "t", FitselPasskey: "p"}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal("config file was not created:", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config file mode = %o, want 600", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal("config dir was not created:", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("config dir mode = %o, want 700", perm)
	}
}

// TestSaveOverwritesExisting checks a second Save replaces the file rather than
// appending, so the stored values always match the latest input.
func TestSaveOverwritesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("FITSEL_CONFIG", path)
	first := Credentials{GarminKey: "first", FitselToken: "first", FitselPasskey: "first"}
	if err := first.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	second := Credentials{GarminKey: "second", FitselToken: "second", FitselPasskey: "second"}
	if err := second.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.GarminKey != "second" || got.FitselToken != "second" || got.FitselPasskey != "second" {
		t.Fatalf("Save did not overwrite: got %+v", got)
	}
}
