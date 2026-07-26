package main

import (
	"strings"
	"testing"
	"time"
)

// envMap builds a getenv backed by a map, for env-free config tests.
func envMap(m map[string]string) getenv {
	return func(k string) string { return m[k] }
}

func TestParseConfigDefaultInterval(t *testing.T) {
	cfg, err := parseConfig(envMap(map[string]string{
		"DATABASE_URL": "postgres://x/y",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.PollInterval != 15*time.Second {
		t.Fatalf("PollInterval = %v, want 15s default", cfg.PollInterval)
	}
	if cfg.DatabaseURL != "postgres://x/y" {
		t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
	}
}

func TestParseConfigValidInterval(t *testing.T) {
	cfg, err := parseConfig(envMap(map[string]string{
		"DATABASE_URL":            "postgres://x/y",
		"RETROSYNC_POLL_INTERVAL": "1m30s",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.PollInterval != 90*time.Second {
		t.Fatalf("PollInterval = %v, want 90s", cfg.PollInterval)
	}
}

func TestParseConfigInvalidInterval(t *testing.T) {
	_, err := parseConfig(envMap(map[string]string{
		"DATABASE_URL":            "postgres://x/y",
		"RETROSYNC_POLL_INTERVAL": "not-a-duration",
	}))
	if err == nil {
		t.Fatal("want error for unparseable RETROSYNC_POLL_INTERVAL")
	}
	if !strings.Contains(err.Error(), "RETROSYNC_POLL_INTERVAL") {
		t.Fatalf("error %q should name the offending var", err)
	}
}

func TestParseConfigNonPositiveInterval(t *testing.T) {
	for _, raw := range []string{"0s", "-5s"} {
		_, err := parseConfig(envMap(map[string]string{
			"DATABASE_URL":            "postgres://x/y",
			"RETROSYNC_POLL_INTERVAL": raw,
		}))
		if err == nil {
			t.Fatalf("want error for non-positive interval %q", raw)
		}
	}
}

func TestParseConfigDefaultPollTimeout(t *testing.T) {
	cfg, err := parseConfig(envMap(map[string]string{
		"DATABASE_URL": "postgres://x/y",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.PollTimeout != 60*time.Second {
		t.Fatalf("PollTimeout = %v, want 60s default", cfg.PollTimeout)
	}
}

func TestParseConfigValidPollTimeout(t *testing.T) {
	cfg, err := parseConfig(envMap(map[string]string{
		"DATABASE_URL":           "postgres://x/y",
		"RETROSYNC_POLL_TIMEOUT": "90s",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.PollTimeout != 90*time.Second {
		t.Fatalf("PollTimeout = %v, want 90s", cfg.PollTimeout)
	}
}

func TestParseConfigInvalidPollTimeout(t *testing.T) {
	_, err := parseConfig(envMap(map[string]string{
		"DATABASE_URL":           "postgres://x/y",
		"RETROSYNC_POLL_TIMEOUT": "not-a-duration",
	}))
	if err == nil {
		t.Fatal("want error for unparseable RETROSYNC_POLL_TIMEOUT")
	}
	if !strings.Contains(err.Error(), "RETROSYNC_POLL_TIMEOUT") {
		t.Fatalf("error %q should name the offending var", err)
	}
}

func TestParseConfigNonPositivePollTimeout(t *testing.T) {
	for _, raw := range []string{"0s", "-5s"} {
		_, err := parseConfig(envMap(map[string]string{
			"DATABASE_URL":           "postgres://x/y",
			"RETROSYNC_POLL_TIMEOUT": raw,
		}))
		if err == nil {
			t.Fatalf("want error for non-positive timeout %q", raw)
		}
	}
}

func TestParseConfigDefaultShareRoot(t *testing.T) {
	cfg, err := parseConfig(envMap(map[string]string{
		"DATABASE_URL": "postgres://x/y",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.ShareRoot != "/shares" {
		t.Fatalf("ShareRoot = %q, want /shares default", cfg.ShareRoot)
	}
}

func TestParseConfigRelativeShareRoot(t *testing.T) {
	_, err := parseConfig(envMap(map[string]string{
		"DATABASE_URL":         "postgres://x/y",
		"RETROSYNC_SHARE_ROOT": "shares",
	}))
	if err == nil {
		t.Fatal("want error for a relative share root")
	}
	if !strings.Contains(err.Error(), "RETROSYNC_SHARE_ROOT") {
		t.Fatalf("error %q should name the offending var", err)
	}
}

func TestParseConfigOverrideShareRoot(t *testing.T) {
	cfg, err := parseConfig(envMap(map[string]string{
		"DATABASE_URL":         "postgres://x/y",
		"RETROSYNC_SHARE_ROOT": "/mnt/syncthing",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.ShareRoot != "/mnt/syncthing" {
		t.Fatalf("ShareRoot = %q, want /mnt/syncthing", cfg.ShareRoot)
	}
}

func TestParseConfigMissingDatabaseURL(t *testing.T) {
	_, err := parseConfig(envMap(map[string]string{}))
	if err == nil {
		t.Fatal("want error when DATABASE_URL is unset")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("error %q should name DATABASE_URL", err)
	}
}

// The config parser must never echo the DSN value (it can embed a password).
func TestParseConfigErrorNeverLeaksDSN(t *testing.T) {
	const secret = "postgres://user:s3cr3t@host/db"
	_, err := parseConfig(envMap(map[string]string{
		"DATABASE_URL":            secret,
		"RETROSYNC_POLL_INTERVAL": "bad",
	}))
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked DSN/secret: %q", err)
	}
}
