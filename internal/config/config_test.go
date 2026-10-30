package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestMissingNamesAbsentKeys checks the missing-credentials error lists exactly
// the absent keys and carries no secret or placeholder value (check a).
func TestMissingNamesAbsentKeys(t *testing.T) {
	t.Setenv("FITSEL_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("GARMIN_API_KEY", "")
	t.Setenv("FITSEL_TOKEN", "")
	t.Setenv("FITSEL_PASSKEY", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing credentials")
	}
	msg := err.Error()
	for _, want := range []string{"GARMIN_API_KEY", "FITSEL_TOKEN", "FITSEL_PASSKEY"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing error should name %q, got %q", want, msg)
		}
	}
	for _, bad := range []string{"<create one at", "<from fitsel"} {
		if strings.Contains(msg, bad) {
			t.Errorf("missing error must not contain a placeholder value, got %q", msg)
		}
	}
}

// TestUnreadableConfig checks a config file that exists but cannot be read
// (here, the path is a directory) yields the distinct ErrConfigUnreadable, not
// a misleading "missing credentials" (check a).
func TestUnreadableConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FITSEL_CONFIG", dir) // a directory: exists, cannot be read as a file
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for unreadable config")
	}
	if !errors.Is(err, ErrConfigUnreadable) {
		t.Fatalf("expected ErrConfigUnreadable, got %v", err)
	}
	if strings.Contains(err.Error(), "missing credentials") {
		t.Fatalf("unreadable config must not masquerade as missing credentials: %v", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("unreadable error should name the path %q, got %v", dir, err)
	}
}

// TestMalformedConfig checks a config file with invalid JSON yields the
// distinct ErrConfigUnreadable, not a silent "missing credentials" (check a).
func TestMalformedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FITSEL_CONFIG", path)
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for malformed config")
	}
	if !errors.Is(err, ErrConfigUnreadable) {
		t.Fatalf("expected ErrConfigUnreadable, got %v", err)
	}
}

// TestDefaultsCreatedAt0600 checks the config file is created mode 0600 and its
// directory mode 0700 (check b).
func TestDefaultsCreatedAt0600(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "garmin-sync")
	path := filepath.Join(dir, "config.json")
	t.Setenv("FITSEL_CONFIG", path)
	t.Setenv("GARMIN_API_KEY", "")
	t.Setenv("FITSEL_TOKEN", "")
	t.Setenv("FITSEL_PASSKEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected missing-credentials error")
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

// TestNoSecretInPartialConfigError checks that when the file is written from a
// partial config the returned error carries no present secret or placeholder
// value (check c).
func TestNoSecretInPartialConfigError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("FITSEL_CONFIG", path)
	t.Setenv("GARMIN_API_KEY", "") // triggers writeDefaults + missing error
	t.Setenv("FITSEL_TOKEN", "")
	t.Setenv("FITSEL_PASSKEY", "")
	// Now set only the Garmin key so the file is written but the other two are
	// missing. The Garmin key is a real secret that must not leak into output.
	t.Setenv("GARMIN_API_KEY", "supersecretkey-should-not-appear")
	_, err := Load()
	if err == nil {
		t.Fatal("expected missing-credentials error")
	}
	msg := err.Error()
	if strings.Contains(msg, "supersecretkey-should-not-appear") {
		t.Fatalf("error leaked the present secret: %q", msg)
	}
	for _, bad := range []string{"<create one at", "<from fitsel"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("error leaked a placeholder value: %q", msg)
		}
	}
}

// TestMigratesOldConfigDir checks that a config left in the pre-rename
// ~/.config/garmin-sync directory is read and moved to the new
// ~/.config/fitsel-client directory on first Load, once. The old file's
// contents survive unchanged, the old file is removed, and a second Load reads
// from the new path without migrating again. HOME is a temp dir so the default
// paths land in a throwaway location.
func TestMigratesOldConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("FITSEL_CONFIG", "") // use the home-dir default, not $FITSEL_CONFIG

	oldConfig := filepath.Join(dir, ".config", "garmin-sync", "config.json")
	if err := os.MkdirAll(filepath.Dir(oldConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	const sample = `{"garmin_api_key":"old-key","fitsel_token":"old-token","fitsel_passkey":"old-passkey"}`
	if err := os.WriteFile(oldConfig, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}

	newConfig := filepath.Join(dir, ".config", "fitsel-client", "config.json")

	// First Load: reads the old file's values, moves it to the new path.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GarminKey != "old-key" || c.FitselToken != "old-token" || c.FitselPasskey != "old-passkey" {
		t.Fatalf("Load did not read the migrated config: %+v", c)
	}
	// The old file is gone.
	if _, err := os.Stat(oldConfig); !os.IsNotExist(err) {
		t.Fatalf("old config was not removed, stat err = %v", err)
	}
	// The new file exists with the same contents.
	got, err := os.ReadFile(newConfig)
	if err != nil {
		t.Fatalf("new config not written: %v", err)
	}
	if string(got) != sample {
		t.Fatalf("new config contents = %q, want %q", got, sample)
	}
	if fi, err := os.Stat(newConfig); err != nil {
		t.Fatal(err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("new config mode = %o, want 600", perm)
	}

	// Second Load: reads from the new path, migrates nothing (old is gone).
	c2, err := Load()
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if c2.GarminKey != "old-key" {
		t.Fatalf("second Load did not read migrated config: %+v", c2)
	}
}
