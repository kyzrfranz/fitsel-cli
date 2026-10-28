package config

import "testing"

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
	t.Setenv("GARMIN_API_KEY", "")
	t.Setenv("FITSEL_TOKEN", "")
	t.Setenv("FITSEL_PASSKEY", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing credentials")
	}
}
