package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// The database startup path must never let the DSN password reach the returned
// error OR the logs, even when the DSN is malformed (so pgxpool.ParseConfig is
// the thing that fails). We feed a DSN that is password-bearing but malformed
// enough that ParseConfig rejects it before any network I/O, keeping the test
// deterministic and host-independent.
func TestRunDatabaseErrorNeverLeaksDSN(t *testing.T) {
	const secret = "s3cr3t"
	// A port that is not a number makes pgxpool.ParseConfig fail fast.
	const badDSN = "postgres://user:" + secret + "@host:notaport/db"

	t.Setenv("DATABASE_URL", badDSN)
	t.Setenv("RETROSYNC_POLL_INTERVAL", "")
	t.Setenv("RETROSYNC_POLL_TIMEOUT", "")

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	err := run(logger)
	if err == nil {
		t.Fatal("run: want a database startup error for a malformed DSN")
	}
	if !errors.Is(err, errStartupDatabase) {
		t.Fatalf("run returned %v, want errStartupDatabase", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), badDSN) {
		t.Fatalf("returned error leaked the DSN/password: %q", err)
	}
	if strings.Contains(buf.String(), secret) || strings.Contains(buf.String(), badDSN) {
		t.Fatalf("logs leaked the DSN/password:\n%s", buf.String())
	}
}

// run uses os.Getenv via parseConfig; ensure the test does not depend on a real
// DATABASE_URL leaking in from the developer's environment.
func TestMain(m *testing.M) {
	os.Unsetenv("DATABASE_URL")
	os.Unsetenv("RETROSYNC_POLL_INTERVAL")
	os.Unsetenv("RETROSYNC_POLL_TIMEOUT")
	os.Exit(m.Run())
}
