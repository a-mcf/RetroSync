// Package memory is a thread-safe in-memory implementation of store.Store.
// It is the fast fake used by unit tests and any logic that does not need a
// real database.
package memory

import (
	"context"
	"sort"
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

	// saveBlobs is the content-addressed blob store, keyed by content hash (the
	// save_blobs PK). Identical content is stored once.
	saveBlobs map[string]blobRec
	// saveVersions is the save_versions index, ordered by Seq (append order ==
	// seq order since seq is monotonic). nextVersionSeq assigns the bigserial.
	saveVersions   []store.SaveVersion
	nextVersionSeq int64
}

// blobRec is one content-addressed blob: its bytes and size.
type blobRec struct {
	data []byte
	size int64
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
		manifest:    make(map[smKey]store.ManifestEntry),
		nextLogID:   1,
		syncs:       make(map[string]store.Sync),
		syncMembers: make(map[smKey]store.SyncMember),

		saveBlobs:      make(map[string]blobRec),
		nextVersionSeq: 1,
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
	// Cascade: save_versions.node_id REFERENCES nodes ON DELETE CASCADE, then GC
	// any blob left orphaned.
	keptV := s.saveVersions[:0]
	for _, v := range s.saveVersions {
		if v.NodeID != id {
			keptV = append(keptV, v)
		}
	}
	s.saveVersions = keptV
	s.gcBlobsLocked()
	// sync_log.from_node/to_node are unconstrained text by design, so node
	// deletion neither cascades nor blocks on the log.
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
	// save_versions.sync_id REFERENCES syncs ON DELETE CASCADE.
	keptV := s.saveVersions[:0]
	for _, v := range s.saveVersions {
		if v.SyncID != syncID {
			keptV = append(keptV, v)
		}
	}
	s.saveVersions = keptV
	s.gcBlobsLocked()
}

// gcBlobsLocked deletes any save_blobs entry no surviving save_versions row
// references (orphan blob GC). The caller must hold s.mu.
func (s *Store) gcBlobsLocked() {
	used := make(map[string]bool, len(s.saveVersions))
	for _, v := range s.saveVersions {
		used[v.Hash] = true
	}
	for h := range s.saveBlobs {
		if !used[h] {
			delete(s.saveBlobs, h)
		}
	}
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
	// Game is a free-text label, not an FK: any value (including "") is allowed.
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
	// UpdateSync rewrites the registry fields (game label, name) only; the runtime
	// state (conflict_at, last_synced) is owned by SetSyncConflict/MarkSyncSynced
	// and preserved across a rename, mirroring the Postgres UPDATE column list. The
	// game label is free text (no FK), so there is no reference to validate.
	cur.Game = sy.Game
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

// ---- SaveVersions ----

func (s *Store) PutSaveVersion(_ context.Context, syncID, nodeID, hash string, data []byte, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.syncs[syncID]; !ok {
		return store.ErrInvalidReference
	}
	if _, ok := s.nodes[nodeID]; !ok {
		return store.ErrInvalidReference
	}

	// Dedup detail: if the most recent version for this (sync,node) already has
	// this exact hash, do NOT add a duplicate (no churn on no-op/identical
	// re-saves). Scan back-to-front since seq order == append order.
	for i := len(s.saveVersions) - 1; i >= 0; i-- {
		v := s.saveVersions[i]
		if v.SyncID == syncID && v.NodeID == nodeID {
			if v.Hash == hash {
				return nil
			}
			break
		}
	}

	// Upsert the blob by hash (content-addressed dedup — identical content stored
	// once). The size is derived from the bytes.
	if _, ok := s.saveBlobs[hash]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		s.saveBlobs[hash] = blobRec{data: cp, size: int64(len(cp))}
	}

	// Append the version row, assigning the monotonic seq.
	s.saveVersions = append(s.saveVersions, store.SaveVersion{
		Seq:        s.nextVersionSeq,
		SyncID:     syncID,
		NodeID:     nodeID,
		Hash:       hash,
		CapturedAt: time.Now().UTC(),
		Size:       s.saveBlobs[hash].size,
		Reason:     reason,
	})
	s.nextVersionSeq++

	// Prune to the newest SaveVersionRetention per (sync,node) ordered by seq DESC.
	s.pruneVersionsLocked(syncID, nodeID)
	// GC any blob no surviving version references.
	s.gcBlobsLocked()
	return nil
}

// pruneVersionsLocked keeps only the newest store.SaveVersionRetention versions
// for (syncID, nodeID) ordered by seq DESC, deleting older rows. The caller must
// hold s.mu. saveVersions is maintained in seq order (== append order), so the
// surviving versions for this key are simply its last N entries.
func (s *Store) pruneVersionsLocked(syncID, nodeID string) {
	// Collect the seqs for this key in ascending order, then mark the oldest
	// (everything before the last N) for deletion.
	var seqs []int64
	for _, v := range s.saveVersions {
		if v.SyncID == syncID && v.NodeID == nodeID {
			seqs = append(seqs, v.Seq)
		}
	}
	if len(seqs) <= store.SaveVersionRetention {
		return
	}
	drop := make(map[int64]bool, len(seqs)-store.SaveVersionRetention)
	for _, sq := range seqs[:len(seqs)-store.SaveVersionRetention] {
		drop[sq] = true
	}
	kept := s.saveVersions[:0]
	for _, v := range s.saveVersions {
		if !drop[v.Seq] {
			kept = append(kept, v)
		}
	}
	s.saveVersions = kept
}

func (s *Store) ListSaveVersions(_ context.Context, syncID, nodeID string, limit int) ([]store.SaveVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Newest-first by seq. saveVersions is in ascending seq order, so walk it
	// backwards.
	out := make([]store.SaveVersion, 0)
	for i := len(s.saveVersions) - 1; i >= 0; i-- {
		v := s.saveVersions[i]
		if v.SyncID != syncID || v.NodeID != nodeID {
			continue
		}
		out = append(out, v)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *Store) GetSaveVersionData(_ context.Context, syncID string, seq int64) (store.SaveVersion, []byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.saveVersions {
		// Bind the lookup to (sync_id, seq): a seq that belongs to a DIFFERENT
		// sync is not found, so a guessed cross-sync seq can never be loaded.
		if v.Seq != seq || v.SyncID != syncID {
			continue
		}
		b, ok := s.saveBlobs[v.Hash]
		if !ok {
			// Should not happen: a live version always has its blob (GC only removes
			// orphan blobs). Treat a dangling reference as not-found.
			return store.SaveVersion{}, nil, store.ErrNotFound
		}
		data := make([]byte, len(b.data))
		copy(data, b.data)
		return v, data, nil
	}
	return store.SaveVersion{}, nil, store.ErrNotFound
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
	return out
}
