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
	// manifest keyed by (sync_id, node_id).
	manifest map[smKey]store.ManifestEntry
	// log is the append-only sync_log, ordered by append; nextLogID assigns the
	// bigserial-equivalent id.
	log       []store.LogEntry
	nextLogID int64
	// syncs keyed by sync id.
	syncs map[string]store.Sync
	// syncMembers keyed by (sync_id, node_id) — the PK.
	syncMembers map[smKey]store.SyncMember
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
		manifest:    make(map[smKey]store.ManifestEntry),
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
	delete(s.nodes, id)
	// Cascade: delete manifest rows referencing this node.
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
	// Under auto-mirror every sync is mirroring; there is no "active session" to
	// tear down. But a CONFLICTED sync is paused awaiting a human's resolution —
	// deleting its game would silently discard that unresolved fork. Refuse in
	// that case (ErrConflict -> friendly 409 "resolve the conflict first"); a
	// non-conflicted game deletes and cascades freely.
	for syncID, sy := range s.syncs {
		if sy.GameID != id {
			continue
		}
		if s.syncs[syncID].ConflictAt != nil {
			return store.ErrConflict
		}
	}
	delete(s.games, id)
	// Cascade: syncs.game_id REFERENCES games ON DELETE CASCADE. Each deleted sync
	// in turn cascades its members, manifest, and sync_log.
	for syncID, sy := range s.syncs {
		if sy.GameID != id {
			continue
		}
		s.deleteSyncCascade(syncID)
	}
	return nil
}

// deleteSyncCascade removes a sync and every row that cascades from it:
// sync_members, manifest, and sync_log. The caller must hold s.mu. It does NOT
// remove the syncs entry's siblings — only the given sync. (The sync's runtime
// state — conflict_at/last_synced — lives on the sync row itself, so it goes
// with the delete(s.syncs, ...) above.)
func (s *Store) deleteSyncCascade(syncID string) {
	delete(s.syncs, syncID)
	for k := range s.syncMembers {
		if k.syncID == syncID {
			delete(s.syncMembers, k)
		}
	}
	for k := range s.manifest {
		if k.syncID == syncID {
			delete(s.manifest, k)
		}
	}
	kept := s.log[:0]
	for _, e := range s.log {
		if e.SyncID != syncID {
			kept = append(kept, e)
		}
	}
	s.log = kept
}

// ---- SyncLog ----

func (s *Store) AppendLog(_ context.Context, e store.LogEntry) error {
	if !store.ValidOutcome(e.Outcome) {
		return store.ErrInvalidValue
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.syncs[e.SyncID]; !ok {
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

func (s *Store) ListLogBySync(_ context.Context, syncID string, limit int) ([]store.LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Most-recent-first by ts, ties broken by id descending — matching the
	// Postgres ORDER BY ts DESC, id DESC. We cannot rely on append order: a
	// caller-supplied or clock-skewed ts (RTC-less MiSTer, drifting Anbernics)
	// can make insertion order diverge from ts order, so sort explicitly.
	out := make([]store.LogEntry, 0)
	for _, e := range s.log {
		if e.SyncID == syncID {
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
	if _, ok := s.syncs[m.SyncID]; !ok {
		return store.ErrInvalidReference
	}
	if _, ok := s.nodes[m.NodeID]; !ok {
		return store.ErrInvalidReference
	}
	s.manifest[smKey{m.SyncID, m.NodeID}] = cloneManifest(m)
	return nil
}

func (s *Store) GetManifest(_ context.Context, syncID, nodeID string) (store.ManifestEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.manifest[smKey{syncID, nodeID}]
	if !ok {
		return store.ManifestEntry{}, store.ErrNotFound
	}
	return cloneManifest(m), nil
}

func (s *Store) ListManifestBySync(_ context.Context, syncID string) ([]store.ManifestEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.ManifestEntry, 0)
	for k, m := range s.manifest {
		if k.syncID == syncID {
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
	// A freshly-created sync has no runtime state yet (not conflicted, never
	// synced), regardless of what the caller passed.
	sy.ConflictAt = nil
	sy.LastSynced = nil
	s.syncs[sy.ID] = cloneSync(sy)
	return nil
}

func (s *Store) GetSync(_ context.Context, id string) (store.Sync, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sy, ok := s.syncs[id]
	if !ok {
		return store.Sync{}, store.ErrNotFound
	}
	return cloneSync(sy), nil
}

func (s *Store) ListSyncsByGame(_ context.Context, gameID string) ([]store.Sync, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.Sync, 0)
	for _, sy := range s.syncs {
		if sy.GameID == gameID {
			out = append(out, cloneSync(sy))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) ListSyncs(_ context.Context) ([]store.Sync, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.Sync, 0, len(s.syncs))
	for _, sy := range s.syncs {
		out = append(out, cloneSync(sy))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) UpdateSync(_ context.Context, sy store.Sync) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Match Postgres: a no-match UPDATE reports ErrNotFound before the FK can
	// fire, so the existence check comes first.
	cur, ok := s.syncs[sy.ID]
	if !ok {
		return store.ErrNotFound
	}
	if _, ok := s.games[sy.GameID]; !ok {
		return store.ErrInvalidReference
	}
	// UpdateSync rewrites the registry fields (game_id, name) only; the runtime
	// state (conflict_at, last_synced) is owned by SetSyncConflict/MarkSyncSynced
	// and preserved across a rename, mirroring the Postgres UPDATE column list.
	cur.GameID = sy.GameID
	cur.Name = sy.Name
	s.syncs[sy.ID] = cur
	return nil
}

func (s *Store) SetSyncConflict(_ context.Context, syncID string, at *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sy, ok := s.syncs[syncID]
	if !ok {
		return store.ErrNotFound
	}
	sy.ConflictAt = clonePtr(at)
	s.syncs[syncID] = sy
	return nil
}

func (s *Store) MarkSyncSynced(_ context.Context, syncID string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sy, ok := s.syncs[syncID]
	if !ok {
		return store.ErrNotFound
	}
	v := t
	sy.LastSynced = &v
	s.syncs[syncID] = sy
	return nil
}

func (s *Store) DeleteSync(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.syncs[id]; !ok {
		return store.ErrNotFound
	}
	// Cascade: sync_members, manifest, and sync_log all REFERENCE syncs ON DELETE
	// CASCADE. The sync's own runtime state (conflict_at, last_synced) lives on
	// the sync row, so it goes with the delete. DeleteSync does NOT refuse a
	// conflicted sync — deleting it intentionally discards the unresolved fork
	// (the web layer guards delete-game on conflict, but delete-sync is direct).
	s.deleteSyncCascade(id)
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

// cloneSync deep-copies the sync's pointer runtime fields (conflict_at,
// last_synced) so callers can't mutate stored state through a returned value.
func cloneSync(sy store.Sync) store.Sync {
	out := sy
	out.ConflictAt = clonePtr(sy.ConflictAt)
	out.LastSynced = clonePtr(sy.LastSynced)
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
