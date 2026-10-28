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
	GarminKey  string
	FitselToken string
	FitselPasskey string
}

// ErrMissingCredentials is returned when a required secret is absent.
var ErrMissingCredentials = errors.New("missing credentials")

// Load reads the credentials. Environment variables take precedence; any value
// not set in the environment is read from the config file, which is created
// (mode 0600) on first use.
func Load() (Credentials, error) {
	var c Credentials
	var missing []string
	if c.GarminKey = os.Getenv("GARMIN_API_KEY"); c.GarminKey == "" {
		missing = append(missing, "GARMIN_API_KEY")
	}
	if c.FitselToken = os.Getenv("FITSEL_TOKEN"); c.FitselToken == "" {
		missing = append(missing, "FITSEL_TOKEN")
	}
	if c.FitselPasskey = os.Getenv("FITSEL_PASSKEY"); c.FitselPasskey == "" {
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

// loadFromFile reads the config file, if present.
func loadFromFile() Credentials {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return Credentials{}
	}
	var c Credentials
	_ = json.Unmarshal(b, &c)
	return c
}
