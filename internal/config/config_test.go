package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("GARMIN_API_KEY", "k")
	t.Setenv("FITSEL_TOKEN", "t")
	t.Setenv("FITSEL_PASSKEY", "p")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.GarminKey != "k" || c.FitselToken != "t" || c.FitselPasskey != "p" {
		t.Fatalf("unexpected creds: %+v", c)
	}
}

func TestMissingCreds(t *testing.T) {
	t.Setenv("FITSEL_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("GARMIN_API_KEY", "")
	t.Setenv("FITSEL_TOKEN", "")
	t.Setenv("FITSEL_PASSKEY", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing credentials")
	}
}

// TestConfigFileFallback checks that a value absent from the environment but
// present in the config file is loaded — the fallback the design mandates.
func TestConfigFileFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("FITSEL_CONFIG", path)
	const sample = `{"garmin_api_key":"from-file","fitsel_token":"from-file","fitsel_passkey":"from-file"}`
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	// No environment set: all three come from the file.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.GarminKey != "from-file" || c.FitselToken != "from-file" || c.FitselPasskey != "from-file" {
		t.Fatalf("expected file creds, got %+v", c)
	}
}

// TestEnvWinsOverFile checks that an environment variable wins over the file
// value for the same key.
func TestEnvWinsOverFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("FITSEL_CONFIG", path)
	const sample = `{"garmin_api_key":"from-file","fitsel_token":"from-file","fitsel_passkey":"from-file"}`
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GARMIN_API_KEY", "from-env")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.GarminKey != "from-env" {
		t.Fatalf("env should win for GARMIN_API_KEY, got %q", c.GarminKey)
	}
	if c.FitselToken != "from-file" || c.FitselPasskey != "from-file" {
		t.Fatalf("file should fill the unset keys, got %+v", c)
	}
}
