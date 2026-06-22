// Package postgres is a pgx-based implementation of store.Store. It uses
// hand-written parameterized queries; no ORM. Business logic never imports
// this package directly — it depends on store.Store.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/a-mcf/retrosync/internal/store"
)

// Postgres-side SQLSTATE codes we care about.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgCheckViolation      = "23514"
)

// querier is the subset of pgx used by Store; satisfied by *pgxpool.Pool.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is a Postgres-backed store.Store.
type Store struct {
	db querier
}

var _ store.Store = (*Store)(nil)

// New returns a Store backed by the given pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{db: pool}
}

// mapErr translates Postgres errors into the store's typed errors.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			// Duplicate id/PK or duplicate (game_id, node_id).
			return fmt.Errorf("%w: %s", store.ErrConflict, pgErr.Code)
		case pgForeignKeyViolation:
			// A write referenced a parent row that does not exist.
			return fmt.Errorf("%w: %s", store.ErrInvalidReference, pgErr.Code)
		case pgCheckViolation:
			// A field failed a domain/enum CHECK constraint.
			return fmt.Errorf("%w: %s", store.ErrInvalidValue, pgErr.Code)
		}
	}
	return err
}

// ---- Users ----

func (s *Store) CreateUser(ctx context.Context, u store.User) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO users (id, display, pw_hash, role) VALUES ($1, $2, $3, $4)`,
		u.ID, u.Display, u.PwHash, string(u.Role))
	return mapErr(err)
}

func (s *Store) GetUser(ctx context.Context, id string) (store.User, error) {
	var u store.User
	var role string
	err := s.db.QueryRow(ctx,
		`SELECT id, display, pw_hash, role FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Display, &u.PwHash, &role)
	if err != nil {
		return store.User{}, mapErr(err)
	}
	u.Role = store.Role(role)
	return u, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]store.User, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, display, pw_hash, role FROM users ORDER BY id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.User, 0)
	for rows.Next() {
		var u store.User
		var role string
		if err := rows.Scan(&u.ID, &u.Display, &u.PwHash, &role); err != nil {
			return nil, mapErr(err)
		}
		u.Role = store.Role(role)
		out = append(out, u)
	}
	return out, mapErr(rows.Err())
}

func (s *Store) UpdateUser(ctx context.Context, u store.User) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE users SET display = $2, pw_hash = $3, role = $4 WHERE id = $1`,
		u.ID, u.Display, u.PwHash, string(u.Role))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ---- Nodes ----

func (s *Store) CreateNode(ctx context.Context, n store.Node) error {
	cfg, err := json.Marshal(n.ReachConfig)
	if err != nil {
		return fmt.Errorf("marshal reach_config: %w", err)
	}
	_, err = s.db.Exec(ctx,
		`INSERT INTO nodes (id, owner_user_id, display, kind, reach, reach_config, last_seen_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		n.ID, n.OwnerUserID, n.Display, string(n.Kind), string(n.Reach), cfg, n.LastSeenAt)
	return mapErr(err)
}

func (s *Store) GetNode(ctx context.Context, id string) (store.Node, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, owner_user_id, display, kind, reach, reach_config, last_seen_at
		 FROM nodes WHERE id = $1`, id)
	n, err := scanNode(row)
	if err != nil {
		return store.Node{}, mapErr(err)
	}
	return n, nil
}

func (s *Store) ListNodes(ctx context.Context) ([]store.Node, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, owner_user_id, display, kind, reach, reach_config, last_seen_at
		 FROM nodes ORDER BY id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.Node, 0)
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, n)
	}
	return out, mapErr(rows.Err())
}

func (s *Store) UpdateNode(ctx context.Context, n store.Node) error {
	cfg, err := json.Marshal(n.ReachConfig)
	if err != nil {
		return fmt.Errorf("marshal reach_config: %w", err)
	}
	tag, err := s.db.Exec(ctx,
		`UPDATE nodes SET owner_user_id = $2, display = $3, kind = $4, reach = $5,
		 reach_config = $6, last_seen_at = $7 WHERE id = $1`,
		n.ID, n.OwnerUserID, n.Display, string(n.Kind), string(n.Reach), cfg, n.LastSeenAt)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteNode(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM nodes WHERE id = $1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// rowScanner abstracts pgx.Row and pgx.Rows for scanNode.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanNode(r rowScanner) (store.Node, error) {
	var (
		n    store.Node
		kind string
		rch  string
		cfg  []byte
	)
	if err := r.Scan(&n.ID, &n.OwnerUserID, &n.Display, &kind, &rch, &cfg, &n.LastSeenAt); err != nil {
		return store.Node{}, err
	}
	n.Kind = store.Kind(kind)
	n.Reach = store.Reach(rch)
	if err := json.Unmarshal(cfg, &n.ReachConfig); err != nil {
		return store.Node{}, fmt.Errorf("unmarshal reach_config for node %q: %w", n.ID, err)
	}
	return n, nil
}

// ---- Games ----

func (s *Store) CreateGame(ctx context.Context, g store.Game) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO games (id, display, system, notes) VALUES ($1, $2, $3, $4)`,
		g.ID, g.Display, g.System, g.Notes)
	return mapErr(err)
}

func (s *Store) GetGame(ctx context.Context, id string) (store.Game, error) {
	var g store.Game
	err := s.db.QueryRow(ctx,
		`SELECT id, display, system, notes FROM games WHERE id = $1`, id,
	).Scan(&g.ID, &g.Display, &g.System, &g.Notes)
	if err != nil {
		return store.Game{}, mapErr(err)
	}
	return g, nil
}

func (s *Store) ListGames(ctx context.Context, f store.GameFilter) ([]store.Game, error) {
	// $1 = system filter ('' means no filter); $2 = substring ('' means none).
	rows, err := s.db.Query(ctx,
		`SELECT id, display, system, notes FROM games
		 WHERE ($1 = '' OR system = $1)
		   AND ($2 = '' OR id ILIKE '%' || $2 || '%' OR display ILIKE '%' || $2 || '%')
		 ORDER BY id`,
		f.System, f.Q)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.Game, 0)
	for rows.Next() {
		var g store.Game
		if err := rows.Scan(&g.ID, &g.Display, &g.System, &g.Notes); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, g)
	}
	return out, mapErr(rows.Err())
}

func (s *Store) UpdateGame(ctx context.Context, g store.Game) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE games SET display = $2, system = $3, notes = $4 WHERE id = $1`,
		g.ID, g.Display, g.System, g.Notes)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteGame(ctx context.Context, id string) error {
	// A game is "active" when any of its syncs has an active binding. The FK
	// graph would CASCADE a game delete through syncs into active_bindings,
	// silently tearing down a live session — so we refuse it explicitly here
	// (brief F: delete-active-game is a friendly 409). We surface ErrConflict so
	// the web layer maps it to "being played right now — stop the session first".
	// This guard mirrors the memory store's; both impls agree.
	var active bool
	if err := s.db.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM active_bindings ab
		   JOIN syncs sy ON sy.id = ab.sync_id
		   WHERE sy.game_id = $1)`, id,
	).Scan(&active); err != nil {
		return mapErr(err)
	}
	if active {
		return store.ErrConflict
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM games WHERE id = $1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ---- ActiveBindings ----
//
// The active_bindings.sync_id PRIMARY KEY enforces one active session per sync
// (duplicate insert -> 23505 -> ErrConflict). sync_id REFERENCES syncs ON DELETE
// CASCADE; primary_node is NO ACTION, so DeleteNode of an active primary fails
// with 23503 -> ErrInvalidReference without any extra check here.

func (s *Store) CreateBinding(ctx context.Context, b store.ActiveBinding) error {
	// Let the DB default started_at when the caller leaves it zero.
	var startedAt any
	if !b.StartedAt.IsZero() {
		startedAt = b.StartedAt
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO active_bindings
		   (sync_id, primary_node, started_at, direction, conflict_at, last_synced)
		 VALUES ($1, $2, COALESCE($3, now()), $4, $5, $6)`,
		b.SyncID, b.PrimaryNode, startedAt, b.Direction, b.ConflictAt, b.LastSynced)
	return mapErr(err)
}

func (s *Store) GetBinding(ctx context.Context, syncID string) (store.ActiveBinding, error) {
	row := s.db.QueryRow(ctx,
		`SELECT sync_id, primary_node, started_at, direction, conflict_at, last_synced
		 FROM active_bindings WHERE sync_id = $1`, syncID)
	b, err := scanBinding(row)
	if err != nil {
		return store.ActiveBinding{}, mapErr(err)
	}
	return b, nil
}

func (s *Store) ListBindings(ctx context.Context) ([]store.ActiveBinding, error) {
	rows, err := s.db.Query(ctx,
		`SELECT sync_id, primary_node, started_at, direction, conflict_at, last_synced
		 FROM active_bindings ORDER BY sync_id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.ActiveBinding, 0)
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, b)
	}
	return out, mapErr(rows.Err())
}

func (s *Store) UpdateBinding(ctx context.Context, b store.ActiveBinding) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE active_bindings
		 SET primary_node = $2, direction = $3,
		     conflict_at = $4, last_synced = $5
		 WHERE sync_id = $1`,
		b.SyncID, b.PrimaryNode, b.Direction, b.ConflictAt, b.LastSynced)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteBinding(ctx context.Context, syncID string) error {
	// Idempotent: an absent binding is not an error (api.md "deactivate is
	// idempotent"), so we ignore RowsAffected.
	_, err := s.db.Exec(ctx, `DELETE FROM active_bindings WHERE sync_id = $1`, syncID)
	return mapErr(err)
}

func scanBinding(r rowScanner) (store.ActiveBinding, error) {
	var b store.ActiveBinding
	if err := r.Scan(&b.SyncID, &b.PrimaryNode, &b.StartedAt, &b.Direction,
		&b.ConflictAt, &b.LastSynced); err != nil {
		return store.ActiveBinding{}, err
	}
	return b, nil
}

// ---- SyncLog ----

func (s *Store) AppendLog(ctx context.Context, e store.LogEntry) error {
	var ts any
	if !e.TS.IsZero() {
		ts = e.TS
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO sync_log
		   (ts, sync_id, from_node, to_node, bytes, src_mtime, dst_mtime, outcome, message)
		 VALUES (COALESCE($1, now()), $2, $3, $4, $5, $6, $7, $8, $9)`,
		ts, e.SyncID, nullIfEmpty(e.FromNode), nullIfEmpty(e.ToNode),
		e.Bytes, e.SrcMtime, e.DstMtime, string(e.Outcome), e.Message)
	return mapErr(err)
}

func (s *Store) ListLogBySync(ctx context.Context, syncID string, limit int) ([]store.LogEntry, error) {
	// Most-recent-first. id (bigserial) breaks ts ties deterministically.
	// limit <= 0 means no cap; NULL disables the LIMIT clause.
	var lim any
	if limit > 0 {
		lim = limit
	}
	rows, err := s.db.Query(ctx,
		`SELECT id, ts, sync_id, from_node, to_node, bytes, src_mtime, dst_mtime, outcome, message
		 FROM sync_log WHERE sync_id = $1
		 ORDER BY ts DESC, id DESC
		 LIMIT $2`, syncID, lim)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.LogEntry, 0)
	for rows.Next() {
		var (
			e        store.LogEntry
			from, to *string
			outcome  string
		)
		if err := rows.Scan(&e.ID, &e.TS, &e.SyncID, &from, &to, &e.Bytes,
			&e.SrcMtime, &e.DstMtime, &outcome, &e.Message); err != nil {
			return nil, mapErr(err)
		}
		if from != nil {
			e.FromNode = *from
		}
		if to != nil {
			e.ToNode = *to
		}
		e.Outcome = store.Outcome(outcome)
		out = append(out, e)
	}
	return out, mapErr(rows.Err())
}

// nullIfEmpty maps "" to a SQL NULL so empty node ids store as NULL rather than
// an empty string (matching the historical-text semantics of from/to_node).
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- Manifest ----

func (s *Store) SetManifest(ctx context.Context, m store.ManifestEntry) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO manifest (sync_id, node_id, mtime, size, sha256, last_checked)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (sync_id, node_id) DO UPDATE SET
		   mtime = EXCLUDED.mtime, size = EXCLUDED.size,
		   sha256 = EXCLUDED.sha256, last_checked = EXCLUDED.last_checked`,
		m.SyncID, m.NodeID, m.Mtime, m.Size, m.SHA256, m.LastChecked)
	return mapErr(err)
}

func (s *Store) GetManifest(ctx context.Context, syncID, nodeID string) (store.ManifestEntry, error) {
	row := s.db.QueryRow(ctx,
		`SELECT sync_id, node_id, mtime, size, sha256, last_checked
		 FROM manifest WHERE sync_id = $1 AND node_id = $2`, syncID, nodeID)
	m, err := scanManifest(row)
	if err != nil {
		return store.ManifestEntry{}, mapErr(err)
	}
	return m, nil
}

func (s *Store) ListManifestBySync(ctx context.Context, syncID string) ([]store.ManifestEntry, error) {
	rows, err := s.db.Query(ctx,
		`SELECT sync_id, node_id, mtime, size, sha256, last_checked
		 FROM manifest WHERE sync_id = $1 ORDER BY node_id`, syncID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.ManifestEntry, 0)
	for rows.Next() {
		m, err := scanManifest(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, m)
	}
	return out, mapErr(rows.Err())
}

func scanManifest(r rowScanner) (store.ManifestEntry, error) {
	var m store.ManifestEntry
	if err := r.Scan(&m.SyncID, &m.NodeID, &m.Mtime, &m.Size, &m.SHA256, &m.LastChecked); err != nil {
		return store.ManifestEntry{}, err
	}
	return m, nil
}

// ---- Syncs ----
//
// syncs/sync_members are the unit of mirroring. The runtime tables
// (active_bindings, manifest, sync_log) key off sync_id, REFERENCES syncs ON
// DELETE CASCADE — so DeleteSync tears down a sync's binding/manifest/log too.

func (s *Store) CreateSync(ctx context.Context, sy store.Sync) error {
	// A missing game (FK) -> 23503 -> ErrInvalidReference; a duplicate id (PK) ->
	// 23505 -> ErrConflict. Both are handled by mapErr.
	_, err := s.db.Exec(ctx,
		`INSERT INTO syncs (id, game_id, name) VALUES ($1, $2, $3)`,
		sy.ID, sy.GameID, sy.Name)
	return mapErr(err)
}

func (s *Store) GetSync(ctx context.Context, id string) (store.Sync, error) {
	var sy store.Sync
	err := s.db.QueryRow(ctx,
		`SELECT id, game_id, name FROM syncs WHERE id = $1`, id,
	).Scan(&sy.ID, &sy.GameID, &sy.Name)
	if err != nil {
		return store.Sync{}, mapErr(err)
	}
	return sy, nil
}

func (s *Store) ListSyncsByGame(ctx context.Context, gameID string) ([]store.Sync, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, game_id, name FROM syncs WHERE game_id = $1 ORDER BY id`, gameID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.Sync, 0)
	for rows.Next() {
		var sy store.Sync
		if err := rows.Scan(&sy.ID, &sy.GameID, &sy.Name); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, sy)
	}
	return out, mapErr(rows.Err())
}

func (s *Store) UpdateSync(ctx context.Context, sy store.Sync) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE syncs SET game_id = $2, name = $3 WHERE id = $1`,
		sy.ID, sy.GameID, sy.Name)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteSync(ctx context.Context, id string) error {
	// sync_members.sync_id cascades, so members are removed by the DB.
	tag, err := s.db.Exec(ctx, `DELETE FROM syncs WHERE id = $1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ---- SyncMembers ----

func (s *Store) SetSyncMember(ctx context.Context, m store.SyncMember) error {
	// Upsert on the PK (sync_id, node_id). A missing sync/node (FK) -> 23503 ->
	// ErrInvalidReference. A (node_id, path) already claimed by a DIFFERENT sync
	// violates the UNIQUE (node_id, path) -> 23505 -> ErrConflict; the ON CONFLICT
	// clause only resolves PK collisions, so the cross-sync unique violation
	// surfaces as the conflict we want.
	_, err := s.db.Exec(ctx,
		`INSERT INTO sync_members (sync_id, node_id, path) VALUES ($1, $2, $3)
		 ON CONFLICT (sync_id, node_id) DO UPDATE SET path = EXCLUDED.path`,
		m.SyncID, m.NodeID, m.Path)
	return mapErr(err)
}

func (s *Store) GetSyncMember(ctx context.Context, syncID, nodeID string) (store.SyncMember, error) {
	var m store.SyncMember
	err := s.db.QueryRow(ctx,
		`SELECT sync_id, node_id, path FROM sync_members WHERE sync_id = $1 AND node_id = $2`,
		syncID, nodeID,
	).Scan(&m.SyncID, &m.NodeID, &m.Path)
	if err != nil {
		return store.SyncMember{}, mapErr(err)
	}
	return m, nil
}

func (s *Store) ListSyncMembers(ctx context.Context, syncID string) ([]store.SyncMember, error) {
	return s.listSyncMembers(ctx,
		`SELECT sync_id, node_id, path FROM sync_members WHERE sync_id = $1 ORDER BY node_id`,
		syncID)
}

func (s *Store) ListSyncMembersByNode(ctx context.Context, nodeID string) ([]store.SyncMember, error) {
	return s.listSyncMembers(ctx,
		`SELECT sync_id, node_id, path FROM sync_members WHERE node_id = $1 ORDER BY sync_id`,
		nodeID)
}

func (s *Store) listSyncMembers(ctx context.Context, sql, arg string) ([]store.SyncMember, error) {
	rows, err := s.db.Query(ctx, sql, arg)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.SyncMember, 0)
	for rows.Next() {
		var m store.SyncMember
		if err := rows.Scan(&m.SyncID, &m.NodeID, &m.Path); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, m)
	}
	return out, mapErr(rows.Err())
}

func (s *Store) DeleteSyncMember(ctx context.Context, syncID, nodeID string) error {
	tag, err := s.db.Exec(ctx,
		`DELETE FROM sync_members WHERE sync_id = $1 AND node_id = $2`, syncID, nodeID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}
