// Package engine is the heart of RetroSync's play-sync: it AUTO-MIRRORS every
// sync. Each Poll runs HASH-BACKED change detection on a sync's members (a cheap
// mtime+size stat gate, then a sha256 content compare only when the stat moved)
// and decides by the distinct content hashes of the changed members: zero
// changed is a noop; a single distinct hash (one changer, OR several that reached
// the same bytes) is the agreed-latest content, fanned out to every member that
// lacks it; two or more distinct hashes is a genuine fork, flagged as a conflict
// and paused (mirroring stays paused until a human resolves it). There is no
// primary, no binding, no activate/deactivate/take-over: the only human action is
// resolving a conflict. It implements docs/state-machine.md.
//
// A *sync* is the unit of mirroring: a specific set of (node, save-file)
// members that sync together (its "game" is just a free-text display label the
// engine ignores; many syncs may share one label). The
// engine operates on a sync id and that sync's SyncMembers — each member's
// node_id + path is the in-scope (node, file) pair. There is no peer-scope: a
// sync's members ARE its scope. Per-sync runtime state (conflict_at,
// last_synced) lives on the sync row itself.
//
// The engine is pure logic over two ports: a store.Store for persistence and a
// reach.Reach per node for filesystem access. It owns no goroutines, no timers,
// and no real I/O. The background ticker that calls Poll on a schedule lives in
// internal/daemon; the HTTP/API surface lives in internal/web.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/safepath"
	"github.com/a-mcf/retrosync/internal/store"
)

// ResolveReach maps a node to the Reach adapter that talks to it. Injecting
// this keeps the engine adapter-agnostic: tests supply fakes; production wires
// the real syncthing-share / ssh adapters.
type ResolveReach func(node store.Node) (reach.Reach, error)

// Clock returns the current time. Injected so tests get deterministic
// timestamps (last_synced, conflict_at, log ts).
type Clock func() time.Time

// Engine implements the play-sync operations. Construct with New.
type Engine struct {
	store   store.Store
	resolve ResolveReach
	clock   Clock
}

// New builds an Engine. clock may be nil, in which case time.Now (UTC) is used.
func New(s store.Store, resolve ResolveReach, clock Clock) *Engine {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Engine{store: s, resolve: resolve, clock: clock}
}

// Sentinel errors specific to the engine. Callers compare with errors.Is.
var (
	// ErrSourceMissing is returned by ResolveConflict when the chosen winner node
	// currently holds no file (there is nothing to fan out from it).
	ErrSourceMissing = errors.New("engine: chosen source node has no save")
	// ErrNoPath is returned when a referenced node is not a member of the sync
	// (e.g. the resolve winner names a node not in the sync's members).
	ErrNoPath = errors.New("engine: node is not a member of the sync")
	// ErrNotConflicted is returned by ResolveConflict when the sync is not in
	// conflict (conflict_at == nil): there is nothing to resolve, and forcing a
	// fan-out would silently overwrite members the human never reviewed.
	ErrNotConflicted = errors.New("engine: sync is not in conflict")
	// ErrSmokeTestUnsupported is returned by SmokeTest when the node's reach
	// strategy has no adapter wired yet (today: ssh). It is the engine's own
	// sentinel so the web layer can map "not supported yet" WITHOUT importing
	// internal/reach (it wraps reach.ErrUnsupportedReach for engine-side callers
	// that still want the underlying cause).
	ErrSmokeTestUnsupported = errors.New("engine: smoke-test not supported for this reach")
	// ErrBrowseUnsupported is returned by BrowseNode when the node's reach
	// strategy has no adapter wired yet (today: ssh). Like ErrSmokeTestUnsupported
	// it is the engine's own sentinel so the web save-file picker can render
	// "browsing not supported for this node" WITHOUT importing internal/reach; the
	// underlying reach.ErrUnsupportedReach stays in the chain.
	// TODO(slice-ssh): ssh nodes become browsable once the ssh/sftp adapter lands.
	ErrBrowseUnsupported = errors.New("engine: browsing not supported for this reach")
	// ErrBrowseUnsafePath is returned by BrowseNode when the requested relPath is
	// rejected by the adapter's containment check (it is absolute, or it escapes
	// the node root via ".."/symlink). It is the engine's own sentinel — wrapping
	// safepath.ErrUnsafePath — so the web layer can map an unsafe browse path to a
	// 400 WITHOUT importing internal/reach or internal/reach/safepath. The
	// underlying safepath.ErrUnsafePath stays in the chain.
	ErrBrowseUnsafePath = errors.New("engine: browse path is unsafe (escapes node root)")
)

// scopedNode pairs an in-scope node (a sync member) with its member path and
// resolved Reach.
type scopedNode struct {
	node store.Node
	path string
	r    reach.Reach
}

// changedMember is a member that genuinely changed content this pass (per the
// two-tier detection in Poll): its current content hash is carried so the
// decision can group changers by distinct hash (a true fork is 2+ distinct
// hashes; one distinct hash — however many members share it — is an agreed
// content to propagate).
type changedMember struct {
	sn   scopedNode
	hash string // current content hash (empty only when the member vanished)
}

// Poll runs ONE auto-mirror pass for syncID. It loads the sync; if the sync is
// already in conflict (conflict_at != nil) it does nothing (paused, awaiting
// human resolution). Otherwise it runs HASH-BACKED two-tier change detection on
// every member and decides by the CONTENT HASHES of the changed members:
//
//	distinct hashes among changed members | action
//	0 members changed                     | noop
//	exactly ONE distinct hash             | that is the agreed-latest content -> fan it out to every member lacking it; mark synced
//	TWO OR MORE distinct hashes           | a genuine fork -> set conflict_at, log, stop (paused until resolved)
//
// Two-tier detection per member (keeps a noop poll hash-free):
//  1. Stat (mtime+size). If it equals the manifest -> UNCHANGED; do NOT hash.
//  2. Only when the stat differs: compute the current Hash and compare to the
//     manifest's stored sha256.
//     - hash EQUAL -> a "touch" (mtime moved, bytes identical): reconcile the
//     manifest mtime/size to current so the next poll fast-paths it, and do NOT
//     count it as changed.
//     - hash DIFFERS -> a real content change.
//
// There is no primary: when a single distinct hash exists among the changers,
// any one of them is the source (they are byte-identical). Crucially, every
// OTHER member is then unchanged-since-manifest, so the fan-out only overwrites
// members that did NOT change — a member that changed to DIFFERENT content is by
// construction a second distinct hash, which is the conflict case where NOTHING
// is written. The manifest and last_synced advance only after a successful write
// (crash-safety: the manifest trails the actual write).
func (e *Engine) Poll(ctx context.Context, syncID string) error {
	sy, err := e.store.GetSync(ctx, syncID)
	if err != nil {
		return fmt.Errorf("engine: get sync: %w", err)
	}
	if sy.ConflictAt != nil {
		// Paused: a conflicted sync is skipped until a human resolves it.
		return nil
	}

	scoped, err := e.inScopeNodes(ctx, syncID)
	if err != nil {
		return err
	}
	manifest, err := e.manifestMap(ctx, syncID)
	if err != nil {
		return err
	}

	now := e.clock()

	// Two-tier detection: stat every member (cheap), and only hash the ones whose
	// stat differs from the manifest. A "touch" (stat moved, bytes identical)
	// reconciles the manifest and is NOT a change. A vanished member (was in the
	// manifest, now absent) is a change with an empty hash.
	metas := make(map[string]reach.FileMeta, len(scoped))
	present := make(map[string]bool, len(scoped))
	var changedSet []changedMember
	for _, sn := range scoped {
		meta, ok, err := e.statOpt(ctx, sn)
		if err != nil {
			return err
		}
		metas[sn.node.ID] = meta
		present[sn.node.ID] = ok

		me := manifest[sn.node.ID]
		// Tier 1: fast stat gate. Equal stat => unchanged, no hash.
		if !statDiffers(me, meta, ok) {
			continue
		}
		// A member that vanished (manifest had a file, now absent) is a change with
		// no current content. Surface it (handled below as the vanished case).
		if !ok {
			changedSet = append(changedSet, changedMember{sn: sn})
			continue
		}
		// Tier 2: hash truth. Compute the current content hash and compare to the
		// manifest's stored sha256.
		curHash, err := sn.r.Hash(ctx, sn.path)
		if err != nil {
			return fmt.Errorf("engine: hash %s: %w", sn.node.ID, err)
		}
		if me.SHA256 != nil && *me.SHA256 == curHash {
			// A touch: mtime/size moved but the bytes are identical. Reconcile the
			// manifest's mtime/size to current (keeping the SAME hash) so the next
			// poll fast-paths it. NOT counted as changed; nothing propagates.
			if err := e.setManifest(ctx, syncID, sn.node.ID, meta, curHash, now); err != nil {
				return err
			}
			continue
		}
		// A real content change.
		changedSet = append(changedSet, changedMember{sn: sn, hash: curHash})
	}

	if len(changedSet) == 0 {
		// Nothing genuinely changed (covers an all-noop poll and a touch-only poll):
		// noop.
		return nil
	}

	// A vanished changed member (manifest had a file, now absent) is treated as a
	// conflict rather than propagating the deletion to the others. Surfacing beats
	// destruction. Check this BEFORE the distinct-hash decision so a lone deletion
	// pauses the sync instead of (with no current content) being mistaken for an
	// agreed propagation.
	for _, cm := range changedSet {
		if !present[cm.sn.node.ID] {
			return e.flagConflict(ctx, syncID, now,
				fmt.Sprintf("member %s vanished (manifest had a file); sync paused", cm.sn.node.ID))
		}
	}

	// Group the changed members by their current content hash. A genuine fork is
	// 2+ DISTINCT hashes; a single distinct hash (however many members share it) is
	// an agreed content to propagate. (Sized to changedSet; it only ever holds 1
	// before the >=2 branch below short-circuits, so the capacity is a loose upper
	// bound, not a tight one.)
	distinct := make(map[string]struct{}, len(changedSet))
	for _, cm := range changedSet {
		distinct[cm.hash] = struct{}{}
	}

	if len(distinct) >= 2 {
		// A genuine fork: the changed members hold DIFFERENT content. Flag and pause.
		// Nothing is written — every distinct-hash changer keeps its file.
		ids := make([]string, 0, len(changedSet))
		for _, cm := range changedSet {
			ids = append(ids, cm.sn.node.ID)
		}
		return e.flagConflict(ctx, syncID, now,
			fmt.Sprintf("%d members changed to %d distinct contents (%s); retrosync won't choose — sync paused",
				len(changedSet), len(distinct), strings.Join(ids, ", ")))
	}

	// Exactly one distinct hash among the changers: the agreed-latest content. Pick
	// any changer as the source (they are byte-identical) and fan it out to every
	// member that does not already hold it. Every NON-changer is unchanged since the
	// manifest, so overwriting it is safe propagation; there is no distinct-hash
	// changer to clobber (that would be the conflict case above).
	//
	// Safe to index [0]: the len(changedSet)==0 case returned earlier, so changedSet
	// is non-empty by construction here.
	src := changedSet[0]
	srcHash := src.hash
	// Every changer holds the agreed content already (one distinct hash), so they
	// must NOT be re-written — fan out only to members that don't already have it.
	// This also makes the multi-changer-same-content case write nothing spurious.
	alreadyHave := make(map[string]bool, len(changedSet))
	for _, cm := range changedSet {
		alreadyHave[cm.sn.node.ID] = true
	}
	if err := e.fanOut(ctx, syncID, src.sn, scoped, metas[src.sn.node.ID], srcHash, alreadyHave, reasonPropagate, now); err != nil {
		return err
	}
	// Record each changer's own manifest state (each already holds the agreed
	// content), carrying the agreed hash. The source is included here.
	for _, cm := range changedSet {
		if err := e.setManifest(ctx, syncID, cm.sn.node.ID, metas[cm.sn.node.ID], srcHash, now); err != nil {
			return err
		}
	}
	return e.markSynced(ctx, syncID, now)
}

// NodeState is a single in-scope node's CURRENT live file state, as read by a
// fresh Stat (not the manifest). It is what the conflict-resolution modal
// renders so the human can see each node's divergent save before picking a
// winner (docs/state-machine.md "Conflict handling": "UI shows every node's
// current state (mtime, size)").
type NodeState struct {
	// NodeID is the in-scope node.
	NodeID string
	// Present is false when the node currently has no file (reach.ErrNotExist).
	Present bool
	// Mtime / Size are the live file's metadata; zero values when !Present.
	Mtime time.Time
	Size  int64
}

// NodeStates returns the live per-node state of every member of the sync, sorted
// by NodeID for determinism. It is read-only: no manifest or filesystem
// mutation. A node whose file is absent is returned with Present=false; any Stat
// error other than reach.ErrNotExist is a real error.
//
// The in-scope set is exactly the sync's members (a sync's members ARE its
// scope). This is the per-member view the conflict-resolution UI renders so the
// human can see every divergent save before picking a winner (docs/state-machine.md
// "Conflict handling").
func (e *Engine) NodeStates(ctx context.Context, syncID string) ([]NodeState, error) {
	scoped, err := e.inScopeNodes(ctx, syncID)
	if err != nil {
		return nil, err
	}
	out := make([]NodeState, 0, len(scoped))
	for _, sn := range scoped {
		meta, present, err := e.statOpt(ctx, sn)
		if err != nil {
			return nil, err
		}
		out = append(out, NodeState{
			NodeID:  sn.node.ID,
			Present: present,
			Mtime:   meta.Mtime,
			Size:    meta.Size,
		})
	}
	// inScopeNodes already sorts by node id; NodeStates inherits that order.
	return out, nil
}

// smokeTestRoot is the path SmokeTest stats to probe reachability: the node's
// save root itself. Per reach.Reach path semantics every path is relative to
// the node's save root, so "." names that root. For a syncthing-share node
// resolve roots the localfs adapter at reach_config.path, so Stat(".") stats
// that directory.
const smokeTestRoot = "."

// SmokeTest verifies that a node is reachable, for the registry "Test" button
// and the pairing smoke-test flow (docs/auth.md "Pairing a new device": "stat a
// known path"). It resolves the node's reach adapter and stats the node's save
// root (".").
//
//   - syncthing-share: a nil return means the share directory exists and is
//     readable (the node is reachable). A non-nil return is the underlying
//     reach error, surfaced to the operator (e.g. the share path is missing).
//   - ssh: resolve has no adapter yet, so this returns ErrSmokeTestUnsupported
//     (which wraps reach.ErrUnsupportedReach). The web layer matches the engine
//     sentinel and renders "smoke-test not supported yet (ssh adapter pending)",
//     so it never needs to import internal/reach.
//     TODO(slice-ssh): a real ssh adapter makes this an actual reachability probe.
//
// SmokeTest is read-only: it never mutates the store or the node. Its result is
// transient — nothing is persisted (the web handler renders a one-off
// reachable/error pill) — so the engine stays a pure logic-over-ports component.
// A missing node returns store.ErrNotFound.
func (e *Engine) SmokeTest(ctx context.Context, nodeID string) error {
	node, err := e.store.GetNode(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("engine: smoke-test get node %q: %w", nodeID, err)
	}
	r, err := e.resolve(node)
	if err != nil {
		// A resolve with no adapter wired (ssh today) surfaces as the engine's own
		// ErrSmokeTestUnsupported so the web layer can map "not supported yet"
		// without importing internal/reach. The underlying reach.ErrUnsupportedReach
		// is preserved in the chain for engine-side callers.
		if errors.Is(err, reach.ErrUnsupportedReach) {
			return fmt.Errorf("engine: smoke-test %q: %w: %w", nodeID, ErrSmokeTestUnsupported, err)
		}
		return fmt.Errorf("engine: smoke-test resolve %q: %w", nodeID, err)
	}
	if _, err := r.Stat(ctx, smokeTestRoot); err != nil {
		return fmt.Errorf("engine: smoke-test stat %q: %w", nodeID, err)
	}
	return nil
}

// DirEntry is one entry in a node directory listing returned by BrowseNode. It
// is the engine's OWN value type (a copy of reach.DirEntry's fields) so the web
// layer can render the picker by depending on internal/engine alone, WITHOUT
// importing internal/reach — exactly as NodeState lets the conflict modal avoid
// a reach import. It carries metadata only; never file contents.
type DirEntry struct {
	// Name is the entry's base name (one path component, no separators).
	Name string
	// IsDir is true for a subdirectory (the picker re-browses) and false for a
	// regular file (the picker selects it).
	IsDir bool
	// Size is the file's size in bytes (0 for a directory).
	Size int64
	// Mtime is the entry's last-modified time.
	Mtime time.Time
}

// BrowseNode lists the directory entries directly under relPath on the given
// node, for the registry's save-file picker (the operator browses a node's
// mounted save directory and clicks the real file instead of typing its path).
// relPath is node-relative (empty / "." names the node's save root); the adapter
// resolves and contains it. It returns metadata only — names, types, sizes,
// mtimes — never file contents.
//
//   - syncthing-share: resolves to the localfs adapter and lists the directory
//     (safepath rejects any traversal/escape path before any I/O).
//   - ssh: resolve has no adapter yet, so this returns ErrBrowseUnsupported
//     (which wraps reach.ErrUnsupportedReach). The web layer matches the engine
//     sentinel and renders "browsing not supported for this node" WITHOUT
//     importing internal/reach.
//     TODO(slice-ssh): a real ssh adapter makes ssh nodes browsable.
//
// BrowseNode is read-only: it never mutates the store or the node. A missing
// node returns store.ErrNotFound. A traversal/unsafe relPath surfaces the
// adapter's safepath rejection (safepath.ErrUnsafePath) in the error chain.
func (e *Engine) BrowseNode(ctx context.Context, nodeID, relPath string) ([]DirEntry, error) {
	node, err := e.store.GetNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("engine: browse get node %q: %w", nodeID, err)
	}
	r, err := e.resolve(node)
	if err != nil {
		// No adapter wired (ssh today) surfaces as the engine's own
		// ErrBrowseUnsupported so the web layer maps "not supported yet" without
		// importing internal/reach; the underlying cause stays in the chain.
		if errors.Is(err, reach.ErrUnsupportedReach) {
			return nil, fmt.Errorf("engine: browse %q: %w: %w", nodeID, ErrBrowseUnsupported, err)
		}
		return nil, fmt.Errorf("engine: browse resolve %q: %w", nodeID, err)
	}
	entries, err := r.List(ctx, relPath)
	if err != nil {
		// An unsafe (escaping/absolute) path is the adapter's safepath rejection;
		// surface it as the engine's own sentinel so the web maps it to a 400
		// without importing internal/reach/safepath. The underlying cause is kept.
		if errors.Is(err, safepath.ErrUnsafePath) {
			return nil, fmt.Errorf("engine: browse %q path %q: %w: %w", nodeID, relPath, ErrBrowseUnsafePath, err)
		}
		return nil, fmt.Errorf("engine: browse list %q: %w", nodeID, err)
	}
	// Map reach.DirEntry -> engine.DirEntry so callers (the web picker) depend on
	// engine alone, not internal/reach.
	out := make([]DirEntry, len(entries))
	for i, de := range entries {
		out[i] = DirEntry{Name: de.Name, IsDir: de.IsDir, Size: de.Size, Mtime: de.Mtime}
	}
	return out, nil
}

// ResolveConflict resolves a flagged conflict by making winnerNodeID's current
// save the authority and fanning it out to every other in-scope node. Each other
// member's pre-resolution bytes are captured into the server-side save-version
// store BEFORE being overwritten (the recovery net replaces the old device-side
// .retrosync-conflict-<ts> sibling backups). It clears the conflict only on full
// success (docs/state-machine.md "Conflict handling").
//
// Preconditions:
//   - The sync MUST be in conflict (conflict_at != nil); otherwise
//     ErrNotConflicted (resolving a non-conflicted sync is not allowed — it
//     would overwrite members the human never reviewed).
//   - winnerNodeID MUST be in scope (ErrNoPath otherwise) and MUST currently
//     hold a file (ErrSourceMissing otherwise).
//
// Ordering / crash-safety:
//  1. Fan the winner's bytes+mtime out to every other in-scope node. Each
//     destination that currently HAS a file has its bytes captured to the server
//     save-version store (reason "conflict-resolve") BEFORE the overwrite — a
//     hard gate: a capture failure aborts that write and leaves the sync
//     conflicted (re-resolvable). The manifest advances only AFTER each
//     successful write.
//  2. Clear conflict_at and set last_synced.
//
// If any capture or fan-out write fails, conflict_at is left set (the sync stays
// conflicted and re-resolvable) and the error is returned.
func (e *Engine) ResolveConflict(ctx context.Context, syncID, winnerNodeID string) error {
	sy, err := e.store.GetSync(ctx, syncID)
	if err != nil {
		return fmt.Errorf("engine: get sync: %w", err)
	}
	if sy.ConflictAt == nil {
		return fmt.Errorf("engine: sync %q: %w", syncID, ErrNotConflicted)
	}

	scoped, err := e.inScopeNodes(ctx, syncID)
	if err != nil {
		return err
	}
	winner, ok := byID(scoped, winnerNodeID)
	if !ok {
		return fmt.Errorf("engine: winner %q: %w", winnerNodeID, ErrNoPath)
	}
	// TODO(slice-daemon): TOCTOU window — the winner is stat'd here, re-read in
	// fanOut, and each loser is independently re-stat'd/re-read for capture, so a
	// file can change between these stats/reads. Harmless today (no concurrent
	// poll loop drives this path), but when the timer loop lands a file mutated
	// mid-resolution could be captured or fanned out inconsistently; revisit to
	// snapshot each node once under the concurrency model then in place.
	winnerMeta, winnerPresent, err := e.statOpt(ctx, winner)
	if err != nil {
		return err
	}
	if !winnerPresent {
		return fmt.Errorf("engine: winner %q: %w", winnerNodeID, ErrSourceMissing)
	}
	// The winner's content hash is the authority recorded in the manifest for the
	// winner and every node it is fanned out to.
	winnerHash, err := winner.r.Hash(ctx, winner.path)
	if err != nil {
		return fmt.Errorf("engine: hash winner %s: %w", winner.node.ID, err)
	}

	now := e.clock()

	// Fan out the winner to every other in-scope node, advancing the manifest
	// (carrying the winner's hash) only after each successful write. fanOut
	// captures each loser-with-a-file's pre-resolution bytes into the server
	// save-version store (reason "conflict-resolve") BEFORE overwriting them — a
	// hard gate: a capture or write failure returns the error and (because we have
	// NOT cleared conflict_at) leaves the sync conflicted for re-resolution. No
	// skip set here: a resolve deliberately overwrites every loser with the winner.
	if err := e.fanOut(ctx, syncID, winner, scoped, winnerMeta, winnerHash, nil, reasonConflictResolve, now); err != nil {
		return err
	}
	// Record the winner's own manifest state (it is the authority for this pass),
	// carrying its content hash.
	if err := e.setManifest(ctx, syncID, winner.node.ID, winnerMeta, winnerHash, now); err != nil {
		return err
	}

	// Step 3: only now, after every write succeeded, clear the conflict and mark
	// the sync synced. Order matters for crash-safety: mark synced first, then
	// clear the conflict last — so a crash between the two leaves the sync
	// conflicted (re-resolvable) rather than un-paused-but-unsynced.
	if err := e.store.MarkSyncSynced(ctx, syncID, now); err != nil {
		return fmt.Errorf("engine: mark synced: %w", err)
	}
	if err := e.store.SetSyncConflict(ctx, syncID, nil); err != nil {
		return fmt.Errorf("engine: clear conflict: %w", err)
	}
	return nil
}

// RestoreVersion makes a previously-captured save version the current
// authoritative content of its (sync, node) member, and propagates it to the
// rest of the sync — effectively "this old save is now the current save
// everywhere" (the recovery net's undo, slice-20).
//
// Flow:
//  1. Load the version + its bytes (ErrNotFound if the seq was pruned/gone).
//  2. Capture the TARGET member's current bytes (it's an overwrite), then write
//     the restored bytes to the target's member path atomically; advance its
//     manifest (mtime/size/hash) so it is the new authoritative content.
//  3. Fan the restored bytes out to every OTHER member, capturing each one's
//     pre-restore bytes before overwriting (reason "restore").
//  4. Clear any conflict_at and mark the sync synced.
//
// Capture-before-overwrite is a hard gate throughout (a capture failure aborts
// the write); a partial restore leaves the manifest trailing the actual writes
// and is retried/re-driven safely. The restored bytes' mtime is set to the
// resolution time (the injected clock) so every member converges to one mtime and
// the next poll is a clean noop.
//
// AUTHORIZATION: seq is loaded BOUND to syncID (the caller's authorized sync) via
// GetSaveVersionData(ctx, syncID, seq). A seq belonging to a DIFFERENT sync is
// ErrNotFound, so a caller authorized on sync A cannot restore a version of sync
// B by guessing its (bigserial, guessable) seq. The whole operation runs ONLY on
// syncID — never on the version's own ver.SyncID — so a restore can only ever
// mutate the sync the caller was authorized against.
func (e *Engine) RestoreVersion(ctx context.Context, syncID string, seq int64) error {
	ver, data, err := e.store.GetSaveVersionData(ctx, syncID, seq)
	if err != nil {
		return fmt.Errorf("engine: restore load version %d (sync %s): %w", seq, syncID, err)
	}
	// By construction GetSaveVersionData binds seq to syncID, so ver.SyncID ==
	// syncID here. We still operate exclusively on the passed-in syncID (never on
	// ver.SyncID) so the authorized sync is the only thing this can ever touch.

	scoped, err := e.inScopeNodes(ctx, syncID)
	if err != nil {
		return err
	}
	target, ok := byID(scoped, ver.NodeID)
	if !ok {
		// The captured member is no longer in the sync (removed since capture).
		return fmt.Errorf("engine: restore target %q: %w", ver.NodeID, ErrNoPath)
	}

	now := e.clock()
	// The restored content's hash is the version's hash (content-addressed), used
	// as the manifest sha256 for the target and every fanned-out member.
	restoredHash := ver.Hash

	// Step 1: overwrite the TARGET member with the restored bytes, capturing its
	// current bytes first (it's an overwrite of recoverable content). The restored
	// content gets the resolution mtime so all members converge.
	if err := e.captureBeforeOverwrite(ctx, syncID, target, reasonRestore); err != nil {
		return err
	}
	if err := target.r.WriteAtomic(ctx, target.path, data, now); err != nil {
		return fmt.Errorf("engine: restore write target %s: %w", target.node.ID, err)
	}
	restoredMeta := reach.FileMeta{Mtime: now, Size: int64(len(data))}
	if err := e.setManifest(ctx, syncID, target.node.ID, restoredMeta, restoredHash, now); err != nil {
		return err
	}
	bytes := restoredMeta.Size
	if err := e.appendLog(ctx, store.LogEntry{
		SyncID:   syncID,
		FromNode: target.node.ID,
		ToNode:   target.node.ID,
		Bytes:    &bytes,
		SrcMtime: tptr(now),
		Outcome:  store.OutcomeOK,
		Message:  fmt.Sprintf("restore version %d", seq),
		TS:       now,
	}); err != nil {
		return err
	}

	// Step 2: fan the restored bytes out to every OTHER member, capturing each
	// one's pre-restore bytes before overwriting. The target is the source; it is
	// skipped by fanOut (src == dst).
	if err := e.fanOut(ctx, syncID, target, scoped, restoredMeta, restoredHash, nil, reasonRestore, now); err != nil {
		return err
	}

	// Step 3: the restored content is now authoritative everywhere — clear any
	// conflict and mark synced. Mark synced first, then clear the conflict (a crash
	// between leaves the sync conflicted/re-resolvable rather than un-paused-unsynced).
	if err := e.store.MarkSyncSynced(ctx, syncID, now); err != nil {
		return fmt.Errorf("engine: restore mark synced: %w", err)
	}
	if err := e.store.SetSyncConflict(ctx, syncID, nil); err != nil {
		return fmt.Errorf("engine: restore clear conflict: %w", err)
	}
	return nil
}

// Reasons recorded on captured save versions (docs/state-machine.md). They tag a
// snapshot with why it was taken, for the history UI.
const (
	reasonPropagate       = "propagate"
	reasonConflictResolve = "conflict-resolve"
	reasonRestore         = "restore"
)

// captureBeforeOverwrite snapshots dst's CURRENT bytes into the server-side
// save-version store (content-addressed by sha256) before they are overwritten,
// tagging the snapshot with reason. It is the recovery net (slice-20): it
// REPLACES the old device-side <path>.retrosync-conflict-<ts> sibling backup with
// a clean central store, so a bad propagation/resolve is always recoverable
// without cluttering device dirs (or replicating backups via Syncthing).
//
// It is a HARD GATE — every caller invokes it immediately before WriteAtomic and
// returns its error, so a capture failure ABORTS the write (the manifest is not
// advanced; the next poll retries) and nothing recoverable is destroyed
// un-captured. A destination with no current file (reach.ErrNotExist) has nothing
// to capture and is a no-op. The store dedups by content hash and prunes to the
// retention cap per (sync, node), so repeated identical captures do not churn.
func (e *Engine) captureBeforeOverwrite(ctx context.Context, syncID string, dst scopedNode, reason string) error {
	data, err := dst.r.Read(ctx, dst.path)
	if err != nil {
		if errors.Is(err, reach.ErrNotExist) {
			// No current file: nothing to capture. The dst still receives the write.
			return nil
		}
		return fmt.Errorf("engine: capture read %s: %w", dst.node.ID, err)
	}
	hash, err := dst.r.Hash(ctx, dst.path)
	if err != nil {
		return fmt.Errorf("engine: capture hash %s: %w", dst.node.ID, err)
	}
	if err := e.store.PutSaveVersion(ctx, syncID, dst.node.ID, hash, data, reason); err != nil {
		return fmt.Errorf("engine: capture %s: %w", dst.node.ID, err)
	}
	return nil
}

// fanOut reads the source file once and WriteAtomic's it to every other
// in-scope node (except those in skip, which already hold the agreed content),
// advancing each written node's manifest — carrying srcHash — and appending a
// sync_log "ok" row per copy. The destination mtime is the source's mtime so
// the next poll sees source == peer.
//
// skip names members that must NOT be overwritten because they already hold the
// agreed content (the byte-identical co-changers in the one-distinct-hash case).
// It is the guard that keeps the propagate path from re-writing a member that
// changed to the SAME content — and, by construction of the caller, there is
// never a member in scope that changed to DIFFERENT content (that is the
// conflict case, where fanOut is not called at all).
//
// Capture-before-overwrite (the recovery net, slice-20): before WriteAtomic
// overwrites a destination that CURRENTLY HAS a file, fanOut snapshots that
// destination's current bytes into the server-side save-version store under
// reason. It is a HARD GATE — if the capture fails, the write is ABORTED and the
// error surfaced (the manifest is not advanced, the next poll retries), so no
// recoverable bytes are ever destroyed without a snapshot. A destination with no
// current file has nothing to capture (skip).
func (e *Engine) fanOut(ctx context.Context, syncID string, src scopedNode, scoped []scopedNode, srcMeta reach.FileMeta, srcHash string, skip map[string]bool, reason string, now time.Time) error {
	data, err := src.r.Read(ctx, src.path)
	if err != nil {
		return fmt.Errorf("engine: read source %s: %w", src.node.ID, err)
	}
	for _, dst := range scoped {
		if dst.node.ID == src.node.ID {
			continue
		}
		if skip[dst.node.ID] {
			continue
		}
		// dst mtime *before* overwrite, for the log (nil if dst absent or
		// unreadable; this is a best-effort diagnostic, not load-bearing).
		var dstMtimeBefore *time.Time
		if fm, present, err := e.statOpt(ctx, dst); err == nil && present {
			dstMtimeBefore = &fm.Mtime
		}

		// Capture-before-overwrite: snapshot the destination's current bytes into
		// the server store BEFORE overwriting them. A hard gate — a capture failure
		// aborts this write (manifest not advanced; next poll retries) so nothing
		// recoverable is destroyed un-captured.
		if err := e.captureBeforeOverwrite(ctx, syncID, dst, reason); err != nil {
			return err
		}

		if err := dst.r.WriteAtomic(ctx, dst.path, data, srcMeta.Mtime); err != nil {
			// Crash-safety: the write failed, so we do NOT advance dst's manifest.
			// Log the error and abort the pass; the next poll re-detects and
			// retries.
			_ = e.appendLog(ctx, store.LogEntry{
				SyncID:   syncID,
				FromNode: src.node.ID,
				ToNode:   dst.node.ID,
				SrcMtime: tptr(srcMeta.Mtime),
				DstMtime: dstMtimeBefore,
				Outcome:  store.OutcomeError,
				Message:  err.Error(),
				TS:       now,
			})
			return fmt.Errorf("engine: write %s: %w", dst.node.ID, err)
		}

		// Advance the written node's manifest to the post-write state (mtime ==
		// source mtime, size == source size, sha256 == source hash). The destination
		// now holds the source's exact bytes, so it inherits the source's hash.
		written := reach.FileMeta{Mtime: srcMeta.Mtime, Size: srcMeta.Size}
		if err := e.setManifest(ctx, syncID, dst.node.ID, written, srcHash, now); err != nil {
			return err
		}
		bytes := srcMeta.Size
		if err := e.appendLog(ctx, store.LogEntry{
			SyncID:   syncID,
			FromNode: src.node.ID,
			ToNode:   dst.node.ID,
			Bytes:    &bytes,
			SrcMtime: tptr(srcMeta.Mtime),
			DstMtime: dstMtimeBefore,
			Outcome:  store.OutcomeOK,
			TS:       now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// flagConflict sets conflict_at on the sync and appends a single conflict log
// row carrying msg, then stops. Auto-mirroring is paused for that sync until a
// human resolves it (subsequent Polls short-circuit on conflict_at != nil).
func (e *Engine) flagConflict(ctx context.Context, syncID string, now time.Time, msg string) error {
	if err := e.store.SetSyncConflict(ctx, syncID, &now); err != nil {
		return fmt.Errorf("engine: flag conflict: %w", err)
	}
	return e.appendLog(ctx, store.LogEntry{
		SyncID:  syncID,
		Outcome: store.OutcomeConflict,
		Message: msg,
		TS:      now,
	})
}

// markSynced advances last_synced on the sync after a successful mirror pass.
func (e *Engine) markSynced(ctx context.Context, syncID string, now time.Time) error {
	if err := e.store.MarkSyncSynced(ctx, syncID, now); err != nil {
		return fmt.Errorf("engine: update last_synced: %w", err)
	}
	return nil
}

// --- helpers -------------------------------------------------------------

// inScopeNodes returns the sync's members (each member's node + path), paired
// with a resolved Reach. A sync's members ARE its scope — there is no further
// filtering. Results are sorted by node id for determinism.
func (e *Engine) inScopeNodes(ctx context.Context, syncID string) ([]scopedNode, error) {
	members, err := e.store.ListSyncMembers(ctx, syncID)
	if err != nil {
		return nil, fmt.Errorf("engine: list sync members: %w", err)
	}

	out := make([]scopedNode, 0, len(members))
	for _, m := range members {
		node, err := e.store.GetNode(ctx, m.NodeID)
		if err != nil {
			return nil, fmt.Errorf("engine: get node %s: %w", m.NodeID, err)
		}
		r, err := e.resolve(node)
		if err != nil {
			return nil, fmt.Errorf("engine: resolve reach for %s: %w", node.ID, err)
		}
		out = append(out, scopedNode{node: node, path: m.Path, r: r})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].node.ID < out[j].node.ID })
	return out, nil
}

// statOpt stats a node, returning (meta, present, err). ErrNotExist maps to
// present=false with a nil err; any other error is returned.
func (e *Engine) statOpt(ctx context.Context, sn scopedNode) (reach.FileMeta, bool, error) {
	fm, err := sn.r.Stat(ctx, sn.path)
	if err != nil {
		if errors.Is(err, reach.ErrNotExist) {
			return reach.FileMeta{}, false, nil
		}
		return reach.FileMeta{}, false, fmt.Errorf("engine: stat %s: %w", sn.node.ID, err)
	}
	return fm, true, nil
}

func (e *Engine) manifestMap(ctx context.Context, syncID string) (map[string]store.ManifestEntry, error) {
	entries, err := e.store.ListManifestBySync(ctx, syncID)
	if err != nil {
		return nil, fmt.Errorf("engine: list manifest: %w", err)
	}
	m := make(map[string]store.ManifestEntry, len(entries))
	for _, me := range entries {
		m[me.NodeID] = me
	}
	return m, nil
}

// setManifest records a member's last-known file state: mtime, size, AND the
// content sha256. The hash is the content-truth consulted by the next poll's
// tier-2 compare (a touch reconciles mtime/size while keeping the same hash; a
// real change records the new hash). sha256 must be the lowercase-hex digest of
// the member's CURRENT content; pass "" only where a hash is genuinely unknown.
func (e *Engine) setManifest(ctx context.Context, syncID, nodeID string, meta reach.FileMeta, sha256 string, now time.Time) error {
	size := meta.Size
	// Truncate to the comparison resolution before storing so the manifest value
	// matches what a later statDiffers compare will use, regardless of where it is
	// persisted. This is belt-and-suspenders: statDiffers truncates both sides too,
	// so correctness does not depend on it — but storing the truncated value keeps
	// stored and compared mtimes at the same precision (and matches what Postgres
	// timestamptz would store anyway).
	mtime := meta.Mtime.UTC().Truncate(mtimeResolution)
	m := store.ManifestEntry{
		SyncID:      syncID,
		NodeID:      nodeID,
		Mtime:       &mtime,
		Size:        &size,
		LastChecked: &now,
	}
	if sha256 != "" {
		h := sha256
		m.SHA256 = &h
	}
	if err := e.store.SetManifest(ctx, m); err != nil {
		return fmt.Errorf("engine: set manifest %s: %w", nodeID, err)
	}
	return nil
}

func (e *Engine) appendLog(ctx context.Context, le store.LogEntry) error {
	if err := e.store.AppendLog(ctx, le); err != nil {
		return fmt.Errorf("engine: append log: %w", err)
	}
	return nil
}

// mtimeResolution is the resolution at which two mtimes are compared for
// change-detection. The filesystem reports mtimes at nanosecond precision, but
// Postgres timestamptz (where the manifest mtime round-trips) stores only
// MICROSECOND precision — so a stat mtime of …252204219 becomes …252204000
// after a manifest write+read. Comparing the raw values with exact equality
// then flags an UNCHANGED file as changed on the next poll, producing a spurious
// conflict after every write. Truncating both sides to the limiting (Postgres)
// resolution makes the comparison precision-agnostic. Microsecond is the right
// granularity: it matches the store's actual precision and is still fine enough
// to detect a legitimate rapid change (second-granularity would be too coarse).
const mtimeResolution = time.Microsecond

// mtimeEqual reports whether two mtimes are equal at the comparison resolution
// (microsecond). Both are normalized to UTC and truncated to mtimeResolution
// before comparing so that sub-microsecond nanoseconds — which Postgres
// timestamptz cannot store — do not register as a change. This is harmless for
// the memory-store path (full-precision values truncate identically on both
// sides) and fixes the Postgres path.
func mtimeEqual(a, b time.Time) bool {
	return a.UTC().Truncate(mtimeResolution).Equal(b.UTC().Truncate(mtimeResolution))
}

// statDiffers reports whether the current stat (cur, present) differs from the
// manifest entry me. It is the TIER-1 fast gate of the hash-backed detection:
// when it returns false the member is unchanged and the (expensive) content hash
// is never computed, keeping a noop poll hash-free. When it returns true, the
// caller computes the content Hash and consults it (tier 2) to tell a real
// content change from a mere touch (mtime moved, bytes identical). Rules:
//   - present on disk but no manifest row, or manifest has no mtime/size => differs
//     (a newly-appeared file).
//   - absent on disk but the manifest recorded it as present => differs (deleted).
//   - both absent => same.
//   - present on both => differs iff mtime (at microsecond resolution) or size
//     differs.
//
// The mtime comparison uses mtimeEqual rather than time.Equal: a file's stat
// mtime carries nanosecond precision the manifest's Postgres-backed timestamptz
// cannot store, so exact equality would spuriously gate an unchanged file as
// differing after every manifest round-trip. Size stays an exact integer compare
// (no precision issue).
func statDiffers(me store.ManifestEntry, cur reach.FileMeta, present bool) bool {
	hadFile := me.Mtime != nil && me.Size != nil
	if !present {
		return hadFile // disappeared
	}
	if !hadFile {
		return true // appeared
	}
	return !mtimeEqual(*me.Mtime, cur.Mtime) || *me.Size != cur.Size
}

func byID(scoped []scopedNode, id string) (scopedNode, bool) {
	for _, sn := range scoped {
		if sn.node.ID == id {
			return sn, true
		}
	}
	return scopedNode{}, false
}

func tptr(t time.Time) *time.Time { return &t }
