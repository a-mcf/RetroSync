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
	tag, err := s.db.Exec(ctx, `DELETE FROM games WHERE id = $1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ---- GamePaths ----

func (s *Store) SetGamePath(ctx context.Context, gp store.GamePath) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO game_paths (game_id, node_id, path) VALUES ($1, $2, $3)
		 ON CONFLICT (game_id, node_id) DO UPDATE SET path = EXCLUDED.path`,
		gp.GameID, gp.NodeID, gp.Path)
	return mapErr(err)
}

func (s *Store) GetGamePath(ctx context.Context, gameID, nodeID string) (store.GamePath, error) {
	var gp store.GamePath
	err := s.db.QueryRow(ctx,
		`SELECT game_id, node_id, path FROM game_paths WHERE game_id = $1 AND node_id = $2`,
		gameID, nodeID,
	).Scan(&gp.GameID, &gp.NodeID, &gp.Path)
	if err != nil {
		return store.GamePath{}, mapErr(err)
	}
	return gp, nil
}

func (s *Store) ListGamePathsByGame(ctx context.Context, gameID string) ([]store.GamePath, error) {
	return s.listGamePaths(ctx,
		`SELECT game_id, node_id, path FROM game_paths WHERE game_id = $1 ORDER BY node_id`,
		gameID)
}

func (s *Store) ListGamePathsByNode(ctx context.Context, nodeID string) ([]store.GamePath, error) {
	return s.listGamePaths(ctx,
		`SELECT game_id, node_id, path FROM game_paths WHERE node_id = $1 ORDER BY game_id`,
		nodeID)
}

func (s *Store) listGamePaths(ctx context.Context, sql, arg string) ([]store.GamePath, error) {
	rows, err := s.db.Query(ctx, sql, arg)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := make([]store.GamePath, 0)
	for rows.Next() {
		var gp store.GamePath
		if err := rows.Scan(&gp.GameID, &gp.NodeID, &gp.Path); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, gp)
	}
	return out, mapErr(rows.Err())
}

func (s *Store) DeleteGamePath(ctx context.Context, gameID, nodeID string) error {
	tag, err := s.db.Exec(ctx,
		`DELETE FROM game_paths WHERE game_id = $1 AND node_id = $2`, gameID, nodeID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// TODO(slice-runtime): implement ActiveBinding/SyncLog/Manifest methods here
// once they are added to store.Store.
