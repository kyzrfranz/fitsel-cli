package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSetPersonalAPIKeyExpiryRoundTrips proves AC1: the expiry is stored in the
// SAME file Load reads (config.Path()) and a later fresh read returns it. It
// mirrors TestSaveThenLoadRoundTrips: $FITSEL_CONFIG points at a temp file, the
// value is written, and a fresh read of the file returns it.
func TestSetPersonalAPIKeyExpiryRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", path)

	creds := Credentials{GarminKey: "k", FitselToken: "t", FitselPasskey: "p"}
	date, err := creds.SetPersonalAPIKeyExpiry("2026-12-31")
	if err != nil {
		t.Fatalf("SetPersonalAPIKeyExpiry: %v", err)
	}
	if date != "2026-12-31" {
		t.Fatalf("SetPersonalAPIKeyExpiry returned %q, want 2026-12-31", date)
	}
	// A fresh read (as a later, separate process would) returns the identical value.
	got, err := loadFromFile()
	if err != nil {
		t.Fatalf("loadFromFile: %v", err)
	}
	if got.PersonalAPIKeyExpiry != "2026-12-31" {
		t.Fatalf("fresh read returned expiry %q, want 2026-12-31", got.PersonalAPIKeyExpiry)
	}
}

// TestSetPersonalAPIKeyExpiryOverwrite proves AC2 (set/change): a second set
// replaces the stored date, and the second value is what reloads.
func TestSetPersonalAPIKeyExpiryOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", path)

	creds := Credentials{GarminKey: "k", FitselToken: "t", FitselPasskey: "p"}
	if _, err := creds.SetPersonalAPIKeyExpiry("2026-01-01"); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if _, err := creds.SetPersonalAPIKeyExpiry("2027-06-15"); err != nil {
		t.Fatalf("second set: %v", err)
	}
	got, err := loadFromFile()
	if err != nil {
		t.Fatalf("loadFromFile: %v", err)
	}
	if got.PersonalAPIKeyExpiry != "2027-06-15" {
		t.Fatalf("overloaded expiry %q, want 2027-06-15", got.PersonalAPIKeyExpiry)
	}
}

// TestSetPersonalAPIKeyExpiryMissingProvesOptional proves AC2 (reports missing):
// with no date stored, the field reloads as "" (blank is allowed, not an error).
func TestSetPersonalAPIKeyExpiryMissingProvesOptional(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", path)

	// A config with the three secrets but no expiry: it reloads with field "".
	creds := Credentials{GarminKey: "k", FitselToken: "t", FitselPasskey: "p"}
	if err := creds.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := loadFromFile()
	if err != nil {
		t.Fatalf("loadFromFile: %v", err)
	}
	if got.PersonalAPIKeyExpiry != "" {
		t.Fatalf("expiry %q, want blank (optional)", got.PersonalAPIKeyExpiry)
	}
}

// TestSetPersonalAPIKeyExpiryRejectsGarbage proves AC3: an invalid date is
// rejected with an error and writes nothing — the prior value stays intact.
func TestSetPersonalAPIKeyExpiryRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", path)

	// A valid date already stored.
	creds := Credentials{GarminKey: "k", FitselToken: "t", FitselPasskey: "p"}
	if _, err := creds.SetPersonalAPIKeyExpiry("2026-03-03"); err != nil {
		t.Fatalf("initial set: %v", err)
	}

	for _, bad := range []string{"2026-13-01", "2026-02-30", "2026-3-3", "garbage", "2026-03-03T00:00:00", "03/03/2026"} {
		if _, err := creds.SetPersonalAPIKeyExpiry(bad); err == nil {
			t.Fatalf("expected rejection for %q, got nil", bad)
		}
	}

	// Nothing was changed: the file is still valid JSON with the prior date.
	got, err := loadFromFile()
	if err != nil {
		t.Fatalf("loadFromFile after garbage: %v", err)
	}
	if got.PersonalAPIKeyExpiry != "2026-03-03" {
		t.Fatalf("expiry changed to %q after a rejected set, want 2026-03-03", got.PersonalAPIKeyExpiry)
	}
	var raw map[string]any
	if err := json.Unmarshal(mustRead(t, path), &raw); err != nil {
		t.Fatalf("file is not valid JSON after a rejected set: %v", err)
	}
	if raw["personal_api_key_expiry"] != "2026-03-03" {
		t.Fatalf("stored value changed to %v, want 2026-03-03", raw["personal_api_key_expiry"])
	}
}

// TestSetPersonalAPIKeyExpiryRejectsGarbageOnFresh proves AC3 holds when no
// valid date was already stored: a rejected set on a fresh config writes no
// empty value (the field is absent, not "").
func TestSetPersonalAPIKeyExpiryRejectsGarbageOnFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", path)

	var creds Credentials
	if _, err := creds.SetPersonalAPIKeyExpiry("not-a-date"); err == nil {
		t.Fatal("expected rejection for garbage on a fresh config")
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		// If a file was written at all, it must be an empty object (no empty
		// value), never a file with the field set to "".
		if string(raw) != "{}\n" {
			t.Fatalf("fresh rejected set wrote %q, want empty object {} (no empty value)", raw)
		}
	}
}

// TestSetPersonalAPIKeyExpiryPersists0600 proves AC4: the file is written at
// mode 0600 after a set.
func TestSetPersonalAPIKeyExpiryPersists0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", path)

	creds := Credentials{GarminKey: "k", FitselToken: "t", FitselPasskey: "p"}
	if _, err := creds.SetPersonalAPIKeyExpiry("2026-12-31"); err != nil {
		t.Fatalf("set: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config mode = %o, want 0600", perm)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	return b
}
