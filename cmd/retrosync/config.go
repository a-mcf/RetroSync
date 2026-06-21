package main

import (
	"fmt"
	"time"
)

// defaultPollInterval is the active-poll cadence when RETROSYNC_POLL_INTERVAL
// is unset (docs/state-machine.md, "Active poll loop": default 15s).
const defaultPollInterval = 15 * time.Second

// defaultPollTimeout bounds a single game's poll when RETROSYNC_POLL_TIMEOUT is
// unset. Generous: saves are tiny, so this only fires on a genuinely hung node.
const defaultPollTimeout = 60 * time.Second

// config holds the parsed, validated process configuration.
type config struct {
	// DatabaseURL is the Postgres DSN (DATABASE_URL). Required.
	DatabaseURL string
	// PollInterval is the sweep cadence (RETROSYNC_POLL_INTERVAL), default 15s.
	PollInterval time.Duration
	// PollTimeout bounds each individual game's poll (RETROSYNC_POLL_TIMEOUT),
	// default 60s.
	PollTimeout time.Duration
}

// getenv abstracts os.Getenv so the parser is testable without touching the
// process environment.
type getenv func(key string) string

// parseConfig reads and validates configuration from env via lookup.
//
//   - DATABASE_URL is required; absent/empty is an error.
//   - RETROSYNC_POLL_INTERVAL is an optional Go duration (e.g. "15s", "1m");
//     absent uses defaultPollInterval, unparseable or non-positive is an error.
//   - RETROSYNC_POLL_TIMEOUT is an optional Go duration bounding each game's
//     poll; absent uses defaultPollTimeout, unparseable or non-positive is an
//     error.
//
// The returned error never echoes the DATABASE_URL value (it may embed a
// password); only its absence is reported.
func parseConfig(lookup getenv) (config, error) {
	dsn := lookup("DATABASE_URL")
	if dsn == "" {
		return config{}, fmt.Errorf("DATABASE_URL is required")
	}

	interval, err := parsePositiveDuration(lookup, "RETROSYNC_POLL_INTERVAL", defaultPollInterval)
	if err != nil {
		return config{}, err
	}

	timeout, err := parsePositiveDuration(lookup, "RETROSYNC_POLL_TIMEOUT", defaultPollTimeout)
	if err != nil {
		return config{}, err
	}

	return config{DatabaseURL: dsn, PollInterval: interval, PollTimeout: timeout}, nil
}

// parsePositiveDuration reads an optional Go-duration env var: absent/empty
// yields def; otherwise it must parse and be strictly positive. Errors name the
// offending variable and never include the DATABASE_URL value.
func parsePositiveDuration(lookup getenv, key string, def time.Duration) (time.Duration, error) {
	raw := lookup(key)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a valid duration: %w", key, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s %q must be positive", key, raw)
	}
	return d, nil
}
