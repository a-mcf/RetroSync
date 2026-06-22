// Package engine is the heart of RetroSync's play-sync: it AUTO-MIRRORS every
// sync. Each Poll stats a sync's members, finds which member changed vs the
// manifest, and — when exactly one changed — fans that member's save out to the
// others. Zero changed is a noop; two or more changed is a genuine fork, which
// it flags as a conflict and pauses (mirroring stays paused until a human
// resolves it). There is no primary, no binding, no activate/deactivate/
// take-over: the only human action is resolving a conflict. It implements
// docs/state-machine.md.
//
// A *sync* is the unit of mirroring: a specific set of (node, save-file)
// members that sync together (one game may have many independent syncs). The
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

// Poll runs ONE auto-mirror pass for syncID. It loads the sync; if the sync is
// already in conflict (conflict_at != nil) it does nothing (paused, awaiting
// human resolution). Otherwise it stats every member, computes the CHANGED SET
// (each member's current stat vs its manifest, via changed()), and applies:
//
//	members changed | action
//	0               | noop
//	1               | fan-out that member -> the others; update manifest; mark synced
//	2+              | conflict (set conflict_at, log, stop — paused until resolved)
//
// There is no primary: any single changed member is the source. The manifest
// and last_synced advance only after a successful write (crash-safety: the
// manifest trails the actual write). A sync with fewer than two members can
// never fork, so it simply propagates (1 changed) or no-ops (0 changed).
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

	// Stat every member and collect those that changed vs the manifest. Each
	// member is stat'd once; a non-ErrNotExist Stat error aborts the pass (the
	// next poll re-stats and resumes).
	metas := make(map[string]reach.FileMeta, len(scoped))
	present := make(map[string]bool, len(scoped))
	var changedSet []scopedNode
	for _, sn := range scoped {
		meta, ok, err := e.statOpt(ctx, sn)
		if err != nil {
			return err
		}
		metas[sn.node.ID] = meta
		present[sn.node.ID] = ok
		if changed(manifest[sn.node.ID], meta, ok) {
			changedSet = append(changedSet, sn)
		}
	}

	now := e.clock()

	switch len(changedSet) {
	case 0:
		// Nothing changed: noop.
		return nil
	case 1:
		// Exactly one member changed — it is the source. If it is now ABSENT but
		// the manifest had it (a deletion), treat as a conflict rather than
		// propagating the delete to the others: surfacing beats destruction.
		src := changedSet[0]
		if !present[src.node.ID] {
			return e.flagConflict(ctx, syncID, now,
				fmt.Sprintf("member %s vanished (manifest had a file); sync paused", src.node.ID))
		}
		if err := e.fanOut(ctx, syncID, src, scoped, metas[src.node.ID], now); err != nil {
			return err
		}
		// Record the source's own manifest state (it is the authority this pass).
		if err := e.setManifest(ctx, syncID, src.node.ID, metas[src.node.ID], now); err != nil {
			return err
		}
		return e.markSynced(ctx, syncID, now)
	default:
		// Two or more members changed — a genuine fork. Flag and pause.
		ids := make([]string, 0, len(changedSet))
		for _, sn := range changedSet {
			ids = append(ids, sn.node.ID)
		}
		return e.flagConflict(ctx, syncID, now,
			fmt.Sprintf("%d members changed (%s); retrosync won't choose — sync paused",
				len(changedSet), strings.Join(ids, ", ")))
	}
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
// SmokeTest is read-only: it never mutates the store or the node. It does NOT
// touch last_seen_at; updating that on success is the web handler's choice (it
// owns the store) so the engine stays a pure logic-over-ports component.
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
// save the authority and fanning it out to every other in-scope node, after
// first preserving each loser's existing file as a sibling backup. It clears the
// conflict only on full success (docs/state-machine.md "Conflict handling").
//
// Preconditions:
//   - The sync MUST be in conflict (conflict_at != nil); otherwise
//     ErrNotConflicted (resolving a non-conflicted sync is not allowed — it
//     would overwrite members the human never reviewed).
//   - winnerNodeID MUST be in scope (ErrNoPath otherwise) and MUST currently
//     hold a file (ErrSourceMissing otherwise).
//
// Ordering / crash-safety:
//  1. Back up every OTHER in-scope node that currently HAS a file to a sibling
//     <path>.retrosync-conflict-<ts> on that same node, BEFORE any overwrite.
//     The ts is the injected Clock in a filesystem-safe, sub-second form
//     (20060102T150405.000000000Z, UTC, no colons), so two resolves in the same
//     wall-clock second do not collide. A node with no current file gets no
//     backup.
//  2. Fan the winner's bytes+mtime out to every other in-scope node, advancing
//     the manifest only AFTER each successful write (reuses fanOut/setManifest).
//  3. Clear conflict_at and set last_synced.
//
// If any backup or fan-out write fails, conflict_at is left set (the binding
// stays conflicted and re-resolvable) and the error is returned.
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
	// fanOut, and the losers are independently re-stat'd in backupLosers, so a
	// file can change between these stats/reads. Harmless today (no concurrent
	// poll loop drives this path), but when the timer loop lands a file mutated
	// mid-resolution could be backed up or fanned out inconsistently; revisit to
	// snapshot each node once under the concurrency model then in place.
	winnerMeta, winnerPresent, err := e.statOpt(ctx, winner)
	if err != nil {
		return err
	}
	if !winnerPresent {
		return fmt.Errorf("engine: winner %q: %w", winnerNodeID, ErrSourceMissing)
	}

	now := e.clock()

	// Step 1: back up every OTHER in-scope node that currently has a file, BEFORE
	// any overwrite. A backup failure aborts WITHOUT having touched the saves and
	// leaves the conflict set (re-resolvable). No loser file is overwritten until
	// every loser-with-a-file has been backed up.
	if err := e.backupLosers(ctx, syncID, scoped, winner, now); err != nil {
		return err
	}

	// Step 2: fan out the winner to every other in-scope node, advancing the
	// manifest only after each successful write. A partial fan-out returns the
	// error and (because we have NOT cleared conflict_at) leaves the sync
	// conflicted for re-resolution.
	if err := e.fanOut(ctx, syncID, winner, scoped, winnerMeta, now); err != nil {
		return err
	}
	// Record the winner's own manifest state (it is the authority for this pass).
	if err := e.setManifest(ctx, syncID, winner.node.ID, winnerMeta, now); err != nil {
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

// backupLosers writes the CURRENT bytes of every in-scope node other than the
// winner that currently holds a file to a sibling backup path
// <path>.retrosync-conflict-<ts> on that same node, via WriteAtomic, and logs an
// "ok" sync_log row per backup. Nodes with no current file are skipped (nothing
// to preserve). Called before any overwrite so each loser's pre-resolution save
// survives. A backup write or read failure is returned (and aborts resolution).
func (e *Engine) backupLosers(ctx context.Context, syncID string, scoped []scopedNode, winner scopedNode, now time.Time) error {
	suffix := backupSuffix(now)
	for _, sn := range scoped {
		if sn.node.ID == winner.node.ID {
			continue
		}
		meta, present, err := e.statOpt(ctx, sn)
		if err != nil {
			return err
		}
		if !present {
			// No current file on this node: nothing to back up. It still RECEIVES
			// the winner's file during the fan-out below.
			continue
		}
		data, err := sn.r.Read(ctx, sn.path)
		if err != nil {
			return fmt.Errorf("engine: read loser %s for backup: %w", sn.node.ID, err)
		}
		backupPath := sn.path + suffix
		// Preserve the loser's existing mtime on its backup (faithful snapshot).
		if err := sn.r.WriteAtomic(ctx, backupPath, data, meta.Mtime); err != nil {
			return fmt.Errorf("engine: backup %s: %w", sn.node.ID, err)
		}
		bytes := meta.Size
		if err := e.appendLog(ctx, store.LogEntry{
			SyncID:   syncID,
			FromNode: sn.node.ID,
			ToNode:   sn.node.ID,
			Bytes:    &bytes,
			SrcMtime: tptr(meta.Mtime),
			Outcome:  store.OutcomeOK,
			Message:  "conflict-resolve backup: " + backupPath,
			TS:       now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// backupSuffix builds the sibling-backup suffix for a conflict resolution at t.
// The timestamp is filesystem-safe: UTC, the Go reference layout
// 20060102T150405.000000000Z (basic-ISO-8601 with a Z zone and nanosecond
// fraction), which contains NO colons, slashes, or spaces — only digits, T, Z,
// and dots — so it is a legal filename component on every target filesystem and
// stays a sibling of <path> (no new directory separators).
//
// The nanosecond fraction is what makes the suffix collision-resistant: two
// ResolveConflict calls in the SAME wall-clock second (a double-click, or a
// quick retry after a partial fan-out) would otherwise compute the IDENTICAL
// backup path and the second WriteAtomic would clobber the first loser's only
// preserved snapshot. With sub-second precision, two distinct clock() values
// yield two distinct backup paths, so the insurance survives.
func backupSuffix(t time.Time) string {
	return ".retrosync-conflict-" + t.UTC().Format("20060102T150405.000000000Z")
}

// fanOut reads the source file once and WriteAtomic's it to every other
// in-scope node, advancing each written node's manifest and appending a
// sync_log "ok" row per copy. The destination mtime is the source's mtime so
// the next poll sees source == peer.
func (e *Engine) fanOut(ctx context.Context, syncID string, src scopedNode, scoped []scopedNode, srcMeta reach.FileMeta, now time.Time) error {
	data, err := src.r.Read(ctx, src.path)
	if err != nil {
		return fmt.Errorf("engine: read source %s: %w", src.node.ID, err)
	}
	for _, dst := range scoped {
		if dst.node.ID == src.node.ID {
			continue
		}
		// dst mtime *before* overwrite, for the log (nil if dst absent or
		// unreadable; this is a best-effort diagnostic, not load-bearing).
		var dstMtimeBefore *time.Time
		if fm, present, err := e.statOpt(ctx, dst); err == nil && present {
			dstMtimeBefore = &fm.Mtime
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
		// source mtime, size == source size).
		written := reach.FileMeta{Mtime: srcMeta.Mtime, Size: srcMeta.Size}
		if err := e.setManifest(ctx, syncID, dst.node.ID, written, now); err != nil {
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

func (e *Engine) setManifest(ctx context.Context, syncID, nodeID string, meta reach.FileMeta, now time.Time) error {
	size := meta.Size
	// Truncate to the comparison resolution before storing so the manifest value
	// matches what a later changed() compare will use, regardless of where it is
	// persisted. This is belt-and-suspenders: changed() truncates both sides too,
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

// changed reports whether the current stat (cur, present) differs from the
// manifest entry me. Rules:
//   - present on disk but no manifest row, or manifest has no mtime/size => changed
//     (a newly-appeared file).
//   - absent on disk but the manifest recorded it as present => changed (deleted).
//   - both absent => not changed.
//   - present on both => changed iff mtime (at microsecond resolution) or size
//     differs.
//
// The mtime comparison uses mtimeEqual rather than time.Equal: a file's stat
// mtime carries nanosecond precision the manifest's Postgres-backed timestamptz
// cannot store, so exact equality would spuriously flag an unchanged file as
// changed after every manifest round-trip. Size stays an exact integer compare
// (no precision issue).
func changed(me store.ManifestEntry, cur reach.FileMeta, present bool) bool {
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
