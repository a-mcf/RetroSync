package engine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/fakereach"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

func ctx() context.Context { return context.Background() }

// sha256Hex is the lowercase-hex sha256 of b, matching what fakereach.Hash and
// the engine's capture path produce, so a test-seeded version's hash lines up
// with what restore writes into the manifest.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// These tests exercise the AUTO-MIRROR engine: Poll computes the changed set of
// a sync's members vs the manifest and propagates (1 changed), no-ops (0), or
// flags a conflict (2+). There is no Activate/Deactivate/primary/take-over — the
// only human action is ResolveConflict. The data-loss safety properties
// (backup-before-overwrite, conflict-halts, manifest-trails-write, µs compare)
// are asserted throughout.

// --- test harness --------------------------------------------------------

const (
	gameLabel = "Super Metroid"
	syncID    = "sm-bob"
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

// logSink is a goroutine-safe io.Writer the harness points the engine's logger
// at, so a test can assert what the engine LOGGED (the mtime-collision decisions
// of slice 38 are required to be visible, not inferable) even when a test drives
// concurrent operations.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// harness wires a memory Store + a fakereach.Fake per node + an Engine.
type harness struct {
	t      *testing.T
	store  store.Store
	fakes  map[string]*fakereach.Fake
	paths  map[string]string // nodeID -> member path
	engine *engine.Engine
	logs   *logSink
	// overrides makes the engine resolve a node to an arbitrary Reach instead of
	// its fake (typically a wrapper AROUND the fake, e.g. lyingHashReach). The
	// fake stays registered as the backing store for direct test assertions.
	overrides map[string]reach.Reach
}

// overrideReach installs r as the Reach the engine resolves for nodeID.
func (h *harness) overrideReach(nodeID string, r reach.Reach) { h.overrides[nodeID] = r }

func newHarness(t *testing.T, clock engine.Clock) *harness {
	t.Helper()
	h := &harness{
		t:         t,
		store:     memory.New(),
		fakes:     map[string]*fakereach.Fake{},
		paths:     map[string]string{},
		overrides: map[string]reach.Reach{},
		logs:      &logSink{},
	}
	resolve := func(n store.Node) (reach.Reach, error) {
		if r, ok := h.overrides[n.ID]; ok {
			return r, nil
		}
		f, ok := h.fakes[n.ID]
		if !ok {
			t.Fatalf("no fake for node %q", n.ID)
		}
		return f, nil
	}
	logger := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.engine = engine.New(h.store, resolve, clock, engine.WithLogger(logger))
	if err := h.store.CreateSync(ctx(), store.Sync{ID: syncID, Game: gameLabel, Name: "Bob's stream"}); err != nil {
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

// seedManifestHash is seedManifest plus a recorded content sha256, so a
// subsequent Poll's tier-2 compare has a hash to match against (needed to
// exercise the touch path: stat differs but hash equals the manifest).
func (h *harness) seedManifestHash(nodeID string, mtime time.Time, size int64, sha string) {
	h.t.Helper()
	mt := mtime.UTC()
	sz := size
	checked := mtime.UTC()
	s := sha
	if err := h.store.SetManifest(ctx(), store.ManifestEntry{
		SyncID: syncID, NodeID: nodeID, Mtime: &mt, Size: &sz, SHA256: &s, LastChecked: &checked,
	}); err != nil {
		h.t.Fatal(err)
	}
}

// seedManifestHashChecked is seedManifestHash with an explicit LastChecked, so a
// test can put a member either side of the engine's periodic-verification
// interval: an OLD LastChecked makes the next Poll hash the member regardless of
// the stat gate (the issue #33 backstop), a RECENT one leaves the poll hash-free.
func (h *harness) seedManifestHashChecked(nodeID string, mtime time.Time, size int64, sha string, lastChecked time.Time) {
	h.t.Helper()
	mt := mtime.UTC()
	sz := size
	checked := lastChecked.UTC()
	s := sha
	if err := h.store.SetManifest(ctx(), store.ManifestEntry{
		SyncID: syncID, NodeID: nodeID, Mtime: &mt, Size: &sz, SHA256: &s, LastChecked: &checked,
	}); err != nil {
		h.t.Fatal(err)
	}
}

// hashOf returns the fakereach content hash a node currently reports for its
// member path (the lowercase-hex sha256 of the stored bytes).
func (h *harness) hashOf(nodeID string) string {
	h.t.Helper()
	hh, err := h.fake(nodeID).Hash(ctx(), h.paths[nodeID])
	if err != nil {
		h.t.Fatalf("hash %s: %v", nodeID, err)
	}
	return hh
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

// versions returns the captured save versions for a member, newest-first.
func (h *harness) versions(nodeID string) []store.SaveVersion {
	h.t.Helper()
	vs, err := h.store.ListSaveVersions(ctx(), syncID, nodeID, 0)
	if err != nil {
		h.t.Fatalf("list versions %s: %v", nodeID, err)
	}
	return vs
}

// versionData returns the bytes of the captured version at seq.
func (h *harness) versionData(seq int64) []byte {
	h.t.Helper()
	_, data, err := h.store.GetSaveVersionData(ctx(), syncID, seq)
	if err != nil {
		h.t.Fatalf("get version data %d: %v", seq, err)
	}
	return data
}

// assertFileContent asserts a node's current file content (ignoring mtime, e.g.
// after a restore that stamps the resolution time).
func (h *harness) assertFileContent(nodeID string, content []byte) {
	h.t.Helper()
	data, err := h.fake(nodeID).Read(ctx(), h.paths[nodeID])
	if err != nil {
		h.t.Fatalf("read %s: %v", nodeID, err)
	}
	if string(data) != string(content) {
		h.t.Fatalf("%s content = %q want %q", nodeID, data, content)
	}
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

// TestPoll_SameMtimeAndSize_ContentChangeIsDetected closes the blind spot of
// issue #33 on the READ side.
//
// Poll's tier-1 gate is stat-only (statDiffers: mtime + size), and save files
// are a fixed size per game, so a member whose BYTES changed while its mtime
// stayed put slips through it. RetroSync's own writes can no longer produce that
// state (the adapters never publish a colliding mtime), but an external writer
// can: syncthing PRESERVES mtimes when it delivers a file, so it can restore an
// older save carrying exactly the mtime already in the manifest. Left to the
// stat gate alone the change would be invisible PERMANENTLY, not merely late,
// and the manifest would keep a hash that no longer describes the file.
//
// The periodic verification backstop catches it: a member whose manifest has not
// been written for verifyInterval is hashed regardless of the stat gate. Here
// the manifest was last checked an hour ago, so the poll hashes and finds the
// change.
func TestPoll_SameMtimeAndSize_ContentChangeIsDetected(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	base := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("V1"), base, true)
	h.addNode("peer", "q.srm", []byte("V1"), base, true)
	v1hash := h.hashOf("primary")
	// Last verified an hour ago: both members are due for a forced hash.
	h.seedManifestHashChecked("primary", base, int64(len("V1")), v1hash, base)
	h.seedManifestHashChecked("peer", base, int64(len("V1")), v1hash, base)

	// Change the primary's CONTENT while leaving mtime and size exactly as the
	// manifest records them. "V1" and "V2" are the same length on purpose: size
	// must not be able to betray the change, mirroring real saves.
	h.fake("primary").Mutate("p.srm", []byte("V2"), base)
	v2hash := h.hashOf("primary")

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// The change is found and propagated despite the identical stat.
	h.assertFileContent("peer", []byte("V2"))
	// Both manifests now describe the bytes actually on disk.
	for _, id := range []string{"primary", "peer"} {
		m := h.manifest(id)
		if m.SHA256 == nil || *m.SHA256 != v2hash {
			t.Fatalf("%s manifest hash = %v, want the current %q", id, m.SHA256, v2hash)
		}
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("a single detected change must not conflict")
	}
	if h.sync().LastSynced == nil {
		t.Fatalf("a detected+propagated change should advance last_synced")
	}
}

// TestPoll_RecentlyChecked_StaysHashFree pins the OTHER half of the backstop's
// contract: it must not turn every poll into a hashing sweep. A member whose
// manifest was written moments ago is inside verifyInterval, so a stat-equal
// poll must not hash it at all — that hash-free fast path is the entire point of
// tier 1.
//
// It is asserted the only way that cannot be faked: Hash is injected to FAIL on
// every member, so any hash attempt would surface as a Poll error.
func TestPoll_RecentlyChecked_StaysHashFree(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	base := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("V1"), base, true)
	h.addNode("peer", "q.srm", []byte("V1"), base, true)
	v1hash := h.hashOf("primary")
	// Checked just now (well inside verifyInterval): not due.
	h.seedManifestHashChecked("primary", base, int64(len("V1")), v1hash, t0)
	h.seedManifestHashChecked("peer", base, int64(len("V1")), v1hash, t0)

	h.fake("primary").FailHash("", errors.New("hash must not be called"))
	h.fake("peer").FailHash("", errors.New("hash must not be called"))

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("a stat-equal poll inside verifyInterval must not hash: %v", err)
	}
	if h.sync().ConflictAt != nil || h.sync().LastSynced != nil {
		t.Fatalf("a hash-free noop poll must not touch sync state")
	}
}

// TestPoll_VerificationSweep_IdenticalBytes_IsNotAChange asserts the forced hash
// is a VERIFICATION, not a change: when the bytes still match the manifest the
// sweep reconciles (advancing last_checked so the member is not re-verified
// until the next interval) and propagates nothing. Without this, the backstop
// would re-fan-out every member every interval.
func TestPoll_VerificationSweep_IdenticalBytes_IsNotAChange(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	base := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("V1"), base, true)
	h.addNode("peer", "q.srm", []byte("V1"), base, true)
	v1hash := h.hashOf("primary")
	h.seedManifestHashChecked("primary", base, int64(len("V1")), v1hash, base)
	h.seedManifestHashChecked("peer", base, int64(len("V1")), v1hash, base)

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// Nothing changed: no writes, no conflict, no last_synced advance.
	for _, id := range []string{"primary", "peer"} {
		if n := len(h.fake(id).Writes()); n != 0 {
			t.Fatalf("%s was written %d times by a verification sweep, want 0", id, n)
		}
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("a verification sweep that found nothing wrong must not conflict")
	}
	if h.sync().LastSynced != nil {
		t.Fatalf("a verification sweep that found nothing wrong must not advance last_synced")
	}
	// last_checked advanced to the poll time, and mtime/size/hash are unchanged.
	for _, id := range []string{"primary", "peer"} {
		m := h.manifest(id)
		if m.LastChecked == nil || !m.LastChecked.Equal(t0) {
			t.Fatalf("%s last_checked = %v, want the poll time %v", id, m.LastChecked, t0)
		}
		if m.SHA256 == nil || *m.SHA256 != v1hash {
			t.Fatalf("%s manifest hash = %v, want the unchanged %q", id, m.SHA256, v1hash)
		}
		if m.Mtime == nil || !m.Mtime.Equal(base) {
			t.Fatalf("%s manifest mtime = %v, want the unchanged %v", id, m.Mtime, base)
		}
	}

	// And the member is now inside the interval: the next poll is hash-free again
	// (a hash attempt would surface as an error).
	h.fake("primary").FailHash("", errors.New("hash must not be called"))
	h.fake("peer").FailHash("", errors.New("hash must not be called"))
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll right after a sweep must be hash-free: %v", err)
	}
}

// TestPoll_FanOut_MtimePolicy is the write-side of issue #33 as slice 38 settles
// it: RetroSync may alter a published save's mtime ONLY where a human made an
// explicit choice, and only when the source mtime EXACTLY collides with the
// destination's.
//
// The collision matters because both syncthing's scanner and the engine's own
// tier-1 stat gate decide "did this change?" from mtime+size, and saves are a
// fixed size per game — so bytes republished under exactly the destination's
// current mtime are invisible to both, permanently. But a routine auto-mirror
// must NOT quietly paper over that: it warns and writes the source mtime, because
// in steady state a collision means the source's content moved while its mtime
// did not, which is an anomaly a human should see.
//
// Intent is discriminated by LastSynced: a sync that has never synced is being
// INITIATED by the person who just set it up (the same discriminator as the
// first-sync card, slice 31).
//
// Setup in every case: the peer is unchanged (manifest matches its file) and the
// primary appeared/changed, so the primary is the lone source and fans out to the
// peer.
func TestPoll_FanOut_MtimePolicy(t *testing.T) {
	base := t0.Add(-2 * time.Hour)

	tests := []struct {
		name string
		// synced marks the sync as having synced before (LastSynced != nil), i.e. a
		// ROUTINE auto-mirror pass rather than the sync's initiation.
		synced bool
		// srcMtime is the mtime of the changed source file; the peer's file always
		// sits at base.
		srcMtime time.Time
		// wantMtime is the mtime the peer's file (and its manifest) must end up with.
		wantMtime time.Time
		// wantLevel is the level of the log line the decision must emit ("" = the
		// engine must say nothing, because it did nothing).
		wantLevel string
	}{
		{
			name:      "initiation, colliding mtime: bumped 1µs so the write is visible",
			srcMtime:  base,
			wantMtime: base.Add(time.Microsecond),
			wantLevel: "INFO",
		},
		{
			name:      "initiation, distinct mtime: source mtime published untouched",
			srcMtime:  base.Add(time.Minute),
			wantMtime: base.Add(time.Minute),
		},
		{
			name: "initiation, OLDER mtime: still untouched (backwards is still a difference)",
			// Slice 34 bumped anything "not strictly newer"; only exact equality can
			// hide a write, and moving backwards is detected by both stat gates.
			srcMtime:  base.Add(-time.Minute),
			wantMtime: base.Add(-time.Minute),
		},
		{
			name:      "routine mirror, colliding mtime: NOT bumped, warned about instead",
			synced:    true,
			srcMtime:  base,
			wantMtime: base,
			wantLevel: "WARN",
		},
		{
			name:      "routine mirror, distinct mtime: source mtime published untouched",
			synced:    true,
			srcMtime:  base.Add(time.Minute),
			wantMtime: base.Add(time.Minute),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, steppingClock(t0, time.Second))
			h.addNode("primary", "p.srm", []byte("V2"), tc.srcMtime, true)
			h.addNode("peer", "q.srm", []byte("V1"), base, true)
			// The peer is fully described by its manifest => unchanged. The primary has
			// no manifest row => it "appeared" and is the single changed member.
			h.seedManifestHash("peer", base, int64(len("V1")), h.hashOf("peer"))
			if tc.synced {
				if err := h.store.MarkSyncSynced(ctx(), syncID, t0.Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
			}

			if err := h.engine.Poll(ctx(), syncID); err != nil {
				t.Fatalf("poll: %v", err)
			}

			// The bytes always land; only the mtime policy differs.
			h.assertFile("peer", []byte("V2"), tc.wantMtime)
			// The manifest records what was really published, so the peer's file and
			// its manifest agree.
			m := h.manifest("peer")
			if m.Mtime == nil || !m.Mtime.Equal(tc.wantMtime.UTC().Truncate(time.Microsecond)) {
				t.Fatalf("peer manifest mtime = %v, want the published %v", m.Mtime, tc.wantMtime)
			}

			// Every decision about an mtime must be VISIBLE in the log — silence is
			// what made issue #33 unanswerable — and a no-decision must be silent.
			logs := h.logs.String()
			switch tc.wantLevel {
			case "":
				if strings.Contains(logs, "mtime") {
					t.Fatalf("engine logged about mtimes when it took no mtime decision:\n%s", logs)
				}
			default:
				if !strings.Contains(logs, `"level":"`+tc.wantLevel+`"`) {
					t.Fatalf("want a %s log line about the mtime collision, got:\n%s", tc.wantLevel, logs)
				}
				for _, want := range []string{`"sync_id":"` + syncID + `"`, `"node_id":"peer"`, `"src_mtime"`, `"dst_mtime"`} {
					if !strings.Contains(logs, want) {
						t.Fatalf("mtime log line is missing %s:\n%s", want, logs)
					}
				}
			}
		})
	}
}

// TestPoll_Initiation_CollisionBump_NextPollIsNoop pins the bookkeeping half of
// the initiation bump: because the manifest records the mtime that was really
// published (not the source's), the very next poll sees the peer as unchanged and
// writes nothing. A manifest that recorded the requested mtime would re-detect
// the peer every pass and re-arm exactly the divergence the bump prevents.
func TestPoll_Initiation_CollisionBump_NextPollIsNoop(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	base := t0.Add(-2 * time.Hour)
	h.addNode("primary", "p.srm", []byte("V2"), base, true)
	h.addNode("peer", "q.srm", []byte("V1"), base, true)
	h.seedManifestHash("peer", base, int64(len("V1")), h.hashOf("peer"))

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	h.assertFile("peer", []byte("V2"), base.Add(time.Microsecond))

	writesBefore := len(h.fake("peer").Writes())
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("re-poll: %v", err)
	}
	if got := len(h.fake("peer").Writes()); got != writesBefore {
		t.Fatalf("re-poll wrote to the peer again: %d -> %d (manifest does not describe the file)", writesBefore, got)
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("re-poll must not conflict")
	}
}

// TestPoll_Touch_NotChanged_ReconcilesManifest pins the tier-2 touch path: a
// member whose mtime moved but whose BYTES are identical (a `touch`) is NOT a
// change — nothing propagates, no conflict, and the manifest's mtime/size is
// reconciled to the new stat so the next poll fast-paths it (stat-equal, no
// hash). This is the false-positive that bare mtime+size produced.
func TestPoll_Touch_NotChanged_ReconcilesManifest(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	base := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("V1"), base, true)
	h.addNode("peer", "q.srm", []byte("V1"), base, true)
	// Seed the manifest WITH the correct content hash so tier 2 can recognize the
	// touch.
	v1hash := h.hashOf("primary")
	h.seedManifestHash("primary", base, int64(len("V1")), v1hash)
	h.seedManifestHash("peer", base, int64(len("V1")), v1hash)

	// Touch the primary: same bytes, NEW mtime (and a manifest mtime ≥1µs away so
	// the stat gate trips).
	touchedMtime := base.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V1"), touchedMtime)

	writesBefore := len(h.fake("peer").Writes())
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// No propagation (a touch is not a change), no conflict, no last_synced.
	if got := len(h.fake("peer").Writes()); got != writesBefore {
		t.Fatalf("touch poll wrote to peer: %d -> %d", writesBefore, got)
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("a touch must not conflict")
	}
	if h.sync().LastSynced != nil {
		t.Fatalf("a touch must not advance last_synced")
	}
	// The primary's manifest mtime was reconciled to the touched mtime (so the next
	// poll fast-paths it) while the hash stayed the same.
	m := h.manifest("primary")
	if m.Mtime == nil || !m.Mtime.Equal(touchedMtime.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("primary manifest mtime not reconciled: %v want %v", m.Mtime, touchedMtime)
	}
	if m.SHA256 == nil || *m.SHA256 != v1hash {
		t.Fatalf("primary manifest hash = %v want %q (unchanged)", m.SHA256, v1hash)
	}

	// The peer's file is untouched.
	h.assertFile("peer", []byte("V1"), base)

	// And a follow-up poll is now a pure stat-equal noop.
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("follow-up poll: %v", err)
	}
	if h.sync().ConflictAt != nil || h.sync().LastSynced != nil {
		t.Fatalf("follow-up poll must be a clean noop")
	}
}

// TestPoll_TwoChangedSameContent_NotConflict_Propagates is the KEY new test:
// two members changed to the SAME bytes (e.g. both already received your save
// via the backup channel). With hash-backed detection that is ONE distinct hash
// — an agreed content, NOT a fork — so it propagates to the lagging member and
// does NOT conflict.
func TestPoll_TwoChangedSameContent_NotConflict_Propagates(t *testing.T) {
	h, _ := seedSyncedMulti(t) // primary, peer-a, peer-b all at V1
	// primary and peer-a both change to the SAME new content; peer-b lags at V1.
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V2-SAME"), newMtime)
	h.fake("peer-a").Mutate("a.srm", []byte("V2-SAME"), newMtime.Add(time.Minute))

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// NOT a conflict — the two changers agree on content.
	if h.sync().ConflictAt != nil {
		t.Fatalf("two members at the SAME content must NOT conflict")
	}
	// peer-b (the lagging member) receives the agreed content. The source is the
	// first changer by sorted node id (peer-a < primary), so peer-b inherits
	// peer-a's mtime.
	h.assertFile("peer-b", []byte("V2-SAME"), newMtime.Add(time.Minute))
	if h.sync().LastSynced == nil {
		t.Fatalf("an agreed-content propagate should advance last_synced")
	}
	// The two co-changers were NOT re-written (they already held the content): only
	// peer-b got a write.
	if n := len(h.fake("primary").Writes()); n != 0 {
		t.Fatalf("primary (a co-changer) was re-written %d times, want 0", n)
	}
	if n := len(h.fake("peer-a").Writes()); n != 0 {
		t.Fatalf("peer-a (a co-changer) was re-written %d times, want 0", n)
	}
	// Manifest carries the agreed hash for every member.
	wantHash := h.hashOf("primary")
	for _, id := range []string{"primary", "peer-a", "peer-b"} {
		m := h.manifest(id)
		if m.SHA256 == nil || *m.SHA256 != wantHash {
			t.Fatalf("%s manifest hash = %v want %q", id, m.SHA256, wantHash)
		}
	}
}

// TestPoll_NewSyncTwoMembersIdenticalSaves_NotConflict is the onboarding case
// the slice-18 brief flagged: a brand-new sync (empty manifest) where two
// members already hold the SAME save. Pre-hash this conflicted (2 changed);
// hash-backed detection sees one distinct hash and treats it as agreed content —
// no conflict, no spurious overwrite (both already match).
func TestPoll_NewSyncTwoMembersIdenticalSaves_NotConflict(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	aMtime := t0.Add(-2 * time.Hour)
	bMtime := t0.Add(-time.Hour)
	// Same bytes on both, NO seeded manifest.
	h.addNode("primary", "p.srm", []byte("SAME-SAVE"), aMtime, true)
	h.addNode("peer", "q.srm", []byte("SAME-SAVE"), bMtime, true)

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("a fresh sync with two IDENTICAL saves must NOT conflict")
	}
	// Neither member needed an overwrite (both already hold the agreed content).
	if n := len(h.fake("primary").Writes()); n != 0 {
		t.Fatalf("primary written %d times, want 0", n)
	}
	if n := len(h.fake("peer").Writes()); n != 0 {
		t.Fatalf("peer written %d times, want 0", n)
	}
	// Manifest now records the agreed hash for both, so the next poll is a noop.
	wantHash := h.hashOf("primary")
	for _, id := range []string{"primary", "peer"} {
		if m := h.manifest(id); m.SHA256 == nil || *m.SHA256 != wantHash {
			t.Fatalf("%s manifest hash = %v want %q", id, m.SHA256, wantHash)
		}
	}
	if h.sync().LastSynced == nil {
		t.Fatalf("an agreed-content onboarding poll should advance last_synced")
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

// TestPoll_DistinctHashFork_NeverOverwrites is the data-loss guarantee: a
// genuine fork (changed members with DIFFERENT content hashes) must flag a
// conflict and write NOTHING — every diverged member keeps its exact bytes. This
// pins the "never silently overwrite a distinct-hash changer" guarantee.
func TestPoll_DistinctHashFork_NeverOverwrites(t *testing.T) {
	h, srcMtime := seedSyncedMulti(t) // primary, peer-a, peer-b at V1
	// primary and peer-a fork to DIFFERENT content; peer-b is innocent (still V1).
	pMtime := t0.Add(time.Hour)
	aMtime := t0.Add(2 * time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("FORK-PRIMARY"), pMtime)
	h.fake("peer-a").Mutate("a.srm", []byte("FORK-PEER-A"), aMtime)

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if h.sync().ConflictAt == nil {
		t.Fatalf("distinct-content changers must conflict")
	}
	// NOT ONE byte written anywhere: each diverged member keeps its own content,
	// and the innocent member keeps V1.
	for _, id := range []string{"primary", "peer-a", "peer-b"} {
		if n := len(h.fake(id).Writes()); n != 0 {
			t.Fatalf("%s was written %d times during a fork; want 0 (no overwrite)", id, n)
		}
	}
	h.assertFile("primary", []byte("FORK-PRIMARY"), pMtime)
	h.assertFile("peer-a", []byte("FORK-PEER-A"), aMtime)
	h.assertFile("peer-b", []byte("V1"), srcMtime)
	if h.sync().LastSynced != nil {
		t.Fatalf("a fork must not advance last_synced")
	}
}

// TestPoll_OneChanged_RecordsHashOnPeers asserts the single-changed propagate
// path records the source's content hash on the source AND every peer it fans
// out to, so a re-poll is a clean noop (stat- and hash-stable).
func TestPoll_OneChanged_RecordsHashOnPeers(t *testing.T) {
	h, _ := seedSynced(t)
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V2-NEW"), newMtime)

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	wantHash := h.hashOf("primary")
	for _, id := range []string{"primary", "peer"} {
		m := h.manifest(id)
		if m.SHA256 == nil || *m.SHA256 != wantHash {
			t.Fatalf("%s manifest hash = %v want %q", id, m.SHA256, wantHash)
		}
	}
	// A re-poll is a pure noop: stat-equal everywhere (no spurious change).
	writesBefore := len(h.fake("peer").Writes())
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("re-poll: %v", err)
	}
	if got := len(h.fake("peer").Writes()); got != writesBefore {
		t.Fatalf("re-poll wrote: %d -> %d (want noop)", writesBefore, got)
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("re-poll must not conflict")
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

// TestPoll_HashFails_AbortsNoMutation pins the fail-SAFE property the auditor
// flagged unpinned: a Reach.Hash error mid-poll (tier-2 content compare) aborts
// the WHOLE pass before any write, manifest advance, or conflict flag. A hash
// error is NOT defaulted to "unchanged", NOT propagated, and does NOT become a
// false conflict — the engine returns the error and mutates nothing.
func TestPoll_HashFails_AbortsNoMutation(t *testing.T) {
	h, srcMtime := seedSynced(t) // primary + peer both at V1, in sync
	// Move the primary's stat so the engine reaches the tier-2 hash step for it
	// (stat differs from the manifest -> it must hash to decide change vs touch).
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V2-CHANGED"), newMtime)
	// Inject a Hash failure for the primary: the content compare can't run.
	hashErr := errors.New("hash io error")
	h.fake("primary").FailHash("", hashErr)

	peerWritesBefore := len(h.fake("peer").Writes())
	primWritesBefore := len(h.fake("primary").Writes())

	// Poll must surface the hash error.
	if err := h.engine.Poll(ctx(), syncID); !errors.Is(err, hashErr) {
		t.Fatalf("Poll should return the hash error, got %v", err)
	}

	// Fail-safe: NOTHING was written to any member.
	if got := len(h.fake("peer").Writes()); got != peerWritesBefore {
		t.Fatalf("peer was written despite a hash failure: %d -> %d", peerWritesBefore, got)
	}
	if got := len(h.fake("primary").Writes()); got != primWritesBefore {
		t.Fatalf("primary was written despite a hash failure: %d -> %d", primWritesBefore, got)
	}
	// The peer's file is untouched (no propagation).
	h.assertFile("peer", []byte("V1"), srcMtime)

	// No manifest advanced: both stay at the pre-poll V1 state (no SHA256 carried).
	for _, id := range []string{"primary", "peer"} {
		m := h.manifest(id)
		if m.Mtime == nil || !m.Mtime.Equal(srcMtime) {
			t.Fatalf("%s manifest advanced past a hash failure: mtime=%v want %v", id, m.Mtime, srcMtime)
		}
		if m.SHA256 != nil {
			t.Fatalf("%s manifest recorded a hash despite the hash failure: %q", id, *m.SHA256)
		}
	}

	// No false conflict: conflict_at stays nil and no conflict was logged.
	if h.sync().ConflictAt != nil {
		t.Fatalf("a hash failure must NOT flag a conflict")
	}
	if n := h.logCount(store.OutcomeConflict); n != 0 {
		t.Fatalf("a hash failure logged %d conflict rows, want 0", n)
	}
	// last_synced did not advance.
	if h.sync().LastSynced != nil {
		t.Fatalf("a hash failure must NOT advance last_synced")
	}
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

// TestPoll_Propagate_CaptureFails_AbortsWrite_ManifestNotAdvanced asserts the
// capture-before-overwrite hard gate on the NORMAL propagate path (not just
// resolve): when a single member changes and fan-out tries to overwrite a peer,
// the peer's pre-overwrite bytes are captured FIRST — and if that capture fails,
// the WriteAtomic is aborted, the peer's recoverable bytes survive, and the
// peer's manifest is not advanced (so the next poll retries). This mirrors
// TestResolveConflict_CaptureFails_AbortsWrite_NothingDestroyed for propagation.
func TestPoll_Propagate_CaptureFails_AbortsWrite_ManifestNotAdvanced(t *testing.T) {
	h, srcMtime := seedSynced(t)
	newMtime := t0.Add(time.Hour)
	// One member changes -> a single distinct hash -> propagate to the peer.
	h.fake("primary").Mutate("p.srm", []byte("V2"), newMtime)
	// Force the peer's capture to fail: captureBeforeOverwrite READS the dst
	// before overwriting (and hashes those bytes in-process), so a Read failure
	// fails the capture.
	h.fake("peer").FailRead("", errors.New("capture boom"))

	if err := h.engine.Poll(ctx(), syncID); err == nil {
		t.Fatal("poll should return the capture error")
	}
	// Clear the injected failure so the assertions below can read the fake.
	h.fake("peer").FailRead("", nil)
	// The peer's original bytes survive (the WriteAtomic was aborted by the gate).
	h.assertFile("peer", []byte("V1"), srcMtime)
	// The peer's manifest did NOT advance past the aborted write.
	if m := h.manifest("peer"); m.Mtime == nil || !m.Mtime.Equal(srcMtime) {
		t.Fatalf("peer manifest advanced past an aborted (capture-failed) write: %v", m.Mtime)
	}
	// No version was committed for the peer (the put never ran).
	if vs := h.versions("peer"); len(vs) != 0 {
		t.Fatalf("captured %d versions despite capture failure, want 0", len(vs))
	}
	// last_synced must not advance on a failed pass.
	if h.sync().LastSynced != nil {
		t.Fatalf("last_synced advanced despite an aborted fan-out")
	}

	// The failure is already cleared; the next poll re-detects and heals.
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("retry poll: %v", err)
	}
	h.assertFile("peer", []byte("V2"), newMtime)
}

// --- ResolveConflict -----------------------------------------------------

func TestResolveConflict_WinnerWins_CapturesLoserAndFansOut(t *testing.T) {
	h, primaryMtime, _ := seedConflicted(t)

	// Capture the loser's pre-resolution content+hash so we can assert the
	// server-side snapshot is its exact bytes.
	loserBytes := []byte("PEER-WROTE")
	loserHash := h.hashOf("peer")

	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	resolvedAt := h.sync().LastSynced
	if resolvedAt == nil {
		t.Fatal("last_synced should be set after resolution")
	}

	// The loser's ORIGINAL bytes were captured to the SERVER save-version store
	// (NOT a device-side sibling file) before being overwritten.
	vs := h.versions("peer")
	if len(vs) != 1 {
		t.Fatalf("loser captured versions = %d, want 1", len(vs))
	}
	if vs[0].Hash != loserHash || vs[0].Reason != "conflict-resolve" {
		t.Fatalf("loser version = %+v, want hash %q reason conflict-resolve", vs[0], loserHash)
	}
	if got := h.versionData(vs[0].Seq); string(got) != string(loserBytes) {
		t.Fatalf("captured loser bytes = %q, want %q", got, loserBytes)
	}
	// The recovery net REPLACES the old device-side sibling backup: no
	// .retrosync-conflict-* file is written to the device anymore.
	for _, p := range h.fake("peer").Paths() {
		if strings.Contains(p, ".retrosync-conflict-") {
			t.Fatalf("a device-side sibling backup %q was written; expected server-side capture only", p)
		}
	}

	// Every other member now holds the WINNER's bytes AND the winner's mtime. The
	// loser's file was newer (seedConflicted: the peer changed last), so this moves
	// the peer's mtime BACKWARDS — which is fine and deliberate (slice 38): a
	// backwards mtime is still a DIFFERENCE, so every stat gate still sees the
	// write. Only an EXACT collision hides a write, and these two mtimes differ.
	wantPeerMtime := primaryMtime
	h.assertFile("peer", []byte("PRIMARY"), wantPeerMtime)

	// Manifest updated for winner + every written node to the mtime each member's
	// file actually carries, and to the winner's size.
	for _, id := range []string{"primary", "peer"} {
		wantMtime := primaryMtime
		if id == "peer" {
			wantMtime = wantPeerMtime
		}
		m := h.manifest(id)
		if m.Mtime == nil || !m.Mtime.Equal(wantMtime) {
			t.Fatalf("%s manifest mtime = %v want %v", id, m.Mtime, wantMtime)
		}
		if m.Size == nil || *m.Size != int64(len("PRIMARY")) {
			t.Fatalf("%s manifest size = %v want %d", id, m.Size, len("PRIMARY"))
		}
	}

	// conflict_at cleared.
	if h.sync().ConflictAt != nil {
		t.Fatalf("conflict_at should be cleared after resolution")
	}

	// sync_log has the fan-out ok row; no device-side "backup" rows anymore.
	entries, err := h.store.ListLogBySync(ctx(), syncID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fanouts int
	for _, e := range entries {
		if e.Outcome != store.OutcomeOK {
			continue
		}
		if strings.Contains(e.Message, "conflict-resolve backup") {
			t.Fatalf("device-side backup log row should not exist anymore: %q", e.Message)
		}
		fanouts++
	}
	if fanouts < 1 {
		t.Fatalf("expected at least one fan-out ok row, got %d", fanouts)
	}
}

// TestResolveConflict_CollidingMtimes_BumpsSoTheChoiceIsVisible is the case that
// makes conflict resolution user-initiated (slice 38). Two members that diverged
// while carrying the SAME mtime are exactly how they became invisible to each
// other — same mtime, same size, so no stat gate on either side ever looked at
// the bytes. If the resolve then published the winner's mtime verbatim, the
// loser's file would change on disk under an unchanged mtime and NOTHING
// downstream (syncthing's scanner, the next poll's tier 1) would notice: the
// human would watch their choice evaporate.
//
// So a resolve — a human's explicit pick — publishes destination + 1µs, and says
// so in the log.
func TestResolveConflict_CollidingMtimes_BumpsSoTheChoiceIsVisible(t *testing.T) {
	h, base := seedSynced(t)
	// Both members diverge AT THE SAME MTIME (same size, too: the fixed-size save
	// case). This is the state issue #33 is about.
	collided := base.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("AAAA"), collided)
	h.fake("peer").Mutate("q.srm", []byte("BBBB"), collided)
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if h.sync().ConflictAt == nil {
		t.Fatal("setup: expected the sync to be conflicted")
	}

	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The loser holds the winner's bytes under an mtime one microsecond past its
	// own — a difference, therefore detectable, therefore the choice took effect.
	want := collided.Add(time.Microsecond)
	h.assertFile("peer", []byte("AAAA"), want)
	m := h.manifest("peer")
	if m.Mtime == nil || !m.Mtime.Equal(want) {
		t.Fatalf("peer manifest mtime = %v, want the published %v", m.Mtime, want)
	}
	// The winner keeps its own mtime: only the destination that would have been
	// invisible is nudged.
	h.assertFile("primary", []byte("AAAA"), collided)

	logs := h.logs.String()
	if !strings.Contains(logs, `"level":"INFO"`) || !strings.Contains(logs, `"node_id":"peer"`) {
		t.Fatalf("the mtime bump must be logged with the node it applied to, got:\n%s", logs)
	}
	if strings.Contains(logs, `"level":"WARN"`) {
		t.Fatalf("a user-initiated bump must not warn:\n%s", logs)
	}
}

// TestResolveConflict_CaptureFails_AbortsWrite_NothingDestroyed asserts the
// capture-before-overwrite hard gate: if the server capture fails, the loser's
// file is NOT overwritten (its recoverable bytes survive) and the sync stays
// conflicted (re-resolvable). The capture is forced to fail by making the
// loser's Read error (capture reads the dst and hashes those bytes in-process).
func TestResolveConflict_CaptureFails_AbortsWrite_NothingDestroyed(t *testing.T) {
	h, _, peerMtime := seedConflicted(t)
	h.fake("peer").FailRead("", errors.New("read boom"))

	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err == nil {
		t.Fatal("resolve should fail when the capture fails")
	}
	// Clear the injected failure so the assertions below can read the fake.
	h.fake("peer").FailRead("", nil)
	// The loser's original file is intact (not overwritten with the winner).
	h.assertFile("peer", []byte("PEER-WROTE"), peerMtime)
	// Conflict stays set -> re-resolvable.
	if h.sync().ConflictAt == nil {
		t.Fatalf("conflict_at should remain set after a capture failure")
	}
	// No version was committed for the loser (the put never ran).
	if vs := h.versions("peer"); len(vs) != 0 {
		t.Fatalf("captured %d versions despite capture failure, want 0", len(vs))
	}
}

// TestResolveConflict_RecordsWinnerHash asserts the resolve path records the
// winner's content hash on the winner and every fanned-out member, so a poll
// after resolution is a clean (stat- and hash-stable) noop.
func TestResolveConflict_RecordsWinnerHash(t *testing.T) {
	h, _, _ := seedConflicted(t)
	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantHash := h.hashOf("primary")
	for _, id := range []string{"primary", "peer"} {
		m := h.manifest(id)
		if m.SHA256 == nil || *m.SHA256 != wantHash {
			t.Fatalf("%s manifest hash = %v want %q (winner's)", id, m.SHA256, wantHash)
		}
	}
	// A poll after resolution is a noop (no spurious change).
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("post-resolve poll: %v", err)
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("post-resolve poll must not re-conflict")
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

// TestResolveConflict_SuccessiveResolves_BothLosersCaptured asserts the server
// store preserves the loser's pre-resolution bytes across two successive
// resolutions: each capture is a distinct content-addressed version (keyed by the
// monotonic seq), so the second never clobbers the first loser's snapshot.
func TestResolveConflict_SuccessiveResolves_BothLosersCaptured(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
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

	// Second fork + resolve: peer holds LOSER-2.
	h.fake("primary").Mutate("p.srm", []byte("WIN-2"), base.Add(3*time.Hour))
	h.fake("peer").Mutate("q.srm", []byte("LOSER-2"), base.Add(4*time.Hour))
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve 2: %v", err)
	}

	// BOTH losers survive as distinct server-side versions (newest-first).
	vs := h.versions("peer")
	if len(vs) != 2 {
		t.Fatalf("peer captured versions = %d, want 2", len(vs))
	}
	if got := string(h.versionData(vs[0].Seq)); got != "LOSER-2" {
		t.Fatalf("newest capture = %q, want LOSER-2", got)
	}
	if got := string(h.versionData(vs[1].Seq)); got != "LOSER-1" {
		t.Fatalf("older capture = %q, want LOSER-1", got)
	}
}

func TestResolveConflict_NodeWithoutFile_NoCapture_StillReceivesWinner(t *testing.T) {
	// Build a conflict where the LOSER has no file: primary + peer both changed,
	// but then the peer's file is removed before resolution. The peer gets no
	// capture (nothing to preserve) but still receives the winner.
	h, primaryMtime, _ := seedConflicted(t)
	h.fake("peer").Remove("q.srm")

	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if h.sync().LastSynced == nil {
		t.Fatal("last_synced should be set")
	}
	// No captured version for the empty loser (nothing to snapshot).
	if vs := h.versions("peer"); len(vs) != 0 {
		t.Fatalf("an empty loser should not be captured: %d versions", len(vs))
	}
	// But it still receives the winner's bytes.
	h.assertFile("peer", []byte("PRIMARY"), primaryMtime)
}

// --- RestoreVersion ------------------------------------------------------

// captureFor seeds a captured save version for (sync,node) with the given bytes
// by driving a real overwrite through the engine's capture path: it mutates the
// node + polls, which captures the OLD bytes of every overwritten member. Rather
// than rely on poll timing, the tests below capture directly via the store and
// then restore, asserting the engine writes the bytes back and propagates.

// TestRestoreVersion_WritesBackAndPropagates asserts that restoring a captured
// version makes those bytes the current content of the target member AND fans
// them out to every other member, clears any conflict, and marks synced.
func TestRestoreVersion_WritesBackAndPropagates(t *testing.T) {
	h, srcMtime := seedSynced(t) // primary + peer both hold "V1"@srcMtime

	// Manually capture a known old version for "primary" in the server store.
	old := []byte("OLD-SAVE")
	oldHash := sha256Hex(old)
	if err := h.store.PutSaveVersion(ctx(), syncID, "primary", oldHash, old, "propagate"); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	vs := h.versions("primary")
	if len(vs) != 1 {
		t.Fatalf("seeded versions = %d, want 1", len(vs))
	}
	seq := vs[0].Seq

	if err := h.engine.RestoreVersion(ctx(), syncID, seq); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// Both members now hold the restored bytes.
	now := h.sync().LastSynced
	if now == nil {
		t.Fatal("last_synced should be set after restore")
	}
	for _, id := range []string{"primary", "peer"} {
		data, err := h.fake(id).Read(ctx(), h.paths[id])
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if string(data) != string(old) {
			t.Fatalf("%s content = %q, want %q (restored)", id, data, old)
		}
		// Manifest carries the restored hash.
		m := h.manifest(id)
		if m.SHA256 == nil || *m.SHA256 != oldHash {
			t.Fatalf("%s manifest hash = %v, want %q", id, m.SHA256, oldHash)
		}
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("restore should clear conflict_at")
	}
	// The peer's pre-restore bytes ("V1") were captured before being overwritten.
	pv := h.versions("peer")
	if len(pv) != 1 || pv[0].Reason != "restore" {
		t.Fatalf("peer pre-restore capture = %+v, want one reason=restore", pv)
	}
	if got := string(h.versionData(pv[0].Seq)); got != "V1" {
		t.Fatalf("peer captured pre-restore bytes = %q, want V1", got)
	}
	// The RESTORE TARGET's own pre-restore bytes ("V1") were ALSO captured before
	// being overwritten, so a mis-click restore is itself reversible. primary's
	// versions are: the seeded OLD-SAVE (newest, the one we restored) plus a fresh
	// reason=restore capture of its pre-restore "V1".
	tv := h.versions("primary") // newest-first
	var restoreCap *store.SaveVersion
	for i := range tv {
		if tv[i].Reason == "restore" {
			restoreCap = &tv[i]
			break
		}
	}
	if restoreCap == nil {
		t.Fatalf("restore target's own pre-restore bytes were not captured (a mis-click would be irreversible): %+v", tv)
	}
	if got := string(h.versionData(restoreCap.Seq)); got != "V1" {
		t.Fatalf("target captured pre-restore bytes = %q, want V1", got)
	}
	_ = srcMtime
}

// TestRestoreVersion_MtimePolicy is the restore half of slice 38's rule: picking
// a version to bring back is an explicit human choice, so a restore may nudge a
// colliding mtime — on the TARGET member (written directly, outside the fan-out)
// and on every other member (written by the fan-out) alike.
//
// It matters for the same reason it matters on a conflict resolve: a restore
// publishes the resolution time, and any member whose file already carries
// exactly that mtime would receive the restored bytes under an unchanged
// mtime+size — invisible to syncthing's scanner and to the next poll's tier 1, so
// the user's choice would silently do nothing on that member.
//
// The engine's clock is frozen at t0 here, so "the restore time" is t0 and a
// member seeded at t0 is precisely the colliding case.
func TestRestoreVersion_MtimePolicy(t *testing.T) {
	old := []byte("OLD-SAVE")
	oldHash := sha256Hex(old)
	base := t0.Add(-time.Hour) // safely distinct from the restore time

	tests := []struct {
		name string
		// seeded file mtimes; t0 == the restore time == a collision.
		primaryMtime, peerMtime time.Time
		// mtimes each member's file must end up carrying.
		wantPrimary, wantPeer time.Time
		// wantLogNode is the node the bump must be logged against ("" = no bump, so
		// the engine must say nothing).
		wantLogNode string
	}{
		{
			// The RESTORE TARGET itself already carries the restore time: the direct
			// write (which bypasses fanOut) must still be nudged.
			name:         "target mtime collides with the restore time",
			primaryMtime: t0,
			peerMtime:    base,
			wantPrimary:  t0.Add(time.Microsecond),
			// The fan-out then propagates what the target actually published.
			wantPeer:    t0.Add(time.Microsecond),
			wantLogNode: "primary",
		},
		{
			// A NON-target member carries the restore time: the fan-out copy to it
			// must be nudged.
			name:         "another member's mtime collides with the restore time",
			primaryMtime: base,
			peerMtime:    t0,
			wantPrimary:  t0,
			wantPeer:     t0.Add(time.Microsecond),
			wantLogNode:  "peer",
		},
		{
			name:         "nothing collides: the restore time is published verbatim",
			primaryMtime: base,
			peerMtime:    base,
			wantPrimary:  t0,
			wantPeer:     t0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A FROZEN clock: every engine timestamp in this restore is exactly t0.
			h := newHarness(t, steppingClock(t0, 0))
			h.addNode("primary", "p.srm", []byte("V1"), tc.primaryMtime, true)
			h.addNode("peer", "q.srm", []byte("V1"), tc.peerMtime, true)
			if err := h.store.PutSaveVersion(ctx(), syncID, "primary", oldHash, old, "propagate"); err != nil {
				t.Fatalf("seed version: %v", err)
			}
			seq := h.versions("primary")[0].Seq

			if err := h.engine.RestoreVersion(ctx(), syncID, seq); err != nil {
				t.Fatalf("restore: %v", err)
			}

			// The restored bytes land everywhere; only the mtime policy differs.
			h.assertFile("primary", old, tc.wantPrimary)
			h.assertFile("peer", old, tc.wantPeer)
			// Every manifest describes the file that is really on disk.
			for id, want := range map[string]time.Time{"primary": tc.wantPrimary, "peer": tc.wantPeer} {
				m := h.manifest(id)
				if m.Mtime == nil || !m.Mtime.Equal(want.UTC().Truncate(time.Microsecond)) {
					t.Fatalf("%s manifest mtime = %v, want the published %v", id, m.Mtime, want)
				}
				if m.SHA256 == nil || *m.SHA256 != oldHash {
					t.Fatalf("%s manifest hash = %v, want the restored %q", id, m.SHA256, oldHash)
				}
			}

			logs := h.logs.String()
			if tc.wantLogNode == "" {
				if strings.Contains(logs, "mtime") {
					t.Fatalf("engine logged about mtimes when it took no mtime decision:\n%s", logs)
				}
				return
			}
			if !strings.Contains(logs, `"level":"INFO"`) || !strings.Contains(logs, `"node_id":"`+tc.wantLogNode+`"`) {
				t.Fatalf("want an INFO bump logged against %q, got:\n%s", tc.wantLogNode, logs)
			}
			if strings.Contains(logs, `"level":"WARN"`) {
				t.Fatalf("a user-initiated bump must not warn:\n%s", logs)
			}
		})
	}
}

// TestRestoreVersion_PrunedSeq_NotFound asserts restoring a seq that no longer
// exists (pruned/gone) returns ErrNotFound.
func TestRestoreVersion_PrunedSeq_NotFound(t *testing.T) {
	h, _ := seedSynced(t)
	if err := h.engine.RestoreVersion(ctx(), syncID, 999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("restore unknown seq: want ErrNotFound, got %v", err)
	}
}

// TestRestoreVersion_ClearsConflict asserts restore is also a valid recovery from
// a conflicted sync: it clears conflict_at.
func TestRestoreVersion_ClearsConflict(t *testing.T) {
	h, _, _ := seedConflicted(t) // sync is paused (conflict_at set)
	old := []byte("RECOVER")
	oldHash := sha256Hex(old)
	if err := h.store.PutSaveVersion(ctx(), syncID, "primary", oldHash, old, "conflict-resolve"); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	seq := h.versions("primary")[0].Seq

	if err := h.engine.RestoreVersion(ctx(), syncID, seq); err != nil {
		t.Fatalf("restore from conflict: %v", err)
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("restore should clear conflict_at")
	}
	h.assertFileContent("peer", old)
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
