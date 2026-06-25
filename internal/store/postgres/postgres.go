// Package postgres is a pgx-based implementation of store.Store. It uses
// hand-written parameterized queries; no ORM. Business logic never imports
// this package directly — it depends on store.Store.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// querier is the subset of pgx used by Store; satisfied by both *pgxpool.Pool
// and pgx.Tx, so a statement can run either directly on the pool or inside a
// transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// txBeginner is the subset of *pgxpool.Pool that opens a transaction. The pool
// satisfies it; PutSaveVersion type-asserts s.db to it so its multi-statement
// blob/version/prune/GC sequence runs atomically. (A non-pool querier — e.g. a
// test double — without Begin falls back to running the statements directly.)
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
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
			// Duplicate id/PK or duplicate (node_id, path) sync_member.
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
		`INSERT INTO nodes (id, owner_user_id, display, kind, reach, reach_config)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		n.ID, n.OwnerUserID, n.Display, string(n.Kind), string(n.Reach), cfg)
	return mapErr(err)
}

func (s *Store) GetNode(ctx context.Context, id string) (store.Node, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, owner_user_id, display, kind, reach, reach_config
		 FROM nodes WHERE id = $1`, id)
	n, err := scanNode(row)
	if err != nil {
		return store.Node{}, mapErr(err)
	}
	return n, nil
}

func (s *Store) ListNodes(ctx context.Context) ([]store.Node, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, owner_user_id, display, kind, reach, reach_config
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
		 reach_config = $6 WHERE id = $1`,
		n.ID, n.OwnerUserID, n.Display, string(n.Kind), string(n.Reach), cfg)
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
	// save_versions cascades on node delete (FK), which can orphan blobs.
	return s.gcBlobs(ctx)
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
	if err := r.Scan(&n.ID, &n.OwnerUserID, &n.Display, &kind, &rch, &cfg); err != nil {
		return store.Node{}, err
	}
	n.Kind = store.Kind(kind)
	n.Reach = store.Reach(rch)
	if err := json.Unmarshal(cfg, &n.ReachConfig); err != nil {
		return store.Node{}, fmt.Errorf("unmarshal reach_config for node %q: %w", n.ID, err)
	}
	return n, nil
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
// syncs/sync_members are the unit of mirroring. Each sync row also carries its
// own runtime state — conflict_at (paused/forked) and last_synced (last mirror
// pass) — since slice-18 retired active_bindings. The runtime tables (manifest,
// sync_log) key off sync_id, REFERENCES syncs ON DELETE CASCADE — so DeleteSync
// tears down a sync's manifest/log too.

func (s *Store) CreateSync(ctx context.Context, sy store.Sync) error {
	// game is a free-text label (no FK), so the only constraint is the PK: a
	// duplicate id (PK) -> 23505 -> ErrConflict, handled by mapErr. A
	// freshly-created sync has no runtime state (conflict_at/last_synced default
	// NULL), so we don't write those columns here.
	_, err := s.db.Exec(ctx,
		`INSERT INTO syncs (id, game, name) VALUES ($1, $2, $3)`,
		sy.ID, sy.Game, sy.Name)
	return mapErr(err)
}

func (s *Store) GetSync(ctx context.Context, id string) (store.Sync, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, game, name, conflict_at, last_synced FROM syncs WHERE id = $1`, id)
	sy, err := scanSync(row)
	if err != nil {
		return store.Sync{}, mapErr(err)
	}
	return sy, nil
}

func (s *Store) ListSyncs(ctx context.Context) ([]store.Sync, error) {
	return s.querySyncs(ctx,
		`SELECT id, game, name, conflict_at, last_synced FROM syncs ORDER BY id`)
}

func (s *Store) querySyncs(ctx context.Context, sql string, args ...any) ([]store.Sync, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.Sync, 0)
	for rows.Next() {
		sy, err := scanSync(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, sy)
	}
	return out, mapErr(rows.Err())
}

func (s *Store) UpdateSync(ctx context.Context, sy store.Sync) error {
	// UpdateSync rewrites only the registry fields (game label, name); the runtime
	// state (conflict_at, last_synced) is owned by SetSyncConflict/MarkSyncSynced
	// and left untouched here. game is free text (no FK).
	tag, err := s.db.Exec(ctx,
		`UPDATE syncs SET game = $2, name = $3 WHERE id = $1`,
		sy.ID, sy.Game, sy.Name)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) SetSyncConflict(ctx context.Context, syncID string, at *time.Time) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE syncs SET conflict_at = $2 WHERE id = $1`, syncID, at)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) MarkSyncSynced(ctx context.Context, syncID string, t time.Time) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE syncs SET last_synced = $2 WHERE id = $1`, syncID, t)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func scanSync(r rowScanner) (store.Sync, error) {
	var sy store.Sync
	if err := r.Scan(&sy.ID, &sy.Game, &sy.Name, &sy.ConflictAt, &sy.LastSynced); err != nil {
		return store.Sync{}, err
	}
	return sy, nil
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
	// save_versions cascades on sync delete (FK), which can orphan blobs.
	return s.gcBlobs(ctx)
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

// ---- SaveVersions ----
//
// The recovery net: save_blobs is content-addressed (hash PK, dedup), and
// save_versions indexes captures per (sync_id, node_id), ordered by the
// monotonic seq bigserial. Retention is enforced on every PutSaveVersion (keep
// the newest store.SaveVersionRetention by seq DESC per (sync,node)), followed by
// orphan-blob GC. save_versions cascades on sync/node delete via FKs; gcBlobs
// then sweeps any blob left unreferenced.

func (s *Store) PutSaveVersion(ctx context.Context, syncID, nodeID, hash string, data []byte, reason string) error {
	// Validate the FK targets up front so a missing sync/node surfaces as
	// ErrInvalidReference (mirroring the memory store), independent of whether the
	// blob already exists.
	var syncOK, nodeOK bool
	if err := s.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM syncs WHERE id = $1),
		        EXISTS (SELECT 1 FROM nodes WHERE id = $2)`, syncID, nodeID,
	).Scan(&syncOK, &nodeOK); err != nil {
		return mapErr(err)
	}
	if !syncOK || !nodeOK {
		return store.ErrInvalidReference
	}

	// Dedup detail: if the most recent version for this (sync,node) already has
	// this exact hash, do NOT add a duplicate (no churn on identical re-saves).
	var lastHash string
	err := s.db.QueryRow(ctx,
		`SELECT hash FROM save_versions
		 WHERE sync_id = $1 AND node_id = $2
		 ORDER BY seq DESC LIMIT 1`, syncID, nodeID).Scan(&lastHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return mapErr(err)
	}
	if err == nil && lastHash == hash {
		return nil
	}

	// The blob-upsert / version-insert / prune / GC are a single logical mutation:
	// run them in ONE transaction so a mid-sequence failure or a concurrent caller
	// can't leave the table in a partial state (e.g. > retention rows, or a GC that
	// raced an interleaved insert). If the pool can't begin a tx (a non-pool test
	// double), fall back to running them directly on s.db.
	beginner, ok := s.db.(txBeginner)
	if !ok {
		return s.putSaveVersionStmts(ctx, s.db, syncID, nodeID, hash, data, reason)
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return mapErr(err)
	}
	defer tx.Rollback(ctx) // no-op after a successful Commit
	if err := s.putSaveVersionStmts(ctx, tx, syncID, nodeID, hash, data, reason); err != nil {
		return err
	}
	return mapErr(tx.Commit(ctx))
}

// putSaveVersionStmts runs the four mutating statements of a save-version put
// (blob upsert, version insert, prune-to-retention, orphan-blob GC) on q, which
// is either the pool or a transaction. PutSaveVersion runs it inside a tx for
// atomicity; the per-statement order is unchanged.
func (s *Store) putSaveVersionStmts(ctx context.Context, q querier, syncID, nodeID, hash string, data []byte, reason string) error {
	// Upsert the blob by hash (content-addressed dedup — identical content stored
	// once; ON CONFLICT keeps the existing bytes).
	if _, err := q.Exec(ctx,
		`INSERT INTO save_blobs (hash, data, size) VALUES ($1, $2, $3)
		 ON CONFLICT (hash) DO NOTHING`, hash, data, int64(len(data))); err != nil {
		return mapErr(err)
	}

	// Append the version row (seq is assigned by the bigserial).
	if _, err := q.Exec(ctx,
		`INSERT INTO save_versions (sync_id, node_id, hash, reason)
		 VALUES ($1, $2, $3, $4)`, syncID, nodeID, hash, reason); err != nil {
		return mapErr(err)
	}

	// Prune to the newest store.SaveVersionRetention per (sync,node) by seq DESC.
	if _, err := q.Exec(ctx,
		`DELETE FROM save_versions
		 WHERE sync_id = $1 AND node_id = $2
		   AND seq NOT IN (
		     SELECT seq FROM save_versions
		     WHERE sync_id = $1 AND node_id = $2
		     ORDER BY seq DESC LIMIT $3)`,
		syncID, nodeID, store.SaveVersionRetention); err != nil {
		return mapErr(err)
	}

	// GC any blob no surviving version references.
	return s.gcBlobsWith(ctx, q)
}

// gcBlobs deletes save_blobs rows that no save_versions row references (orphan
// blob GC). Called after the cascade deletes that can orphan a blob
// (sync/node/game delete); PutSaveVersion's prune uses gcBlobsWith inside its tx.
func (s *Store) gcBlobs(ctx context.Context) error {
	return s.gcBlobsWith(ctx, s.db)
}

// gcBlobsWith runs the orphan-blob GC on q (the pool or a transaction).
func (s *Store) gcBlobsWith(ctx context.Context, q querier) error {
	_, err := q.Exec(ctx,
		`DELETE FROM save_blobs b
		 WHERE NOT EXISTS (SELECT 1 FROM save_versions v WHERE v.hash = b.hash)`)
	return mapErr(err)
}

func (s *Store) ListSaveVersions(ctx context.Context, syncID, nodeID string, limit int) ([]store.SaveVersion, error) {
	var lim any
	if limit > 0 {
		lim = limit
	}
	rows, err := s.db.Query(ctx,
		`SELECT v.seq, v.sync_id, v.node_id, v.hash, v.captured_at, b.size, v.reason
		 FROM save_versions v JOIN save_blobs b ON b.hash = v.hash
		 WHERE v.sync_id = $1 AND v.node_id = $2
		 ORDER BY v.seq DESC
		 LIMIT $3`, syncID, nodeID, lim)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.SaveVersion, 0)
	for rows.Next() {
		var v store.SaveVersion
		if err := rows.Scan(&v.Seq, &v.SyncID, &v.NodeID, &v.Hash, &v.CapturedAt, &v.Size, &v.Reason); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, v)
	}
	return out, mapErr(rows.Err())
}

func (s *Store) GetSaveVersionData(ctx context.Context, syncID string, seq int64) (store.SaveVersion, []byte, error) {
	var (
		v    store.SaveVersion
		data []byte
	)
	// Bind the lookup to (sync_id, seq): a seq that belongs to a DIFFERENT sync
	// returns no rows -> ErrNotFound, so a guessed cross-sync seq can never be
	// loaded (the store-enforced authz floor for restore).
	err := s.db.QueryRow(ctx,
		`SELECT v.seq, v.sync_id, v.node_id, v.hash, v.captured_at, b.size, v.reason, b.data
		 FROM save_versions v JOIN save_blobs b ON b.hash = v.hash
		 WHERE v.seq = $1 AND v.sync_id = $2`, seq, syncID,
	).Scan(&v.Seq, &v.SyncID, &v.NodeID, &v.Hash, &v.CapturedAt, &v.Size, &v.Reason, &data)
	if err != nil {
		return store.SaveVersion{}, nil, mapErr(err)
	}
	return v, data, nil
}
