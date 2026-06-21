// Package memory is a thread-safe in-memory implementation of store.Store.
// It is the fast fake used by unit tests and any logic that does not need a
// real database.
package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

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
	// bindings keyed by game_id (the PK / one-active-session invariant).
	bindings map[string]store.ActiveBinding
	// manifest keyed by (game_id, node_id).
	manifest map[gpKey]store.ManifestEntry
	// log is the append-only sync_log, ordered by append; nextLogID assigns the
	// bigserial-equivalent id.
	log       []store.LogEntry
	nextLogID int64
	// syncs keyed by sync id.
	syncs map[string]store.Sync
	// syncMembers keyed by (sync_id, node_id) — the PK.
	syncMembers map[smKey]store.SyncMember
}

type gpKey struct {
	gameID string
	nodeID string
}

type smKey struct {
	syncID string
	nodeID string
}

// New returns an empty, ready-to-use in-memory Store.
func New() *Store {
	return &Store{
		users:       make(map[string]store.User),
		nodes:       make(map[string]store.Node),
		games:       make(map[string]store.Game),
		paths:       make(map[gpKey]store.GamePath),
		bindings:    make(map[string]store.ActiveBinding),
		manifest:    make(map[gpKey]store.ManifestEntry),
		nextLogID:   1,
		syncs:       make(map[string]store.Sync),
		syncMembers: make(map[smKey]store.SyncMember),
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
	// An active binding holds play authority on this node. The Postgres
	// NO-ACTION FK (active_bindings.primary_node) refuses the delete with a
	// 23503 -> ErrInvalidReference; mirror that here so both impls agree.
	for _, b := range s.bindings {
		if b.PrimaryNode == id {
			return store.ErrInvalidReference
		}
	}
	delete(s.nodes, id)
	// Cascade: delete game_paths and manifest rows referencing this node.
	for k := range s.paths {
		if k.nodeID == id {
			delete(s.paths, k)
		}
	}
	for k := range s.manifest {
		if k.nodeID == id {
			delete(s.manifest, k)
		}
	}
	// Cascade: sync_members.node_id REFERENCES nodes ON DELETE CASCADE.
	for k := range s.syncMembers {
		if k.nodeID == id {
			delete(s.syncMembers, k)
		}
	}
	// sync_log.from_node/to_node are unconstrained text by design, so node
	// deletion neither cascades nor blocks on the log.
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
	// An active binding makes this game active. The Postgres NO-ACTION FK
	// (active_bindings.game_id) refuses the delete with a 23503 ->
	// ErrInvalidReference; mirror that here so both impls agree.
	if _, ok := s.bindings[id]; ok {
		return store.ErrInvalidReference
	}
	delete(s.games, id)
	// Cascade: delete game_paths, manifest, and sync_log referencing this game.
	for k := range s.paths {
		if k.gameID == id {
			delete(s.paths, k)
		}
	}
	for k := range s.manifest {
		if k.gameID == id {
			delete(s.manifest, k)
		}
	}
	kept := s.log[:0]
	for _, e := range s.log {
		if e.GameID != id {
			kept = append(kept, e)
		}
	}
	s.log = kept
	// Cascade: syncs.game_id REFERENCES games ON DELETE CASCADE, and
	// sync_members.sync_id cascades from syncs. Delete the game's syncs and
	// their members.
	for syncID, sy := range s.syncs {
		if sy.GameID != id {
			continue
		}
		delete(s.syncs, syncID)
		for k := range s.syncMembers {
			if k.syncID == syncID {
				delete(s.syncMembers, k)
			}
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

// ---- ActiveBindings ----

func (s *Store) CreateBinding(_ context.Context, b store.ActiveBinding) error {
	if !store.ValidDirection(b.Direction) {
		return store.ErrInvalidValue
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.games[b.GameID]; !ok {
		return store.ErrInvalidReference
	}
	if _, ok := s.nodes[b.PrimaryNode]; !ok {
		return store.ErrInvalidReference
	}
	if _, ok := s.bindings[b.GameID]; ok {
		return store.ErrConflict
	}
	if b.PeerScope == "" {
		b.PeerScope = "all-configured"
	}
	if b.StartedAt.IsZero() {
		b.StartedAt = time.Now().UTC()
	}
	s.bindings[b.GameID] = cloneBinding(b)
	return nil
}

func (s *Store) GetBinding(_ context.Context, gameID string) (store.ActiveBinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.bindings[gameID]
	if !ok {
		return store.ActiveBinding{}, store.ErrNotFound
	}
	return cloneBinding(b), nil
}

func (s *Store) ListBindings(_ context.Context) ([]store.ActiveBinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.ActiveBinding, 0, len(s.bindings))
	for _, b := range s.bindings {
		out = append(out, cloneBinding(b))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GameID < out[j].GameID })
	return out, nil
}

func (s *Store) UpdateBinding(_ context.Context, b store.ActiveBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Match Postgres: a no-match UPDATE reports ErrNotFound before the CHECK
	// constraint can fire, so the existence check comes first.
	if _, ok := s.bindings[b.GameID]; !ok {
		return store.ErrNotFound
	}
	if !store.ValidDirection(b.Direction) {
		return store.ErrInvalidValue
	}
	if _, ok := s.nodes[b.PrimaryNode]; !ok {
		return store.ErrInvalidReference
	}
	if b.PeerScope == "" {
		b.PeerScope = "all-configured"
	}
	s.bindings[b.GameID] = cloneBinding(b)
	return nil
}

func (s *Store) DeleteBinding(_ context.Context, gameID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Idempotent: deleting an absent binding is a no-op (api.md "deactivate is
	// idempotent"). No ErrNotFound here, unlike the other Delete* methods.
	delete(s.bindings, gameID)
	return nil
}

// ---- SyncLog ----

func (s *Store) AppendLog(_ context.Context, e store.LogEntry) error {
	if !store.ValidOutcome(e.Outcome) {
		return store.ErrInvalidValue
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.games[e.GameID]; !ok {
		return store.ErrInvalidReference
	}
	e.ID = s.nextLogID
	s.nextLogID++
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	s.log = append(s.log, cloneLog(e))
	return nil
}

func (s *Store) ListLogByGame(_ context.Context, gameID string, limit int) ([]store.LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Most-recent-first by ts, ties broken by id descending — matching the
	// Postgres ORDER BY ts DESC, id DESC. We cannot rely on append order: a
	// caller-supplied or clock-skewed ts (RTC-less MiSTer, drifting Anbernics)
	// can make insertion order diverge from ts order, so sort explicitly.
	out := make([]store.LogEntry, 0)
	for _, e := range s.log {
		if e.GameID == gameID {
			out = append(out, cloneLog(e))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].TS.Equal(out[j].TS) {
			return out[i].TS.After(out[j].TS)
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---- Manifest ----

func (s *Store) SetManifest(_ context.Context, m store.ManifestEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.games[m.GameID]; !ok {
		return store.ErrInvalidReference
	}
	if _, ok := s.nodes[m.NodeID]; !ok {
		return store.ErrInvalidReference
	}
	s.manifest[gpKey{m.GameID, m.NodeID}] = cloneManifest(m)
	return nil
}

func (s *Store) GetManifest(_ context.Context, gameID, nodeID string) (store.ManifestEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.manifest[gpKey{gameID, nodeID}]
	if !ok {
		return store.ManifestEntry{}, store.ErrNotFound
	}
	return cloneManifest(m), nil
}

func (s *Store) ListManifestByGame(_ context.Context, gameID string) ([]store.ManifestEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.ManifestEntry, 0)
	for k, m := range s.manifest {
		if k.gameID == gameID {
			out = append(out, cloneManifest(m))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

// ---- Syncs ----

func (s *Store) CreateSync(_ context.Context, sy store.Sync) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.games[sy.GameID]; !ok {
		return store.ErrInvalidReference
	}
	if _, ok := s.syncs[sy.ID]; ok {
		return store.ErrConflict
	}
	s.syncs[sy.ID] = sy
	return nil
}

func (s *Store) GetSync(_ context.Context, id string) (store.Sync, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sy, ok := s.syncs[id]
	if !ok {
		return store.Sync{}, store.ErrNotFound
	}
	return sy, nil
}

func (s *Store) ListSyncsByGame(_ context.Context, gameID string) ([]store.Sync, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.Sync, 0)
	for _, sy := range s.syncs {
		if sy.GameID == gameID {
			out = append(out, sy)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) UpdateSync(_ context.Context, sy store.Sync) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Match Postgres: a no-match UPDATE reports ErrNotFound before the FK can
	// fire, so the existence check comes first.
	if _, ok := s.syncs[sy.ID]; !ok {
		return store.ErrNotFound
	}
	if _, ok := s.games[sy.GameID]; !ok {
		return store.ErrInvalidReference
	}
	s.syncs[sy.ID] = sy
	return nil
}

func (s *Store) DeleteSync(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.syncs[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.syncs, id)
	// Cascade: sync_members.sync_id REFERENCES syncs ON DELETE CASCADE.
	for k := range s.syncMembers {
		if k.syncID == id {
			delete(s.syncMembers, k)
		}
	}
	return nil
}

// ---- SyncMembers ----

func (s *Store) SetSyncMember(_ context.Context, m store.SyncMember) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.syncs[m.SyncID]; !ok {
		return store.ErrInvalidReference
	}
	if _, ok := s.nodes[m.NodeID]; !ok {
		return store.ErrInvalidReference
	}
	// Enforce the global UNIQUE (node_id, path): a given (device, file) lives in
	// at most one sync. Reject if that (node, path) is already a member of a
	// DIFFERENT sync. The same (node, path) within THIS sync (i.e. the row we are
	// about to upsert in place) is fine.
	for k, existing := range s.syncMembers {
		if existing.NodeID == m.NodeID && existing.Path == m.Path && k.syncID != m.SyncID {
			return store.ErrConflict
		}
	}
	s.syncMembers[smKey{m.SyncID, m.NodeID}] = m
	return nil
}

func (s *Store) GetSyncMember(_ context.Context, syncID, nodeID string) (store.SyncMember, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.syncMembers[smKey{syncID, nodeID}]
	if !ok {
		return store.SyncMember{}, store.ErrNotFound
	}
	return m, nil
}

func (s *Store) ListSyncMembers(_ context.Context, syncID string) ([]store.SyncMember, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.SyncMember, 0)
	for k, m := range s.syncMembers {
		if k.syncID == syncID {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

func (s *Store) ListSyncMembersByNode(_ context.Context, nodeID string) ([]store.SyncMember, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.SyncMember, 0)
	for k, m := range s.syncMembers {
		if k.nodeID == nodeID {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SyncID < out[j].SyncID })
	return out, nil
}

func (s *Store) DeleteSyncMember(_ context.Context, syncID, nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := smKey{syncID, nodeID}
	if _, ok := s.syncMembers[k]; !ok {
		return store.ErrNotFound
	}
	delete(s.syncMembers, k)
	return nil
}

// cloneBinding deep-copies pointer fields so callers can't mutate stored state.
func cloneBinding(b store.ActiveBinding) store.ActiveBinding {
	out := b
	out.ConflictAt = clonePtr(b.ConflictAt)
	out.LastSynced = clonePtr(b.LastSynced)
	return out
}

// cloneLog deep-copies pointer fields so callers can't mutate stored state.
func cloneLog(e store.LogEntry) store.LogEntry {
	out := e
	out.Bytes = clonePtr(e.Bytes)
	out.SrcMtime = clonePtr(e.SrcMtime)
	out.DstMtime = clonePtr(e.DstMtime)
	return out
}

// cloneManifest deep-copies pointer fields so callers can't mutate stored state.
func cloneManifest(m store.ManifestEntry) store.ManifestEntry {
	out := m
	out.Mtime = clonePtr(m.Mtime)
	out.Size = clonePtr(m.Size)
	out.SHA256 = clonePtr(m.SHA256)
	out.LastChecked = clonePtr(m.LastChecked)
	return out
}

// clonePtr returns a copy of *p (or nil), so stored pointer fields can't be
// mutated through a returned value.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
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
