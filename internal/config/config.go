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

// configPath returns the location of the config file, created on first use.
func configPath() string {
	if p := os.Getenv("FITSEL_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "garmin-sync", "config.json")
	}
	return filepath.Join(home, ".config", "garmin-sync", "config.json")
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
