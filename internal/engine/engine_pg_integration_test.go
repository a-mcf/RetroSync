//go:build integration

package engine_test

// Integration regression for slice-13 (mtime precision). This is the test that
// would have caught the live bug: it wires the REAL Postgres store (manifest
// mtimes round-trip through timestamptz, which stores only microseconds) to the
// REAL localfs adapter over t.TempDir() roots (stat returns nanosecond mtimes).
//
// Activate fans the primary's save out to a peer and records both nodes'
// manifest mtimes through Postgres — truncating the sub-microsecond digits. The
// next Poll re-stats the files at full nanosecond precision and compares against
// the truncated manifest. With exact equality this flagged a SPURIOUS conflict
// on the very first poll after a write; with mtimeEqual's microsecond-resolution
// compare it must be a noop. We assert: conflict_at stays nil and Poll adds no
// new fan-out sync_log rows. A second Poll must stay a noop too.
//
// The fakereach + memory-store unit path cannot reproduce this: both keep
// time.Time exact, so the precision mismatch never appears.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/migrate"
	"github.com/a-mcf/retrosync/internal/reach/resolve"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/postgres"
)

// pgPool returns a pool against a freshly-migrated, ISOLATED schema, or skips
// when DATABASE_URL is unset (so plain `go test ./...` without the tag/DB stays
// green).
//
// Isolation matters here: `go test -tags integration ./...` runs each package's
// test binary in PARALLEL, and the postgres conformance suite also migrates the
// shared `public` schema. To avoid racing it, this test creates its own
// dedicated schema and routes every connection's search_path to it (migrations
// and the store both use unqualified table names, so they land in that schema).
// The conformance suite keeps `public` to itself; neither clobbers the other.
func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres engine integration test")
	}
	ctx := context.Background()

	const schema = "engine_int_test"

	// Drop + recreate the isolated schema so the test starts clean, then point the
	// migrate connection's search_path at it before running migrations.
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := conn.Exec(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	if err := migrate.Up(ctx, conn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	// Build a pool whose every connection also defaults to the isolated schema.
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// Best-effort cleanup of the isolated schema.
		c, err := pgx.Connect(context.Background(), url)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	return pool
}

// TestEngine_PollNoopAfterActivate_PostgresLocalFS proves no spurious conflict
// after a normal activate fan-out when the manifest mtime round-trips through
// Postgres µs precision but the filesystem stats at ns precision.
func TestEngine_PollNoopAfterActivate_PostgresLocalFS(t *testing.T) {
	ctx := context.Background()
	st := postgres.New(pgPool(t))

	primaryRoot := t.TempDir()
	peerRoot := t.TempDir()

	const relPath = "retroarch/saves/Super Metroid.srm"
	saveData := []byte("primary save bytes v1")

	// Seed the primary's real file. Use a mtime with a non-zero sub-microsecond
	// nanosecond fraction (…252204219) — exactly the shape of the live bug: the
	// 219ns is what Postgres timestamptz cannot store, so a naive exact-equality
	// compare would see manifest(…252204000) != stat(…252204219) and conflict.
	saveMtime := time.Date(2026, 6, 21, 5, 6, 7, 252204219, time.UTC)
	primaryAbs := filepath.Join(primaryRoot, relPath)
	if err := os.MkdirAll(filepath.Dir(primaryAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(primaryAbs, saveData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(primaryAbs, saveMtime, saveMtime); err != nil {
		t.Fatal(err)
	}

	if err := st.CreateGame(ctx, store.Game{ID: gameID, Display: "Super Metroid", System: "snes"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSync(ctx, store.Sync{ID: syncID, GameID: gameID, Name: "Bob's stream"}); err != nil {
		t.Fatal(err)
	}
	mustPGNode(t, st, "primary", primaryRoot)
	mustPGNode(t, st, "peer", peerRoot)
	mustPGPath(t, st, "primary", relPath)
	mustPGPath(t, st, "peer", relPath)

	clock := steppingClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Second)
	eng := engine.New(st, resolve.ResolveReach, clock)

	// Activate: fan-out primary -> peer; manifest mtimes recorded through Postgres.
	if err := eng.Activate(ctx, syncID, "primary", "from-primary", false); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// Confirm the peer received the bytes (sanity: fan-out actually ran).
	peerAbs := filepath.Join(peerRoot, relPath)
	if got, err := os.ReadFile(peerAbs); err != nil || string(got) != string(saveData) {
		t.Fatalf("peer file after activate = %q (err %v), want %q", got, err, saveData)
	}

	okBefore := countOutcome(t, st, store.OutcomeOK)

	// First Poll after activate: MUST be a noop. Pre-fix this flagged a conflict
	// because manifest(µs) != stat(ns).
	if err := eng.Poll(ctx, syncID); err != nil {
		t.Fatalf("Poll #1: %v", err)
	}
	assertNoConflictNoFanout(t, st, "after Poll #1", okBefore)

	// Second Poll: still a noop.
	if err := eng.Poll(ctx, syncID); err != nil {
		t.Fatalf("Poll #2: %v", err)
	}
	assertNoConflictNoFanout(t, st, "after Poll #2", okBefore)
}

func assertNoConflictNoFanout(t *testing.T, st store.Store, when string, okBefore int) {
	t.Helper()
	ctx := context.Background()

	b, err := st.GetBinding(ctx, syncID)
	if err != nil {
		t.Fatalf("%s: get binding: %v", when, err)
	}
	if b.ConflictAt != nil {
		t.Fatalf("%s: spurious conflict — conflict_at = %v, want nil", when, b.ConflictAt)
	}
	if got := countOutcome(t, st, store.OutcomeConflict); got != 0 {
		t.Fatalf("%s: %d conflict sync_log rows, want 0", when, got)
	}
	if got := countOutcome(t, st, store.OutcomeOK); got != okBefore {
		t.Fatalf("%s: ok sync_log rows = %d, want %d (a noop poll must not re-fan-out)", when, got, okBefore)
	}
}

func countOutcome(t *testing.T, st store.Store, outcome store.Outcome) int {
	t.Helper()
	entries, err := st.ListLogBySync(context.Background(), syncID, 0)
	if err != nil {
		t.Fatalf("list sync_log: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.Outcome == outcome {
			n++
		}
	}
	return n
}

func mustPGNode(t *testing.T, st store.Store, id, root string) {
	t.Helper()
	if err := st.CreateNode(context.Background(), store.Node{
		ID:          id,
		Display:     id,
		Kind:        store.KindGeneric,
		Reach:       store.ReachSyncthingShare,
		ReachConfig: store.ReachConfig{Path: root},
	}); err != nil {
		t.Fatalf("CreateNode %q: %v", id, err)
	}
}

func mustPGPath(t *testing.T, st store.Store, nodeID, path string) {
	t.Helper()
	if err := st.SetSyncMember(context.Background(), store.SyncMember{SyncID: syncID, NodeID: nodeID, Path: path}); err != nil {
		t.Fatalf("SetSyncMember %q: %v", nodeID, err)
	}
}
