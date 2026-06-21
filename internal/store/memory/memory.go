// Package memory is a thread-safe in-memory implementation of store.Store.
// It is the fast fake used by unit tests and any logic that does not need a
// real database.
package memory

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/a-mcf/retrosync/internal/store"
)

// Store is an in-memory store.Store guarded by a single mutex. The zero value
// is not usable; construct with New.
type Store struct {
	mu sync.RWMutex

	users map[string]store.User
	nodes map[string]store.Node
	games map[string]store.Game
	// paths keyed by game_id then node_id.
	paths map[gpKey]store.GamePath
}

type gpKey struct {
	gameID string
	nodeID string
}

// New returns an empty, ready-to-use in-memory Store.
func New() *Store {
	return &Store{
		users: make(map[string]store.User),
		nodes: make(map[string]store.Node),
		games: make(map[string]store.Game),
		paths: make(map[gpKey]store.GamePath),
	}
}

var _ store.Store = (*Store)(nil)

// ---- Users ----

func (s *Store) CreateUser(_ context.Context, u store.User) error {
	if !store.ValidRole(u.Role) {
		return store.ErrInvalidValue
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[u.ID]; ok {
		return store.ErrConflict
	}
	s.users[u.ID] = u
	return nil
}

func (s *Store) GetUser(_ context.Context, id string) (store.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func (s *Store) ListUsers(_ context.Context) ([]store.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) UpdateUser(_ context.Context, u store.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Match Postgres: a no-match UPDATE reports ErrNotFound before the CHECK
	// constraint can fire, so the existence check comes first.
	if _, ok := s.users[u.ID]; !ok {
		return store.ErrNotFound
	}
	if !store.ValidRole(u.Role) {
		return store.ErrInvalidValue
	}
	s.users[u.ID] = u
	return nil
}

func (s *Store) DeleteUser(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.users, id)
	return nil
}

// ---- Nodes ----

func (s *Store) CreateNode(_ context.Context, n store.Node) error {
	if !store.ValidKind(n.Kind) || !store.ValidReach(n.Reach) {
		return store.ErrInvalidValue
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOwner(n); err != nil {
		return err
	}
	if _, ok := s.nodes[n.ID]; ok {
		return store.ErrConflict
	}
	s.nodes[n.ID] = cloneNode(n)
	return nil
}

// checkOwner verifies a node's owner_user_id, if set, references an existing
// user. The caller must hold s.mu.
func (s *Store) checkOwner(n store.Node) error {
	if n.OwnerUserID != nil && *n.OwnerUserID != "" {
		if _, ok := s.users[*n.OwnerUserID]; !ok {
			return store.ErrInvalidReference
		}
	}
	return nil
}

func (s *Store) GetNode(_ context.Context, id string) (store.Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[id]
	if !ok {
		return store.Node{}, store.ErrNotFound
	}
	return cloneNode(n), nil
}

func (s *Store) ListNodes(_ context.Context) ([]store.Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, cloneNode(n))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) UpdateNode(_ context.Context, n store.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Match Postgres: a no-match UPDATE reports ErrNotFound before any CHECK or
	// FK constraint can fire, so the existence check comes first.
	if _, ok := s.nodes[n.ID]; !ok {
		return store.ErrNotFound
	}
	if !store.ValidKind(n.Kind) || !store.ValidReach(n.Reach) {
		return store.ErrInvalidValue
	}
	if err := s.checkOwner(n); err != nil {
		return err
	}
	s.nodes[n.ID] = cloneNode(n)
	return nil
}

func (s *Store) DeleteNode(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nodes[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.nodes, id)
	// Cascade: delete game_paths referencing this node.
	for k := range s.paths {
		if k.nodeID == id {
			delete(s.paths, k)
		}
	}
	return nil
}

// ---- Games ----

func (s *Store) CreateGame(_ context.Context, g store.Game) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.games[g.ID]; ok {
		return store.ErrConflict
	}
	s.games[g.ID] = g
	return nil
}

func (s *Store) GetGame(_ context.Context, id string) (store.Game, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.games[id]
	if !ok {
		return store.Game{}, store.ErrNotFound
	}
	return g, nil
}

func (s *Store) ListGames(_ context.Context, f store.GameFilter) ([]store.Game, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q := strings.ToLower(f.Q)
	out := make([]store.Game, 0, len(s.games))
	for _, g := range s.games {
		if f.System != "" && g.System != f.System {
			continue
		}
		if q != "" &&
			!strings.Contains(strings.ToLower(g.ID), q) &&
			!strings.Contains(strings.ToLower(g.Display), q) {
			continue
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) UpdateGame(_ context.Context, g store.Game) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.games[g.ID]; !ok {
		return store.ErrNotFound
	}
	s.games[g.ID] = g
	return nil
}

func (s *Store) DeleteGame(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.games[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.games, id)
	// Cascade: delete game_paths referencing this game.
	for k := range s.paths {
		if k.gameID == id {
			delete(s.paths, k)
		}
	}
	return nil
}

// ---- GamePaths ----

func (s *Store) SetGamePath(_ context.Context, gp store.GamePath) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.games[gp.GameID]; !ok {
		return store.ErrInvalidReference
	}
	if _, ok := s.nodes[gp.NodeID]; !ok {
		return store.ErrInvalidReference
	}
	s.paths[gpKey{gp.GameID, gp.NodeID}] = gp
	return nil
}

func (s *Store) GetGamePath(_ context.Context, gameID, nodeID string) (store.GamePath, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	gp, ok := s.paths[gpKey{gameID, nodeID}]
	if !ok {
		return store.GamePath{}, store.ErrNotFound
	}
	return gp, nil
}

func (s *Store) ListGamePathsByGame(_ context.Context, gameID string) ([]store.GamePath, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.GamePath, 0)
	for k, gp := range s.paths {
		if k.gameID == gameID {
			out = append(out, gp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

func (s *Store) ListGamePathsByNode(_ context.Context, nodeID string) ([]store.GamePath, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.GamePath, 0)
	for k, gp := range s.paths {
		if k.nodeID == nodeID {
			out = append(out, gp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GameID < out[j].GameID })
	return out, nil
}

func (s *Store) DeleteGamePath(_ context.Context, gameID, nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := gpKey{gameID, nodeID}
	if _, ok := s.paths[k]; !ok {
		return store.ErrNotFound
	}
	delete(s.paths, k)
	return nil
}

// cloneNode deep-copies pointer fields so callers can't mutate stored state.
func cloneNode(n store.Node) store.Node {
	out := n
	if n.OwnerUserID != nil {
		v := *n.OwnerUserID
		out.OwnerUserID = &v
	}
	if n.LastSeenAt != nil {
		v := *n.LastSeenAt
		out.LastSeenAt = &v
	}
	return out
}

// TODO(slice-runtime): implement ActiveBinding/SyncLog/Manifest methods here
// once they are added to store.Store.
