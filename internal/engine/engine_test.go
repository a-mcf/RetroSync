package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/fakereach"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

func ctx() context.Context { return context.Background() }

// These tests exercise the AUTO-MIRROR engine: Poll computes the changed set of
// a sync's members vs the manifest and propagates (1 changed), no-ops (0), or
// flags a conflict (2+). There is no Activate/Deactivate/primary/take-over — the
// only human action is ResolveConflict. The data-loss safety properties
// (backup-before-overwrite, conflict-halts, manifest-trails-write, µs compare)
// are asserted throughout.

// --- test harness --------------------------------------------------------

const (
	gameID = "super-metroid"
	syncID = "sm-bob"
)

// steppingClock returns a Clock that advances by a fixed step on each call, so
// timestamps are deterministic but distinguishable.
func steppingClock(start time.Time, step time.Duration) engine.Clock {
	cur := start
	return func() time.Time {
		t := cur
		cur = cur.Add(step)
		return t
	}
}

// harness wires a memory Store + a fakereach.Fake per node + an Engine.
type harness struct {
	t      *testing.T
	store  store.Store
	fakes  map[string]*fakereach.Fake
	paths  map[string]string // nodeID -> member path
	engine *engine.Engine
}

func newHarness(t *testing.T, clock engine.Clock) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		store: memory.New(),
		fakes: map[string]*fakereach.Fake{},
		paths: map[string]string{},
	}
	resolve := func(n store.Node) (reach.Reach, error) {
		f, ok := h.fakes[n.ID]
		if !ok {
			t.Fatalf("no fake for node %q", n.ID)
		}
		return f, nil
	}
	h.engine = engine.New(h.store, resolve, clock)
	if err := h.store.CreateGame(ctx(), store.Game{ID: gameID, Display: "Super Metroid", System: "snes"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateSync(ctx(), store.Sync{ID: syncID, GameID: gameID, Name: "Bob's stream"}); err != nil {
		t.Fatal(err)
	}
	return h
}

// addNode registers a node + a sync member (the engine's in-scope (node, path))
// + a backing fake. If present, seeds the node's file with content at mtime.
func (h *harness) addNode(id, path string, content []byte, mtime time.Time, present bool) {
	h.t.Helper()
	if err := h.store.CreateNode(ctx(), store.Node{ID: id, Display: id, Kind: store.KindGeneric, Reach: store.ReachSyncthingShare}); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.SetSyncMember(ctx(), store.SyncMember{SyncID: syncID, NodeID: id, Path: path}); err != nil {
		h.t.Fatal(err)
	}
	f := fakereach.New()
	if present {
		f.Put(path, content, mtime)
	}
	h.fakes[id] = f
	h.paths[id] = path
}

// seedManifest writes a manifest entry for a node at the given mtime/size, so a
// subsequent Poll sees that node as UNCHANGED unless its live file moved. This
// is how a test establishes a "clean, in-sync" starting state directly (there is
// no Activate to bootstrap it anymore).
func (h *harness) seedManifest(nodeID string, mtime time.Time, size int64) {
	h.t.Helper()
	mt := mtime.UTC()
	sz := size
	checked := mtime.UTC()
	if err := h.store.SetManifest(ctx(), store.ManifestEntry{
		SyncID: syncID, NodeID: nodeID, Mtime: &mt, Size: &sz, LastChecked: &checked,
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) fake(id string) *fakereach.Fake { return h.fakes[id] }

func (h *harness) sync() store.Sync {
	h.t.Helper()
	sy, err := h.store.GetSync(ctx(), syncID)
	if err != nil {
		h.t.Fatalf("get sync: %v", err)
	}
	return sy
}

func (h *harness) manifest(nodeID string) store.ManifestEntry {
	h.t.Helper()
	m, err := h.store.GetManifest(ctx(), syncID, nodeID)
	if err != nil {
		h.t.Fatalf("get manifest %s: %v", nodeID, err)
	}
	return m
}

func (h *harness) logCount(outcome store.Outcome) int {
	h.t.Helper()
	entries, err := h.store.ListLogBySync(ctx(), syncID, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Outcome == outcome {
			n++
		}
	}
	return n
}

// assertFile asserts a node's current file content + mtime.
func (h *harness) assertFile(nodeID string, content []byte, mtime time.Time) {
	h.t.Helper()
	fm, err := h.fake(nodeID).Stat(ctx(), h.paths[nodeID])
	if err != nil {
		h.t.Fatalf("stat %s: %v", nodeID, err)
	}
	if !fm.Mtime.Equal(mtime) {
		h.t.Fatalf("%s mtime = %v want %v", nodeID, fm.Mtime, mtime)
	}
	data, err := h.fake(nodeID).Read(ctx(), h.paths[nodeID])
	if err != nil {
		h.t.Fatalf("read %s: %v", nodeID, err)
	}
	if string(data) != string(content) {
		h.t.Fatalf("%s content = %q want %q", nodeID, data, content)
	}
}

var t0 = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

// --- seeding helpers -----------------------------------------------------

// seedSynced builds a two-member sync (primary + peer) that is fully in sync:
// both hold "V1"@srcMtime and both manifests record that state. A Poll right now
// is a noop. Returns the harness and the in-sync mtime.
func seedSynced(t *testing.T) (*harness, time.Time) {
	t.Helper()
	h := newHarness(t, steppingClock(t0, time.Second))
	srcMtime := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("V1"), srcMtime, true)
	h.addNode("peer", "q.srm", []byte("V1"), srcMtime, true)
	h.seedManifest("primary", srcMtime, int64(len("V1")))
	h.seedManifest("peer", srcMtime, int64(len("V1")))
	return h, srcMtime
}

// seedSyncedMulti is seedSynced with two peers (primary, peer-a, peer-b), all in
// sync at srcMtime.
func seedSyncedMulti(t *testing.T) (*harness, time.Time) {
	t.Helper()
	h := newHarness(t, steppingClock(t0, time.Second))
	srcMtime := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("V1"), srcMtime, true)
	h.addNode("peer-a", "a.srm", []byte("V1"), srcMtime, true)
	h.addNode("peer-b", "b.srm", []byte("V1"), srcMtime, true)
	h.seedManifest("primary", srcMtime, int64(len("V1")))
	h.seedManifest("peer-a", srcMtime, int64(len("V1")))
	h.seedManifest("peer-b", srcMtime, int64(len("V1")))
	return h, srcMtime
}

// seedConflicted builds a two-member sync that is in conflict: it starts in
// sync, then BOTH members are mutated out of band and a Poll flags the fork.
// Returns the harness, the primary's (winner-candidate) mtime, and the peer's
// (loser) mtime.
//   - primary "p.srm": "PRIMARY" at primaryMtime (the eventual winner content)
//   - peer    "q.srm": "PEER-WROTE" at peerMtime (the loser's divergent content)
func seedConflicted(t *testing.T) (h *harness, primaryMtime, peerMtime time.Time) {
	t.Helper()
	h, base := seedSynced(t)
	primaryMtime = base.Add(time.Hour)
	peerMtime = base.Add(2 * time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("PRIMARY"), primaryMtime)
	h.fake("peer").Mutate("q.srm", []byte("PEER-WROTE"), peerMtime)
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if h.sync().ConflictAt == nil {
		t.Fatal("setup: expected the sync to be conflicted")
	}
	return h, primaryMtime, peerMtime
}

// --- Poll: the changed-set case table ------------------------------------

func TestPoll_NoChange_Noop(t *testing.T) {
	h, srcMtime := seedSynced(t)
	writesBefore := len(h.fake("peer").Writes())

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// 0 changed -> noop: no writes, no conflict, no last_synced advance.
	if got := len(h.fake("peer").Writes()); got != writesBefore {
		t.Fatalf("noop poll wrote: %d -> %d", writesBefore, got)
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("noop poll flagged a conflict")
	}
	if h.sync().LastSynced != nil {
		t.Fatalf("noop poll advanced last_synced")
	}
	h.assertFile("peer", []byte("V1"), srcMtime)
}

func TestPoll_OneChanged_Propagates(t *testing.T) {
	h, _ := seedSynced(t)
	// Exactly one member (primary) changes.
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V2-LONGER"), newMtime)

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// The changed member is the source; the peer now holds its bytes + mtime.
	h.assertFile("peer", []byte("V2-LONGER"), newMtime)
	// Manifest advanced for both to the new state.
	for _, id := range []string{"primary", "peer"} {
		m := h.manifest(id)
		if m.Mtime == nil || !m.Mtime.Equal(newMtime) {
			t.Fatalf("%s manifest mtime = %v want %v", id, m.Mtime, newMtime)
		}
		if m.Size == nil || *m.Size != int64(len("V2-LONGER")) {
			t.Fatalf("%s manifest size = %v want %d", id, m.Size, len("V2-LONGER"))
		}
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("a single change must not conflict")
	}
	if h.sync().LastSynced == nil {
		t.Fatalf("propagate should advance last_synced")
	}
}

func TestPoll_TwoChanged_Conflict(t *testing.T) {
	h, _ := seedSynced(t)
	// Both members change between polls -> a genuine fork.
	h.fake("primary").Mutate("p.srm", []byte("PRIMARY"), t0.Add(time.Hour))
	h.fake("peer").Mutate("q.srm", []byte("PEER"), t0.Add(2*time.Hour))

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if h.sync().ConflictAt == nil {
		t.Fatalf("two changed members must conflict")
	}
	if h.logCount(store.OutcomeConflict) != 1 {
		t.Fatalf("expected exactly one conflict log row, got %d", h.logCount(store.OutcomeConflict))
	}
	// Conflict halts: NEITHER member's file was overwritten.
	h.assertFile("primary", []byte("PRIMARY"), t0.Add(time.Hour))
	h.assertFile("peer", []byte("PEER"), t0.Add(2*time.Hour))
}

// TestPoll_NewSyncTwoMembersWithSaves_Conflict pins the onboarding behavior:
// the FIRST poll of a brand-new sync (empty manifest) where 2+ members already
// hold a save treats every present member as "appeared/changed", so len(changed)
// >= 2 -> conflict. This is intended — surfacing the fork beats silently picking
// a winner during onboarding. Neither file is overwritten; nothing propagates.
func TestPoll_NewSyncTwoMembersWithSaves_Conflict(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	// Two members, each already holding a (distinct) save, NO seeded manifest.
	aMtime := t0.Add(-2 * time.Hour)
	bMtime := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("A-SAVE"), aMtime, true)
	h.addNode("peer", "q.srm", []byte("B-SAVE"), bMtime, true)

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// Both present members count as changed vs the empty manifest -> conflict.
	if h.sync().ConflictAt == nil {
		t.Fatalf("a fresh sync with two present saves must conflict on first poll")
	}
	if h.logCount(store.OutcomeConflict) != 1 {
		t.Fatalf("expected exactly one conflict log row, got %d", h.logCount(store.OutcomeConflict))
	}
	// Conflict halts: NEITHER member's file was overwritten (no winner picked).
	h.assertFile("primary", []byte("A-SAVE"), aMtime)
	h.assertFile("peer", []byte("B-SAVE"), bMtime)
	// No propagation happened and last_synced did not advance.
	if h.sync().LastSynced != nil {
		t.Fatalf("a conflicting first poll must not advance last_synced")
	}
}

func TestPoll_ConflictedSync_Skipped(t *testing.T) {
	h, _, _ := seedConflicted(t)
	conflictAt := h.sync().ConflictAt
	writesBefore := len(h.fake("peer").Writes())

	// A subsequent poll on a conflicted sync is a no-op (paused). Even though both
	// members still differ from the manifest, the engine must NOT re-evaluate.
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll conflicted: %v", err)
	}
	if got := len(h.fake("peer").Writes()); got != writesBefore {
		t.Fatalf("conflicted poll wrote: %d -> %d", writesBefore, got)
	}
	// conflict_at unchanged (not re-flagged with a new timestamp).
	if got := h.sync().ConflictAt; got == nil || !got.Equal(*conflictAt) {
		t.Fatalf("conflicted poll changed conflict_at: %v -> %v", conflictAt, got)
	}
}

func TestPoll_OneMemberVanished_Conflict_OthersUntouched(t *testing.T) {
	h, srcMtime := seedSynced(t)
	// The primary's file disappears (manifest still has it). Treat as a conflict
	// rather than propagating the deletion to the peer.
	h.fake("primary").Remove("p.srm")

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if h.sync().ConflictAt == nil {
		t.Fatalf("a vanished member must conflict, not propagate a delete")
	}
	// The peer's file is untouched (NOT deleted).
	h.assertFile("peer", []byte("V1"), srcMtime)
}

func TestPoll_MultiPeer_OneChanged_FansOutToBoth(t *testing.T) {
	h, _ := seedSyncedMulti(t)
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V2"), newMtime)

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	h.assertFile("peer-a", []byte("V2"), newMtime)
	h.assertFile("peer-b", []byte("V2"), newMtime)
	if h.sync().ConflictAt != nil {
		t.Fatalf("single change across 3 members must not conflict")
	}
}

func TestPoll_MultiPeer_TwoChanged_Conflict_InnocentUntouched(t *testing.T) {
	h, srcMtime := seedSyncedMulti(t)
	// primary and peer-a both change; peer-b is innocent.
	h.fake("primary").Mutate("p.srm", []byte("PRIMARY"), t0.Add(time.Hour))
	h.fake("peer-a").Mutate("a.srm", []byte("PEER-A"), t0.Add(2*time.Hour))

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if h.sync().ConflictAt == nil {
		t.Fatalf("two changed members must conflict")
	}
	// The innocent peer is untouched: conflict halts the whole pass.
	h.assertFile("peer-b", []byte("V1"), srcMtime)
	// And the two changed members keep their divergent files (no overwrite).
	h.assertFile("primary", []byte("PRIMARY"), t0.Add(time.Hour))
	h.assertFile("peer-a", []byte("PEER-A"), t0.Add(2*time.Hour))
}

// --- crash-safety: manifest trails the write ----------------------------

func TestPoll_WriteFails_ManifestNotAdvanced_RetriesNextPoll(t *testing.T) {
	h, srcMtime := seedSynced(t)
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V2"), newMtime)
	// The peer's write fails on this pass.
	h.fake("peer").FailWriteAtomic("", errors.New("disk full"))

	if err := h.engine.Poll(ctx(), syncID); err == nil {
		t.Fatal("poll should return the write error")
	}
	// Manifest for the peer must NOT have advanced (it trails the actual write).
	if m := h.manifest("peer"); m.Mtime == nil || !m.Mtime.Equal(srcMtime) {
		t.Fatalf("peer manifest advanced past a failed write: %v", m.Mtime)
	}
	// last_synced must not have advanced either.
	if h.sync().LastSynced != nil {
		t.Fatalf("last_synced advanced despite a failed fan-out")
	}

	// Clear the failure; the next poll re-detects the divergence and heals.
	h.fake("peer").FailWriteAtomic("", nil)
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("retry poll: %v", err)
	}
	h.assertFile("peer", []byte("V2"), newMtime)
	if h.sync().LastSynced == nil {
		t.Fatalf("retry should advance last_synced")
	}
}

func TestPoll_MultiPeer_PartialFanOut_SelfHeals(t *testing.T) {
	h, srcMtime := seedSyncedMulti(t)
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V2"), newMtime)
	// peer-a sorts before peer-b; fail peer-b so peer-a is written but the pass
	// aborts before finishing.
	h.fake("peer-b").FailWriteAtomic("", errors.New("offline"))

	if err := h.engine.Poll(ctx(), syncID); err == nil {
		t.Fatal("poll should return the write error")
	}
	// peer-a got the write; peer-b did not.
	h.assertFile("peer-a", []byte("V2"), newMtime)
	h.assertFile("peer-b", []byte("V1"), srcMtime)
	// peer-b manifest did not advance.
	if m := h.manifest("peer-b"); m.Mtime == nil || !m.Mtime.Equal(srcMtime) {
		t.Fatalf("peer-b manifest advanced past a failed write: %v", m.Mtime)
	}

	// Heal on the next poll. peer-a is now in sync (its manifest advanced), so the
	// only remaining changed member is the primary -> still a single source.
	h.fake("peer-b").FailWriteAtomic("", nil)
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("retry poll: %v", err)
	}
	h.assertFile("peer-b", []byte("V2"), newMtime)
	if h.sync().ConflictAt != nil {
		t.Fatalf("self-heal must not spuriously conflict")
	}
}

// --- ResolveConflict -----------------------------------------------------

// backupSuffix mirrors engine.backupSuffix for test assertions.
func backupSuffix(t time.Time) string {
	return ".retrosync-conflict-" + t.UTC().Format("20060102T150405.000000000Z")
}

func TestResolveConflict_WinnerWins_BacksUpLoserAndFansOut(t *testing.T) {
	h, primaryMtime, peerMtime := seedConflicted(t)

	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The <ts> in the backup path is the RESOLUTION time, recorded as last_synced.
	resolvedAt := h.sync().LastSynced
	if resolvedAt == nil {
		t.Fatal("last_synced should be set after resolution")
	}
	// The loser's ORIGINAL file was backed up to <path>.retrosync-conflict-<ts>
	// on the loser's node, with the loser's original bytes + mtime.
	backupPath := "q.srm" + backupSuffix(*resolvedAt)
	bf, ok := h.fake("peer").Get(backupPath)
	if !ok {
		t.Fatalf("loser backup %q not found; paths=%v", backupPath, h.fake("peer").Paths())
	}
	if string(bf.Data) != "PEER-WROTE" {
		t.Fatalf("backup content = %q want PEER-WROTE", bf.Data)
	}
	if !bf.Mtime.Equal(peerMtime) {
		t.Fatalf("backup mtime = %v want %v (loser's original)", bf.Mtime, peerMtime)
	}

	// Every other member now holds the WINNER's bytes + mtime.
	h.assertFile("peer", []byte("PRIMARY"), primaryMtime)

	// Manifest updated for winner + every written node to the winner's mtime/size.
	for _, id := range []string{"primary", "peer"} {
		m := h.manifest(id)
		if m.Mtime == nil || !m.Mtime.Equal(primaryMtime) {
			t.Fatalf("%s manifest mtime = %v want %v", id, m.Mtime, primaryMtime)
		}
		if m.Size == nil || *m.Size != int64(len("PRIMARY")) {
			t.Fatalf("%s manifest size = %v want %d", id, m.Size, len("PRIMARY"))
		}
	}

	// conflict_at cleared.
	if h.sync().ConflictAt != nil {
		t.Fatalf("conflict_at should be cleared after resolution")
	}

	// sync_log has the backup row (naming the path) + the fan-out ok row.
	entries, err := h.store.ListLogBySync(ctx(), syncID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var backups, fanouts int
	for _, e := range entries {
		if e.Outcome != store.OutcomeOK {
			continue
		}
		if strings.Contains(e.Message, "conflict-resolve backup") {
			backups++
			if !strings.Contains(e.Message, backupPath) {
				t.Fatalf("backup log message should name the backup path, got %q", e.Message)
			}
		} else {
			fanouts++
		}
	}
	if backups != 1 {
		t.Fatalf("backup log rows = %d want 1", backups)
	}
	if fanouts < 1 {
		t.Fatalf("expected at least one fan-out ok row, got %d", fanouts)
	}
}

func TestResolveConflict_NotConflicted_Errors_NothingMutated(t *testing.T) {
	h, srcMtime := seedSynced(t) // clean, no conflict
	okBefore := h.logCount(store.OutcomeOK)
	writesBefore := len(h.fake("peer").Writes())

	err := h.engine.ResolveConflict(ctx(), syncID, "primary")
	if !errors.Is(err, engine.ErrNotConflicted) {
		t.Fatalf("want ErrNotConflicted, got %v", err)
	}
	if got := h.logCount(store.OutcomeOK); got != okBefore {
		t.Fatalf("ResolveConflict on a clean sync logged rows: %d -> %d", okBefore, got)
	}
	if got := len(h.fake("peer").Writes()); got != writesBefore {
		t.Fatalf("no new writes expected: %d -> %d", writesBefore, got)
	}
	h.assertFile("peer", []byte("V1"), srcMtime)
}

func TestResolveConflict_WinnerNotInScope_Errors_NothingMutated(t *testing.T) {
	h, _, _ := seedConflicted(t)
	conflictBefore := h.sync().ConflictAt

	err := h.engine.ResolveConflict(ctx(), syncID, "ghost")
	if !errors.Is(err, engine.ErrNoPath) {
		t.Fatalf("want ErrNoPath, got %v", err)
	}
	// Conflict still set, nothing mutated.
	if h.sync().ConflictAt == nil || !h.sync().ConflictAt.Equal(*conflictBefore) {
		t.Fatalf("conflict_at changed on a rejected resolve")
	}
}

func TestResolveConflict_WinnerHasNoFile_Errors(t *testing.T) {
	h, _, _ := seedConflicted(t)
	// Remove the winner's file so it can't be the source.
	h.fake("primary").Remove("p.srm")

	err := h.engine.ResolveConflict(ctx(), syncID, "primary")
	if !errors.Is(err, engine.ErrSourceMissing) {
		t.Fatalf("want ErrSourceMissing, got %v", err)
	}
	if h.sync().ConflictAt == nil {
		t.Fatalf("conflict_at should remain set after a rejected resolve")
	}
}

func TestResolveConflict_FanOutFails_LeavesConflictSet_Reresolvable(t *testing.T) {
	h, _, _ := seedConflicted(t)
	h.fake("peer").FailWriteAtomic("", errors.New("disk full"))

	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err == nil {
		t.Fatal("resolve should fail when the fan-out write fails")
	}
	// conflict_at is left set so the sync stays re-resolvable.
	if h.sync().ConflictAt == nil {
		t.Fatalf("conflict_at should remain set after a failed fan-out")
	}

	// Clear the failure and re-resolve successfully.
	h.fake("peer").FailWriteAtomic("", nil)
	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("conflict_at should be cleared after a successful re-resolve")
	}
}

// subSecondClock advances by one nanosecond per call, so two resolves in the
// same wall-clock second still get distinct backup-suffix timestamps.
func subSecondClock(start time.Time) engine.Clock {
	cur := start
	return func() time.Time {
		t := cur
		cur = cur.Add(time.Nanosecond)
		return t
	}
}

// TestResolveConflict_SameSecondResolves_DistinctBackupPaths_NoClobber asserts
// the sub-second backup-suffix: two conflict resolutions in the same wall-clock
// second must write DISTINCT backup paths, so the second never clobbers the
// first loser's only preserved snapshot.
func TestResolveConflict_SameSecondResolves_DistinctBackupPaths_NoClobber(t *testing.T) {
	// Build a sync with a sub-second clock so two resolves get nanosecond-distinct
	// timestamps within the same second.
	h := newHarness(t, subSecondClock(t0))
	base := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("V1"), base, true)
	h.addNode("peer", "q.srm", []byte("V1"), base, true)
	h.seedManifest("primary", base, int64(len("V1")))
	h.seedManifest("peer", base, int64(len("V1")))

	// First fork + resolve: peer holds LOSER-1.
	h.fake("primary").Mutate("p.srm", []byte("WIN-1"), base.Add(time.Hour))
	h.fake("peer").Mutate("q.srm", []byte("LOSER-1"), base.Add(2*time.Hour))
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	resolved1 := *h.sync().LastSynced
	backup1 := "q.srm" + backupSuffix(resolved1)

	// Second fork + resolve in the same second: peer holds LOSER-2.
	h.fake("primary").Mutate("p.srm", []byte("WIN-2"), base.Add(3*time.Hour))
	h.fake("peer").Mutate("q.srm", []byte("LOSER-2"), base.Add(4*time.Hour))
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	resolved2 := *h.sync().LastSynced
	backup2 := "q.srm" + backupSuffix(resolved2)

	if backup1 == backup2 {
		t.Fatalf("two same-second resolves produced the same backup path %q (clobber)", backup1)
	}
	// BOTH backups survive with their distinct losers.
	if bf, ok := h.fake("peer").Get(backup1); !ok || string(bf.Data) != "LOSER-1" {
		t.Fatalf("backup1 missing or wrong: %q ok=%v", backup1, ok)
	}
	if bf, ok := h.fake("peer").Get(backup2); !ok || string(bf.Data) != "LOSER-2" {
		t.Fatalf("backup2 missing or wrong: %q ok=%v", backup2, ok)
	}
}

func TestResolveConflict_NodeWithoutFile_NoBackup_StillReceivesWinner(t *testing.T) {
	// Build a conflict where the LOSER has no file: primary + peer both changed,
	// but then the peer's file is removed before resolution. The peer gets no
	// backup (nothing to preserve) but still receives the winner.
	h, primaryMtime, _ := seedConflicted(t)
	h.fake("peer").Remove("q.srm")

	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	resolvedAt := h.sync().LastSynced
	if resolvedAt == nil {
		t.Fatal("last_synced should be set")
	}
	// No backup file for the empty loser.
	if _, ok := h.fake("peer").Get("q.srm" + backupSuffix(*resolvedAt)); ok {
		t.Fatalf("an empty loser should not get a backup")
	}
	// But it still receives the winner's bytes.
	h.assertFile("peer", []byte("PRIMARY"), primaryMtime)
}

// --- NodeStates ----------------------------------------------------------

func TestNodeStates_PerNodePresentMtimeSize_SortedByNodeID(t *testing.T) {
	h, srcMtime := seedSynced(t)
	// Remove one node's file so it reports Present=false.
	h.fake("peer").Remove("q.srm")

	states, err := h.engine.NodeStates(ctx(), syncID)
	if err != nil {
		t.Fatalf("node states: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("want 2 states, got %d", len(states))
	}
	// Sorted by node id: peer < primary.
	if states[0].NodeID != "peer" || states[1].NodeID != "primary" {
		t.Fatalf("states not sorted by node id: %+v", states)
	}
	if states[0].Present {
		t.Fatalf("peer should report Present=false")
	}
	if !states[1].Present || !states[1].Mtime.Equal(srcMtime) || states[1].Size != int64(len("V1")) {
		t.Fatalf("primary state wrong: %+v", states[1])
	}
}

func TestNodeStates_StatError_IsReturned(t *testing.T) {
	h, _ := seedSynced(t)
	h.fake("peer").FailStat("", errors.New("io error"))
	if _, err := h.engine.NodeStates(ctx(), syncID); err == nil {
		t.Fatal("a non-NotExist Stat error should surface from NodeStates")
	}
}
