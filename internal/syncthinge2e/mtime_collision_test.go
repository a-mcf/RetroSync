//go:build syncthing

package syncthinge2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/reach/resolve"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

// TestMtimeCollision_ContentChangeIsInvisible is the reproduction of issue #33 —
// the candidate that implicated RetroSync rather than syncthing, and the one that
// was right.
//
// A fan-out stamps the published file with the SOURCE file's mtime so the copy
// stays mtime-faithful. Syncthing's scanner is stat-gated: it only rehashes a
// file when mtime or size changed. Save files are a fixed size for a given game.
// So a fan-out whose source mtime happens to EQUAL what syncthing already has
// indexed for that path is invisible to syncthing — not delayed, invisible: never
// hashed, never propagated, reporting in sync forever.
//
// That requires files with pre-existing mtimes on both sides being copied between
// each other, which is precisely the "sync created over a file that already
// exists on both members" setup that stalled in production.
//
// WHERE THE FIX LIVES (slice 38): not in localfs — an adapter cannot know whether
// it is serving a human's one-time choice or the ten-thousandth routine mirror,
// and RetroSync alters a save's mtime only in the former case. It lives in the
// engine's fan-out, gated on user intent, and the case exercised here is a sync's
// INITIATION (LastSynced == nil), which is exactly the production scenario: a
// human just created this sync over saves that already existed on both sides.
//
// So this test drives the real engine over the real localfs adapter over a real
// syncthing share, and asserts the bytes reach the DEVICE — the only assertion
// that can tell "wrote the file" from "syncthing propagated the file".
func TestMtimeCollision_ContentChangeIsInvisible(t *testing.T) {
	m := newMesh(t)
	ctx := context.Background()

	const (
		folder   = "folder-mtime"
		saveName = "Secret of Mana (USA).srm"
		syncID   = "mtime-collision"
	)
	original := []byte("SAVE-ORIGINAL-CONTENT")
	replacement := []byte("SAVE-REPLACEMENT-XXXX") // same length: size cannot betray it

	if len(original) != len(replacement) {
		t.Fatalf("test bug: contents must be the same size to exercise the stat gate")
	}

	if err := m.Server.addDevice(m.DeviceA); err != nil {
		t.Fatalf("server addDevice: %v", err)
	}
	if err := m.DeviceA.addDevice(m.Server); err != nil {
		t.Fatalf("device addDevice: %v", err)
	}
	if err := m.Server.shareFolder(folder, "/data/"+folder, 10, m.DeviceA); err != nil {
		t.Fatalf("server shareFolder: %v", err)
	}
	if err := m.DeviceA.shareFolder(folder, "/data/"+folder, 10, m.Server); err != nil {
		t.Fatalf("device shareFolder: %v", err)
	}
	m.Server.waitConnected(t, m.DeviceA, 60*time.Second)

	// Baseline both ends agree on, with a known mtime.
	seedOnDevice(t, m.DeviceA, folder, saveName, original)
	shareRoot := filepath.Join(m.Server.SharesDir, folder)
	serverPath := filepath.Join(shareRoot, saveName)
	waitFileContent(t, serverPath, original, 60*time.Second)

	// Whatever mtime syncthing has now indexed for the server's replica.
	fi, err := os.Stat(serverPath)
	if err != nil {
		t.Fatalf("stat server replica: %v", err)
	}
	indexedMtime := fi.ModTime()

	// The OTHER member of the sync: a second node holding the save that is about
	// to win, carrying EXACTLY the indexed mtime — the collision itself. (In
	// production this is another device's share replica whose copy happens to
	// carry the same mtime; here a plain directory keeps the test to one mesh.)
	srcRoot := t.TempDir()
	srcPath := filepath.Join(srcRoot, saveName)
	if err := os.WriteFile(srcPath, replacement, 0o644); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	if err := os.Chtimes(srcPath, indexedMtime, indexedMtime); err != nil {
		t.Fatalf("chtimes source: %v", err)
	}

	// A sync over the two of them that has NEVER synced (last_synced == nil): its
	// first fan-out is the user-initiated initiation the fix is gated on.
	st := memory.New()
	if err := st.CreateSync(ctx, store.Sync{ID: syncID, Game: "Secret of Mana", Name: "collision"}); err != nil {
		t.Fatalf("CreateSync: %v", err)
	}
	for _, n := range []struct{ id, root string }{{"src", srcRoot}, {"share", shareRoot}} {
		if err := st.CreateNode(ctx, store.Node{
			ID: n.id, Display: n.id, Kind: store.KindGeneric,
			Reach: store.ReachSyncthingShare, ReachConfig: store.ReachConfig{Path: n.root},
		}); err != nil {
			t.Fatalf("CreateNode %s: %v", n.id, err)
		}
		if err := st.SetSyncMember(ctx, store.SyncMember{SyncID: syncID, NodeID: n.id, Path: saveName}); err != nil {
			t.Fatalf("SetSyncMember %s: %v", n.id, err)
		}
	}
	// The share member is recorded as already-known (its manifest describes the
	// file on disk), so it is UNCHANGED; the src member has no manifest row, so it
	// is the single changed member and therefore the fan-out source.
	origHash := sha256.Sum256(original)
	hexHash := hex.EncodeToString(origHash[:])
	size := int64(len(original))
	checked := time.Now()
	mt := indexedMtime.UTC().Truncate(time.Microsecond)
	if err := st.SetManifest(ctx, store.ManifestEntry{
		SyncID: syncID, NodeID: "share",
		Mtime: &mt, Size: &size, SHA256: &hexHash, LastChecked: &checked,
	}); err != nil {
		t.Fatalf("SetManifest: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng := engine.New(st, resolve.ResolveReach, time.Now, engine.WithLogger(logger))

	// THE FAN-OUT, through the production engine + write path, with a source mtime
	// equal to the destination's indexed one.
	if err := eng.Poll(ctx, syncID); err != nil {
		t.Fatalf("Poll (initiation fan-out): %v", err)
	}

	published, err := os.Stat(serverPath)
	if err != nil {
		t.Fatalf("stat published file: %v", err)
	}
	if got, err := os.ReadFile(serverPath); err != nil {
		t.Fatalf("read published file: %v", err)
	} else if string(got) != string(replacement) {
		t.Fatalf("share replica holds %q after the fan-out, want %q", got, replacement)
	}
	// The engine must not have published the colliding mtime on this user-initiated
	// write; if it did, the scanner cannot possibly see the change and the rest of
	// this test is moot.
	if !published.ModTime().After(indexedMtime) {
		t.Fatalf("fan-out published mtime %v, which is not newer than the indexed %v: "+
			"a colliding mtime is invisible to syncthing's stat gate (issue #33)",
			published.ModTime(), indexedMtime)
	}
	// The manifest must describe the file that is actually on disk, or the next
	// poll re-detects it forever.
	me, err := st.GetManifest(ctx, syncID, "share")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if me.Mtime == nil || !me.Mtime.Equal(published.ModTime().UTC().Truncate(time.Microsecond)) {
		t.Fatalf("manifest mtime = %v, want the published %v", me.Mtime, published.ModTime())
	}
	if err := m.Server.scan(folder, ""); err != nil {
		t.Fatalf("server scan: %v", err)
	}

	devicePath := filepath.Join(m.DeviceA.SharesDir, folder, saveName)
	deadline := time.Now().Add(45 * time.Second)
	for {
		got, readErr := os.ReadFile(devicePath)
		if readErr == nil && string(got) == string(replacement) {
			return // delivered: the stat gate saw the change
		}
		if time.Now().After(deadline) {
			t.Errorf("content change with a colliding SOURCE mtime never reached the device.\n"+
				"  device holds: %q\n  want:         %q\n"+
				"REPRODUCTION of the issue #33 stall: syncthing's stat gate (mtime+size) "+
				"never saw a change, so the new bytes were never hashed or propagated. "+
				"On a user-initiated fan-out RetroSync must publish one microsecond past "+
				"the destination's mtime (published %v, destination had %v).",
				got, replacement, published.ModTime(), indexedMtime)
			if c, cerr := m.Server.completion(folder, m.DeviceA.deviceID); cerr == nil {
				t.Logf("server's view of device A: completion=%.2f needBytes=%d needItems=%d",
					c.Completion, c.NeedBytes, c.NeedItems)
			}
			t.Logf("version vectors for %q in %s:", saveName, folder)
			dumpVectors(t, folder, saveName, m.Server, m.DeviceA)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}
