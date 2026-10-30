// Package config loads the athlete's credentials from the environment or a
// config file the tool creates readable only by the athlete (mode 0600).
//
// The athlete's Garmin key, fitsel token and passkey are personal data: the
// tool never logs or prints them, and never stores them anywhere but on the
// athlete's own machine.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Credentials holds the three secrets the tool needs.
type Credentials struct {
	GarminKey     string `json:"garmin_api_key"`
	FitselToken   string `json:"fitsel_token"`
	FitselPasskey string `json:"fitsel_passkey"`
}

// ErrMissingCredentials is returned when a required secret is absent.
var ErrMissingCredentials = errors.New("missing credentials")

// ErrConfigUnreadable is returned when the config file exists but cannot be
// read or is not valid JSON. The error names the path so the athlete knows
// which file to fix.
var ErrConfigUnreadable = errors.New("config file could not be read")

// Load reads the credentials. Environment variables take precedence; any value
// not set in the environment is read from the config file, which is created
// (mode 0600) on first use.
func Load() (Credentials, error) {
	var c Credentials
	// The config file is read first; any field it leaves empty is filled from
	// the environment, so an environment variable wins per key and the file
	// fills the gaps.
	migrateConfig()
	c, err := loadFromFile()
	if err != nil {
		return Credentials{}, err
	}
	var missing []string
	if c.GarminKey = envOr("GARMIN_API_KEY", c.GarminKey); c.GarminKey == "" {
		missing = append(missing, "GARMIN_API_KEY")
	}
	if c.FitselToken = envOr("FITSEL_TOKEN", c.FitselToken); c.FitselToken == "" {
		missing = append(missing, "FITSEL_TOKEN")
	}
	if c.FitselPasskey = envOr("FITSEL_PASSKEY", c.FitselPasskey); c.FitselPasskey == "" {
		missing = append(missing, "FITSEL_PASSKEY")
	}
	if len(missing) > 0 {
		if err := c.writeDefaults(); err != nil {
			return Credentials{}, fmt.Errorf("%w: %v (%s)", ErrMissingCredentials, missing, err)
		}
		return Credentials{}, fmt.Errorf("%w: %v — set them, or edit %s", ErrMissingCredentials, missing, configPath())
	}
	return c, nil
}

// oldConfigDir is the pre-rename home-dir default, kept only so a config left
// there can be found and moved once. It is the single source of the old path,
// so the migration and any check for it agree on it.
const oldConfigDir = ".config/garmin-sync"

// configPath returns the location of the config file, created on first use.
func configPath() string {
	if p := os.Getenv("FITSEL_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("fitsel-client", "config.json")
	}
	return filepath.Join(home, ".config", "fitsel-client", "config.json")
}

// migrateConfig moves a config left in the old home-dir default to the new
// location, so the tool reads the credentials it was already given after the
// directory was renamed. It runs once, the first time Load is called with no
// $FITSEL_CONFIG set, on a machine that still has the old file and not yet the
// new one. It atomically renames the old file to the new path at 0600, so the
// old path is gone the moment the new one exists. It does nothing (and prints
// nothing) if either file is absent, the paths are overridden by $FITSEL_CONFIG,
// or the move has already happened: the new file's presence is what stops a
// repeat.
//
// The old file is trusted as-is — it holds the athlete's own credentials they
// already wrote here — so its contents are moved unchanged, not validated.
func migrateConfig() {
	// The migration is a home-dir-default concern only: a caller who sets
	// $FITSEL_CONFIG has taken explicit control of the path and must not have
	// it silently moved.
	if os.Getenv("FITSEL_CONFIG") != "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	oldPath := filepath.Join(home, oldConfigDir, "config.json")
	newPath := configPath()

	// A move is a one-time event: if the new file is already there, the
	// credentials are where the tool reads them and there is nothing to do.
	if _, err := os.Stat(newPath); err == nil {
		return
	}
	// Only the old file is a migration trigger: if it is gone, there is
	// nothing to move, so the new file is simply created empty on need.
	if _, err := os.Stat(oldPath); err != nil {
		return
	}

	// Move the old file to the new path in one atomic rename (at the hygiene
	// the new default uses), rather than copy-then-remove: a rename either
	// lands or it does not, so a crash or interrupted run can never leave the
	// credentials stranded — the old path is gone the moment the new one
	// exists.
	if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
		return
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		// The rename failed (or the old file vanished mid-move): leave the old
		// file in place so it can be tried again next run, rather than
		// destroying credentials over a transient failure.
		return
	}
	fmt.Fprintf(os.Stderr, "Moved config from %s to %s\n", oldPath, newPath)
}

// Path returns the location of the config file, so a caller can tell the
// athlete where the credentials were saved. It honors $FITSEL_CONFIG and,
// unset, the home-dir default.
func Path() string {
	return configPath()
}

// writeDefaults creates the config file with mode 0600 if it does not exist,
// so the athlete knows where to put secrets they are missing. It never writes
// a secret value; the athlete edits the file by hand.
func (c Credentials) writeDefaults() error {
	dir := filepath.Dir(configPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := configPath()
	if _, err := os.Stat(path); err == nil {
		return nil // already exists
	}
	const sample = `{"garmin_api_key":"<create one at developer.garmin.com>",
"fitsel_token":"<from fitsel's setup page>",
"fitsel_passkey":"<from fitsel's setup page>"}`
	return os.WriteFile(path, []byte(sample), 0o600)
}

// loadFromFile reads the config file, if present. A file that exists but
// cannot be read or is not valid JSON is a clear error, not a missing file:
// the athlete needs to know the file is broken, not that credentials are
// absent.
func loadFromFile() (Credentials, error) {
	b, err := os.ReadFile(configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return Credentials{}, nil // no file yet — not an error
		}
		return Credentials{}, fmt.Errorf("%w: %s: %v", ErrConfigUnreadable, configPath(), err)
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return Credentials{}, fmt.Errorf("%w: %s: not valid JSON: %v", ErrConfigUnreadable, configPath(), err)
	}
	return c, nil
}

// Save writes all three secrets to the home-dir config, the same file Load
// reads, so an interactive setup can store them without the athlete editing
// JSON by hand. It reuses configPath() and the Credentials fields exactly:
// no new store, no new format. The directory is created at 0700 and the file
// at 0600, matching the existing default. It never touches the environment.
func (c Credentials) Save() error {
	dir := filepath.Dir(configPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := configPath()
	return os.WriteFile(path, c.marshal(), 0o600)
}

// marshal renders the credentials as indented JSON with the existing keys,
// so a file written by Save is read back by Load unchanged.
func (c Credentials) marshal() []byte {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return []byte(`{}`)
	}
	return append(b, '\n')
}

// envOr returns the value of the environment variable, or fallback if it is
// unset or empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
