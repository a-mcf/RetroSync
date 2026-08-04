//go:build syncthing

package syncthinge2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/reach/localfs"
)

// TestMtimeCollision_ContentChangeIsInvisible is the reproduction of issue #33 —
// the candidate that implicated RetroSync rather than syncthing, and the one that
// was right.
//
// WriteAtomic stamps the published file with the SOURCE file's mtime (os.Chtimes
// before the rename) so the manifest stays consistent. Syncthing's scanner is
// stat-gated: it only rehashes a file when mtime or size changed. Save files are
// a fixed size for a given game. So a fan-out whose source mtime happens to equal
// what syncthing already has indexed for that path used to be invisible to
// syncthing — not delayed, invisible: never hashed, never propagated, reporting
// in sync forever.
//
// That requires files with pre-existing mtimes on both sides being copied between
// each other, which is precisely the "sync created over a file that already
// exists on both members" setup that stalled in production.
//
// The test drives the PRODUCTION write path — localfs.WriteAtomic, with a source
// mtime deliberately equal to the destination's current one — and asserts the
// bytes still reach the device. It is deliberately not a hand-rolled
// os.WriteFile + os.Chtimes: that would characterize syncthing's scanner (which
// will always behave this way, by design) instead of RetroSync's write path,
// which is where the fix lives. The fix is that WriteAtomic refuses to publish a
// colliding mtime, so the write is visible to the scanner again.
func TestMtimeCollision_ContentChangeIsInvisible(t *testing.T) {
	m := newMesh(t)
	ctx := context.Background()

	const (
		folder   = "folder-mtime"
		saveName = "Secret of Mana (USA).srm"
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
	serverPath := filepath.Join(m.Server.SharesDir, folder, saveName)
	waitFileContent(t, serverPath, original, 60*time.Second)

	// Whatever mtime syncthing has now indexed for the server's replica.
	fi, err := os.Stat(serverPath)
	if err != nil {
		t.Fatalf("stat server replica: %v", err)
	}
	indexedMtime := fi.ModTime()

	// THE FAN-OUT, through the production write path, carrying a source mtime
	// equal to the destination's indexed one — the collision itself.
	fs, err := localfs.New(filepath.Join(m.Server.SharesDir, folder))
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}
	published, err := fs.WriteAtomic(ctx, saveName, replacement, indexedMtime)
	if err != nil {
		t.Fatalf("fan-out WriteAtomic: %v", err)
	}
	// The write path must not have published the colliding mtime; if it did, the
	// scanner cannot possibly see the change and the rest of this test is moot.
	if !published.After(indexedMtime) {
		t.Fatalf("WriteAtomic published mtime %v, which is not newer than the indexed %v: "+
			"a colliding mtime is invisible to syncthing's stat gate (issue #33)", published, indexedMtime)
	}
	if got, err := os.Stat(serverPath); err != nil {
		t.Fatalf("stat published file: %v", err)
	} else if !got.ModTime().Equal(published) {
		t.Fatalf("on-disk mtime %v does not match the mtime WriteAtomic reported publishing (%v)",
			got.ModTime(), published)
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
				"RetroSync must never publish a save whose mtime is not strictly newer "+
				"than the one the destination already carried (published %v, destination had %v).",
				got, replacement, published, indexedMtime)
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
