// Package migrate applies SQL migrations embedded at build time. It is a
// minimal forward/down applier: enough for RetroSync's needs without pulling
// in a migration framework. Migrations are plain .sql files named
// NNNN_name.up.sql / NNNN_name.down.sql.
//
// The canonical migration SQL lives at the repository root in migrations/.
// Copies embedded here under sql/ are kept byte-identical (asserted by a test)
// because go:embed cannot reach paths outside the package directory.
package migrate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed all:sql
var migrationsFS embed.FS

// migrationsDir is the embedded subdirectory holding the .sql files.
const migrationsDir = "sql"

// migration is one parsed up/down pair.
type migration struct {
	version int
	name    string
	up      string
	down    string
}

// Up applies all migrations not yet recorded in schema_migrations, in order,
// each in its own transaction. It is idempotent: already-applied versions are
// skipped.
func Up(ctx context.Context, db *pgx.Conn) error {
	migs, err := load()
	if err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version bigint PRIMARY KEY,
		name    text NOT NULL
	)`); err != nil {
		return fmt.Errorf("migrate: ensure schema_migrations: %w", err)
	}
	for _, m := range migs {
		var exists bool
		if err := db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
			m.version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("migrate: check version %d: %w", m.version, err)
		}
		if exists {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func applyOne(ctx context.Context, db *pgx.Conn, m migration) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: begin %d: %w", m.version, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	if _, err := tx.Exec(ctx, m.up); err != nil {
		return fmt.Errorf("migrate: apply %d (%s): %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		m.version, m.name,
	); err != nil {
		return fmt.Errorf("migrate: record %d: %w", m.version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: commit %d: %w", m.version, err)
	}
	return nil
}

// Down rolls back the single most recently applied migration. It is a no-op if
// nothing is applied.
func Down(ctx context.Context, db *pgx.Conn) error {
	migs, err := load()
	if err != nil {
		return err
	}
	byVersion := make(map[int]migration, len(migs))
	for _, m := range migs {
		byVersion[m.version] = m
	}
	var version int
	err = db.QueryRow(ctx,
		`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`,
	).Scan(&version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("migrate: latest version: %w", err)
	}
	m, ok := byVersion[version]
	if !ok {
		return fmt.Errorf("migrate: no down script for applied version %d", version)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: begin down %d: %w", version, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, m.down); err != nil {
		return fmt.Errorf("migrate: down %d: %w", version, err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM schema_migrations WHERE version = $1`, version,
	); err != nil {
		return fmt.Errorf("migrate: unrecord %d: %w", version, err)
	}
	return tx.Commit(ctx)
}

// load parses the embedded migration files into ordered migrations.
func load() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("migrate: read embedded dir: %w", err)
	}
	type pair struct {
		name string
		up   string
		down string
	}
	pairs := map[int]*pair{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		version, label, dir, ok := parseName(name)
		if !ok {
			continue
		}
		body, err := fs.ReadFile(migrationsFS, migrationsDir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", name, err)
		}
		p := pairs[version]
		if p == nil {
			p = &pair{name: label}
			pairs[version] = p
		}
		switch dir {
		case "up":
			p.up = string(body)
		case "down":
			p.down = string(body)
		}
	}
	out := make([]migration, 0, len(pairs))
	for v, p := range pairs {
		if p.up == "" {
			return nil, fmt.Errorf("migrate: version %d missing up script", v)
		}
		out = append(out, migration{version: v, name: p.name, up: p.up, down: p.down})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// parseName parses NNNN_label.(up|down).sql. Returns ok=false for non-matching
// names.
func parseName(name string) (version int, label, dir string, ok bool) {
	if !strings.HasSuffix(name, ".sql") {
		return 0, "", "", false
	}
	base := strings.TrimSuffix(name, ".sql")
	// base = NNNN_label.up  or  NNNN_label.down
	dot := strings.LastIndex(base, ".")
	if dot < 0 {
		return 0, "", "", false
	}
	dir = base[dot+1:]
	if dir != "up" && dir != "down" {
		return 0, "", "", false
	}
	rest := base[:dot] // NNNN_label
	us := strings.Index(rest, "_")
	if us < 0 {
		return 0, "", "", false
	}
	v, err := strconv.Atoi(rest[:us])
	if err != nil {
		return 0, "", "", false
	}
	return v, rest[us+1:], dir, true
}
