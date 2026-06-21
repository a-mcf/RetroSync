// Package engine is the heart of RetroSync's play-sync: it activates a game
// for a play session, polls an active binding to fan-out changes from the
// primary to its peers, detects (and flags, but does not resolve) conflicts,
// and deactivates with a final sync pass. It implements docs/state-machine.md.
//
// The engine is pure logic over two ports: a store.Store for persistence and a
// reach.Reach per node for filesystem access. It owns no goroutines, no timers,
// and no real I/O. The background ticker that calls Poll on a schedule, plus
// the HTTP/API surface, live in later slices.
//
// TODO(slice-daemon): the poll-loop ticker that calls Poll(ctx, gameID) every
// N seconds for each active binding.
// TODO(slice-api): the HTTP handlers that drive Activate/Poll/Deactivate.
// TODO(slice-conflict-ui): the conflict modal and the
// POST /api/games/{id}/resolve-conflict handler that drives ResolveConflict /
// NodeStates, plus the dashboard wiring that surfaces a conflicted binding.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/store"
)

// ResolveReach maps a node to the Reach adapter that talks to it. Injecting
// this keeps the engine adapter-agnostic: tests supply fakes; production wires
// the real syncthing-share / ssh adapters.
type ResolveReach func(node store.Node) (reach.Reach, error)

// Clock returns the current time. Injected so tests get deterministic
// timestamps (started_at, last_synced, conflict_at, log ts).
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
	// ErrNoSave is returned by Activate when no in-scope node holds the file:
	// there is nothing to start a session from (docs/state-machine.md step 3).
	ErrNoSave = errors.New("engine: no save to start from")
	// ErrSourceMissing is returned when the direction names a source node whose
	// file is absent (e.g. from-peer-<id> but that peer has no save).
	ErrSourceMissing = errors.New("engine: chosen source node has no save")
	// ErrNoPath is returned when a referenced node has no game_paths row for the
	// game (e.g. the direction names a node not configured for this game).
	ErrNoPath = errors.New("engine: node has no path for game")
	// ErrNotConflicted is returned by ResolveConflict when the binding is not in
	// conflict (conflict_at == nil): there is nothing to resolve, and forcing a
	// fan-out would silently overwrite peers the human never reviewed.
	ErrNotConflicted = errors.New("engine: binding is not in conflict")
)

// scopedNode pairs an in-scope node with its game_paths.path and resolved Reach.
type scopedNode struct {
	node store.Node
	path string
	r    reach.Reach
}

// Activate starts a play session for gameID with primaryNode holding play
// authority. direction selects the first-sync source ("from-primary" or
// "from-peer-<nodeID>"); peerScope is "all-configured" or a csv of node ids.
//
// It determines the in-scope nodes (those with a game_paths row for the game,
// filtered by peerScope), stats them, resolves the source per direction, creates
// the active_bindings row, and fans the source file out to every other in-scope
// node — updating the manifest and appending a sync_log row per directional
// copy.
//
// force controls the already-active case (docs/state-machine.md step 1):
//   - force=false: activating a game that already has a binding is rejected with
//     store.ErrConflict ("currently bound to <other>; force takeover?").
//   - force=true: the existing binding row is deleted (a raw replace — NOT a
//     Deactivate-with-final-sync) and the new session proceeds with the normal
//     activate + fan-out. The takeover's chosen source (per direction) is what
//     gets fanned out, so the taker's "use my save" choice wins.
func (e *Engine) Activate(ctx context.Context, gameID, primaryNode, direction, peerScope string, force bool) error {
	if !store.ValidDirection(direction) {
		return fmt.Errorf("engine: invalid direction %q: %w", direction, store.ErrInvalidValue)
	}

	// All read-only validation runs FIRST, before we touch any existing binding.
	// A force-takeover that turns out to be non-viable (no save in scope, chosen
	// source missing, etc.) must return its error WITHOUT having displaced the
	// existing session (the displaced row would otherwise be lost for nothing).
	scoped, err := e.inScopeNodes(ctx, gameID, peerScope)
	if err != nil {
		return err
	}
	if len(scoped) == 0 {
		return fmt.Errorf("engine: no in-scope nodes for game %q: %w", gameID, ErrNoSave)
	}

	// Stat every in-scope node. Absent files are fine (a node may not yet hold a
	// save); any other stat error is fatal to activation.
	metas := make(map[string]reach.FileMeta, len(scoped))
	present := make(map[string]bool, len(scoped))
	for _, sn := range scoped {
		fm, err := sn.r.Stat(ctx, sn.path)
		if err != nil {
			if errors.Is(err, reach.ErrNotExist) {
				continue
			}
			return fmt.Errorf("engine: stat %s: %w", sn.node.ID, err)
		}
		metas[sn.node.ID] = fm
		present[sn.node.ID] = true
	}
	if len(present) == 0 {
		return fmt.Errorf("engine: game %q: %w", gameID, ErrNoSave)
	}

	// Resolve the source node from direction.
	sourceID, err := sourceNodeID(direction, primaryNode)
	if err != nil {
		return err
	}
	src, ok := byID(scoped, sourceID)
	if !ok {
		return fmt.Errorf("engine: source %q for game %q: %w", sourceID, gameID, ErrNoPath)
	}
	if !present[sourceID] {
		return fmt.Errorf("engine: source %q: %w", sourceID, ErrSourceMissing)
	}

	// The new activation is now known viable. ONLY now, for a force-takeover, do
	// we raw-delete the existing binding so CreateBinding below does not hit
	// ErrConflict. This is intentionally NOT a Deactivate (no final sync of the
	// displaced session's primary): the human sitting down with the taking node
	// has just chosen the source they want, and a final sync of the old primary
	// could overwrite that choice (docs/state-machine.md step 1: "Force = delete
	// the existing row, create new").
	if force {
		if _, err := e.store.GetBinding(ctx, gameID); err == nil {
			if err := e.store.DeleteBinding(ctx, gameID); err != nil {
				return fmt.Errorf("engine: force-takeover delete binding: %w", err)
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("engine: force-takeover get binding: %w", err)
		}
	}

	now := e.clock()
	binding := store.ActiveBinding{
		GameID:      gameID,
		PrimaryNode: primaryNode,
		StartedAt:   now,
		Direction:   direction,
		PeerScope:   peerScope,
	}
	// CreateBinding rejects a duplicate game_id via the PK (store.ErrConflict),
	// which surfaces "currently bound to ... force takeover?" at the UI layer.
	if err := e.store.CreateBinding(ctx, binding); err != nil {
		return fmt.Errorf("engine: create binding: %w", err)
	}

	// From here on the active_bindings row exists. If the first-sync fan-out (or
	// the manifest/last_synced bookkeeping) fails, the row would persist with a
	// nil last_synced and partial manifests; a retry would then hit ErrConflict
	// and the operator would be stuck on a half-active game. Unlike Poll (which
	// self-heals on the next pass), Activate has no later pass to recover, so we
	// best-effort roll back to idle on ANY error and let the operator retry.
	if err := e.activateFanOut(ctx, gameID, src, scoped, metas[sourceID], binding, now); err != nil {
		if delErr := e.store.DeleteBinding(ctx, gameID); delErr != nil {
			// Rollback is best-effort: the original error is what the caller acts
			// on. We do not have a logger wired into the engine yet, so wrap the
			// rollback failure into the returned error for visibility.
			return fmt.Errorf("%w (rollback also failed: %v)", err, delErr)
		}
		return err
	}
	return nil
}

// activateFanOut performs the first-sync fan-out plus the source-manifest and
// last_synced bookkeeping. Split out so Activate can wrap any failure in a
// best-effort rollback (see the call site).
func (e *Engine) activateFanOut(ctx context.Context, gameID string, src scopedNode, scoped []scopedNode, srcMeta reach.FileMeta, binding store.ActiveBinding, now time.Time) error {
	// Initial fan-out from the chosen source to every other in-scope node.
	if err := e.fanOut(ctx, gameID, src, scoped, srcMeta, now); err != nil {
		return err
	}
	// Record the source's own manifest state (it is the authority for this pass).
	if err := e.setManifest(ctx, gameID, src.node.ID, srcMeta, now); err != nil {
		return err
	}
	return e.markSynced(ctx, binding, now)
}

// Poll runs ONE pass for the game's active binding. If the binding is already
// in conflict it does nothing (paused, awaiting human resolution). Otherwise it
// stats the primary plus every in-scope peer, compares each against the
// manifest, and applies the docs/state-machine.md case table:
//
//	primary changed | any peer changed | action
//	no              | no               | noop
//	yes             | no               | fan-out primary -> peers; update manifest
//	no              | one+ peer        | conflict (flag, stop)
//	yes             | yes              | conflict (flag, stop)
//
// The manifest and last_synced advance only after a successful write
// (crash-safety: the manifest trails the actual write).
func (e *Engine) Poll(ctx context.Context, gameID string) error {
	binding, err := e.store.GetBinding(ctx, gameID)
	if err != nil {
		return fmt.Errorf("engine: get binding: %w", err)
	}
	if binding.ConflictAt != nil {
		// Paused: a conflicted binding is skipped until a human resolves it.
		return nil
	}
	return e.syncPass(ctx, binding, false /* finalPass */)
}

// Deactivate ends the play session: it runs one final sync pass (fan-out
// primary -> peers if the primary changed). If that final pass WOULD be a
// conflict, the binding is left active and the conflict is surfaced (the row is
// NOT deleted). Otherwise the active_bindings row is deleted. Idempotent: if the
// game is already idle, it returns nil.
func (e *Engine) Deactivate(ctx context.Context, gameID string) error {
	binding, err := e.store.GetBinding(ctx, gameID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // already idle
		}
		return fmt.Errorf("engine: get binding: %w", err)
	}
	// If the binding is already conflicted, the final pass cannot clean it up;
	// leave it active and flagged for human resolution.
	if binding.ConflictAt != nil {
		return nil
	}
	if err := e.syncPass(ctx, binding, true /* finalPass */); err != nil {
		return err
	}
	// Re-read: syncPass may have flagged a conflict. If so, leave the binding.
	binding, err = e.store.GetBinding(ctx, gameID)
	if err != nil {
		return fmt.Errorf("engine: re-read binding: %w", err)
	}
	if binding.ConflictAt != nil {
		return nil // conflict on final pass: leave active + flagged
	}
	if err := e.store.DeleteBinding(ctx, gameID); err != nil {
		return fmt.Errorf("engine: delete binding: %w", err)
	}
	return nil
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

// NodeStates returns the live per-node state of every in-scope node that has a
// game_paths row for gameID, sorted by NodeID for determinism. It is read-only:
// no manifest, binding, or filesystem mutation. A node whose file is absent is
// returned with Present=false; any Stat error other than reach.ErrNotExist is a
// real error.
//
// The in-scope set is filtered by the binding's peer_scope when a binding
// exists; for an idle game (no binding) every configured node is in scope so the
// pre-bind stale-peer view (docs/state-machine.md "Stale-peer visibility") can
// render too.
func (e *Engine) NodeStates(ctx context.Context, gameID string) ([]NodeState, error) {
	peerScope := "all-configured"
	if b, err := e.store.GetBinding(ctx, gameID); err == nil {
		peerScope = b.PeerScope
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("engine: get binding: %w", err)
	}

	scoped, err := e.inScopeNodes(ctx, gameID, peerScope)
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

// ResolveConflict resolves a flagged conflict by making winnerNodeID's current
// save the authority and fanning it out to every other in-scope node, after
// first preserving each loser's existing file as a sibling backup. It clears the
// conflict only on full success (docs/state-machine.md "Conflict handling").
//
// Preconditions:
//   - The binding MUST be in conflict (conflict_at != nil); otherwise
//     ErrNotConflicted (resolving a non-conflicted game is not allowed — it
//     would overwrite peers the human never reviewed).
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
func (e *Engine) ResolveConflict(ctx context.Context, gameID, winnerNodeID string) error {
	binding, err := e.store.GetBinding(ctx, gameID)
	if err != nil {
		return fmt.Errorf("engine: get binding: %w", err)
	}
	if binding.ConflictAt == nil {
		return fmt.Errorf("engine: game %q: %w", gameID, ErrNotConflicted)
	}

	scoped, err := e.inScopeNodes(ctx, gameID, binding.PeerScope)
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
	if err := e.backupLosers(ctx, gameID, scoped, winner, now); err != nil {
		return err
	}

	// Step 2: fan out the winner to every other in-scope node, advancing the
	// manifest only after each successful write. A partial fan-out returns the
	// error and (because we have NOT cleared conflict_at) leaves the game
	// conflicted for re-resolution.
	if err := e.fanOut(ctx, gameID, winner, scoped, winnerMeta, now); err != nil {
		return err
	}
	// Record the winner's own manifest state (it is the authority for this pass).
	if err := e.setManifest(ctx, gameID, winner.node.ID, winnerMeta, now); err != nil {
		return err
	}

	// Step 3: only now, after every write succeeded, clear the conflict and mark
	// the binding synced.
	binding.ConflictAt = nil
	binding.LastSynced = &now
	if err := e.store.UpdateBinding(ctx, binding); err != nil {
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
func (e *Engine) backupLosers(ctx context.Context, gameID string, scoped []scopedNode, winner scopedNode, now time.Time) error {
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
			GameID:   gameID,
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

// syncPass implements the case table shared by Poll and Deactivate's final
// pass. finalPass only affects logging context; the conflict/fan-out logic is
// identical (the spec's "final sync pass ... but if it does [conflict], leave
// the binding active and surface the conflict").
func (e *Engine) syncPass(ctx context.Context, binding store.ActiveBinding, finalPass bool) error {
	scoped, err := e.inScopeNodes(ctx, binding.GameID, binding.PeerScope)
	if err != nil {
		return err
	}
	primary, ok := byID(scoped, binding.PrimaryNode)
	if !ok {
		return fmt.Errorf("engine: primary %q: %w", binding.PrimaryNode, ErrNoPath)
	}

	manifest, err := e.manifestMap(ctx, binding.GameID)
	if err != nil {
		return err
	}

	// Determine changed-ness per node.
	primaryMeta, primaryPresent, err := e.statOpt(ctx, primary)
	if err != nil {
		return err
	}
	primaryChanged := changed(manifest[primary.node.ID], primaryMeta, primaryPresent)

	peerChanged := false
	for _, sn := range scoped {
		if sn.node.ID == primary.node.ID {
			continue
		}
		meta, present, err := e.statOpt(ctx, sn)
		if err != nil {
			// Known limitation: peers are scanned in node-id order, and any
			// non-ErrNotExist Stat error aborts the whole pass. If peer A mutated
			// out-of-band but peer B is hard-down and sorts first, B's error
			// returns here before A's change is flagged as a conflict — delaying
			// conflict detection until B is reachable again. For a transient fault
			// this is fine (the next poll retries and flags it); sustained
			// unreachability could mask the conflict for as long as B stays down.
			// TODO(slice-daemon): when the timer loop lands, revisit so a single
			// unreachable peer degrades to "scan the rest, surface the conflict"
			// rather than aborting the pass.
			return err
		}
		if changed(manifest[sn.node.ID], meta, present) {
			peerChanged = true
		}
	}

	now := e.clock()

	switch {
	case peerChanged:
		// no/yes primary + any-peer-changed => conflict. A peer wrote
		// out-of-band; pause rather than overwrite it.
		return e.flagConflict(ctx, binding, now, finalPass)
	case primaryChanged:
		// yes/no => fan-out primary -> peers.
		if !primaryPresent {
			// Primary's manifest says it existed but it's now gone: treat as a
			// conflict rather than deleting peers. Surfacing beats destruction.
			return e.flagConflict(ctx, binding, now, finalPass)
		}
		if err := e.fanOut(ctx, binding.GameID, primary, scoped, primaryMeta, now); err != nil {
			return err
		}
		if err := e.setManifest(ctx, binding.GameID, primary.node.ID, primaryMeta, now); err != nil {
			return err
		}
		return e.markSynced(ctx, binding, now)
	default:
		// no/no => noop.
		return nil
	}
}

// fanOut reads the source file once and WriteAtomic's it to every other
// in-scope node, advancing each written node's manifest and appending a
// sync_log "ok" row per copy. The destination mtime is the source's mtime so
// the next poll sees source == peer.
func (e *Engine) fanOut(ctx context.Context, gameID string, src scopedNode, scoped []scopedNode, srcMeta reach.FileMeta, now time.Time) error {
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
				GameID:   gameID,
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
		if err := e.setManifest(ctx, gameID, dst.node.ID, written, now); err != nil {
			return err
		}
		bytes := srcMeta.Size
		if err := e.appendLog(ctx, store.LogEntry{
			GameID:   gameID,
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

// flagConflict sets conflict_at on the binding and appends a single conflict
// log row, then stops. Syncing is paused until a human resolves it. finalPass
// marks the conflict as having surfaced during deactivation's final pass, so it
// is distinguishable from a poll-time conflict in sync_log.
func (e *Engine) flagConflict(ctx context.Context, binding store.ActiveBinding, now time.Time, finalPass bool) error {
	binding.ConflictAt = &now
	if err := e.store.UpdateBinding(ctx, binding); err != nil {
		return fmt.Errorf("engine: flag conflict: %w", err)
	}
	msg := "non-primary peer mutated or primary+peer diverged; sync paused"
	if finalPass {
		msg = "conflict on final (deactivation) pass: " + msg + "; binding left active"
	}
	return e.appendLog(ctx, store.LogEntry{
		GameID:   binding.GameID,
		FromNode: binding.PrimaryNode,
		Outcome:  store.OutcomeConflict,
		Message:  msg,
		TS:       now,
	})
}

// markSynced advances last_synced on the binding after a successful pass.
func (e *Engine) markSynced(ctx context.Context, binding store.ActiveBinding, now time.Time) error {
	binding.LastSynced = &now
	if err := e.store.UpdateBinding(ctx, binding); err != nil {
		return fmt.Errorf("engine: update last_synced: %w", err)
	}
	return nil
}

// --- helpers -------------------------------------------------------------

// inScopeNodes returns the nodes that have a game_paths row for gameID, filtered
// by peerScope, each paired with its path and a resolved Reach. Results are
// sorted by node id for determinism.
func (e *Engine) inScopeNodes(ctx context.Context, gameID, peerScope string) ([]scopedNode, error) {
	paths, err := e.store.ListGamePathsByGame(ctx, gameID)
	if err != nil {
		return nil, fmt.Errorf("engine: list game paths: %w", err)
	}
	scopeSet, scopeAll := parseScope(peerScope)

	out := make([]scopedNode, 0, len(paths))
	for _, gp := range paths {
		if !scopeAll && !scopeSet[gp.NodeID] {
			continue
		}
		node, err := e.store.GetNode(ctx, gp.NodeID)
		if err != nil {
			return nil, fmt.Errorf("engine: get node %s: %w", gp.NodeID, err)
		}
		r, err := e.resolve(node)
		if err != nil {
			return nil, fmt.Errorf("engine: resolve reach for %s: %w", node.ID, err)
		}
		out = append(out, scopedNode{node: node, path: gp.Path, r: r})
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

func (e *Engine) manifestMap(ctx context.Context, gameID string) (map[string]store.ManifestEntry, error) {
	entries, err := e.store.ListManifestByGame(ctx, gameID)
	if err != nil {
		return nil, fmt.Errorf("engine: list manifest: %w", err)
	}
	m := make(map[string]store.ManifestEntry, len(entries))
	for _, me := range entries {
		m[me.NodeID] = me
	}
	return m, nil
}

func (e *Engine) setManifest(ctx context.Context, gameID, nodeID string, meta reach.FileMeta, now time.Time) error {
	size := meta.Size
	mtime := meta.Mtime
	m := store.ManifestEntry{
		GameID:      gameID,
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

// changed reports whether the current stat (cur, present) differs from the
// manifest entry me. Rules:
//   - present on disk but no manifest row, or manifest has no mtime/size => changed
//     (a newly-appeared file).
//   - absent on disk but the manifest recorded it as present => changed (deleted).
//   - both absent => not changed.
//   - present on both => changed iff mtime or size differs.
func changed(me store.ManifestEntry, cur reach.FileMeta, present bool) bool {
	hadFile := me.Mtime != nil && me.Size != nil
	if !present {
		return hadFile // disappeared
	}
	if !hadFile {
		return true // appeared
	}
	return !me.Mtime.Equal(cur.Mtime) || *me.Size != cur.Size
}

// sourceNodeID resolves the first-sync source from a direction. "from-primary"
// yields primaryNode; "from-peer-<id>" yields <id>.
func sourceNodeID(direction, primaryNode string) (string, error) {
	if direction == "from-primary" {
		return primaryNode, nil
	}
	if id := strings.TrimPrefix(direction, "from-peer-"); id != direction && id != "" {
		return id, nil
	}
	return "", fmt.Errorf("engine: invalid direction %q: %w", direction, store.ErrInvalidValue)
}

// parseScope interprets peer_scope. "all-configured" (or empty) => match all.
// Otherwise a csv of node ids.
func parseScope(peerScope string) (set map[string]bool, all bool) {
	if peerScope == "" || peerScope == "all-configured" {
		return nil, true
	}
	set = make(map[string]bool)
	for _, id := range strings.Split(peerScope, ",") {
		id = strings.TrimSpace(id)
		if id != "" {
			set[id] = true
		}
	}
	return set, false
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
