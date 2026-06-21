//go:build integration

package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/a-mcf/retrosync/internal/migrate"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/postgres"
	"github.com/a-mcf/retrosync/internal/store/storetest"
)

// dsn returns DATABASE_URL or skips the test when it is unset, so plain
// `go test ./...` (without the integration tag, and without a DB) stays green.
func dsn(t *testing.T) string {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	return url
}

// freshPool returns a pool against a freshly-migrated empty schema. It rolls
// the schema all the way down then back up so every factory call starts clean.
func freshPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := dsn(t)
	ctx := context.Background()

	// Reset schema using a single connection (migrate operates on *pgx.Conn).
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	// Drop everything we know about, then re-apply. Down is a no-op when empty.
	for {
		var n int
		err := conn.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			 WHERE table_schema = 'public' AND table_name = 'schema_migrations'`).Scan(&n)
		if err != nil {
			t.Fatalf("probe schema_migrations: %v", err)
		}
		if n == 0 {
			break
		}
		var applied int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
			t.Fatalf("count applied: %v", err)
		}
		if applied == 0 {
			break
		}
		if err := migrate.Down(ctx, conn); err != nil {
			t.Fatalf("migrate down: %v", err)
		}
	}
	if err := migrate.Up(ctx, conn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPostgresConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		return postgres.New(freshPool(t))
	})
}

// TestPostgresMigrationRoundTrip verifies up then down then up leaves the
// registry tables present and empty.
func TestPostgresMigrationRoundTrip(t *testing.T) {
	url := dsn(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	if err := migrate.Up(ctx, conn); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := migrate.Down(ctx, conn); err != nil {
		t.Fatalf("down: %v", err)
	}
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		 WHERE table_schema='public' AND table_name='users')`).Scan(&exists); err != nil {
		t.Fatalf("probe users: %v", err)
	}
	if exists {
		t.Fatal("users table should be gone after down")
	}
	if err := migrate.Up(ctx, conn); err != nil {
		t.Fatalf("re-up: %v", err)
	}
}

// Enum-rejection (bad role/kind/reach) and FK-rejection (missing owner/game/
// node) are now exercised by the shared conformance suite (InvalidValue /
// InvalidReference subtests), which runs against this Postgres store too.

// TestPostgresReachConfigNoSecret asserts the persisted reach_config JSON never
// contains a cleartext secret field — only the secret_ref pointer.
func TestPostgresReachConfigNoSecret(t *testing.T) {
	pool := freshPool(t)
	s := postgres.New(pool)
	ctx := context.Background()

	if err := s.CreateNode(ctx, store.Node{
		ID: "mister", Display: "MiSTer", Kind: store.KindMister, Reach: store.ReachSSH,
		ReachConfig: store.ReachConfig{Host: "172.16.7.12", User: "root", SecretRef: "mister-1"},
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}

	var raw string
	if err := pool.QueryRow(ctx, `SELECT reach_config::text FROM nodes WHERE id = 'mister'`).Scan(&raw); err != nil {
		t.Fatalf("read reach_config: %v", err)
	}
	for _, banned := range []string{"password", "passwd", "private_key", "secret_key", "\"key\""} {
		if containsFold(raw, banned) {
			t.Errorf("reach_config JSON contains banned secret field %q: %s", banned, raw)
		}
	}
	if !containsFold(raw, "secret_ref") {
		t.Errorf("reach_config JSON missing secret_ref pointer: %s", raw)
	}
}

func containsFold(haystack, needle string) bool {
	hl, nl := len(haystack), len(needle)
	if nl == 0 {
		return true
	}
	for i := 0; i+nl <= hl; i++ {
		match := true
		for j := 0; j < nl; j++ {
			if lower(haystack[i+j]) != lower(needle[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
