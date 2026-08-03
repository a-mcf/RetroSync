//go:build syncthing

package syncthinge2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMtimeCollision_ContentChangeIsInvisible probes the third candidate for
// issue #33, and the first one that implicates RetroSync rather than syncthing.
//
// WriteAtomic stamps the published file with the SOURCE file's mtime
// (localfs.go, os.Chtimes before the rename) so the manifest stays consistent.
// Syncthing's scanner is stat-gated: it only rehashes a file when mtime or size
// changed. Save files are a fixed size for a given game. So if a fan-out ever
// writes content carrying an mtime that matches what syncthing already has
// indexed for that path, the new content is invisible to syncthing — not
// delayed, invisible. It will never be hashed, never propagate, and the file
// will report as in sync forever.
//
// That requires files with pre-existing mtimes on both sides being copied
// between each other, which is precisely the "sync created over a file that
// already exists on both members" setup that stalls in production.
//
// This test writes new content while restoring the previously indexed mtime and
// asserts the change still reaches the device. If it FAILS, the stall is
// reproduced and the cause is RetroSync's mtime preservation, not syncthing.
func TestMtimeCollision_ContentChangeIsInvisible(t *testing.T) {
	m := newMesh(t)

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

	// A fan-out that happens to carry a source mtime equal to the indexed one.
	// Written the same way WriteAtomic publishes — new content, then the mtime
	// restored — so syncthing sees identical stat metadata over new bytes.
	if err := os.WriteFile(serverPath, replacement, 0o644); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := os.Chtimes(serverPath, indexedMtime, indexedMtime); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}
	if err := m.Server.scan(folder, ""); err != nil {
		t.Fatalf("server scan: %v", err)
	}

	devicePath := filepath.Join(m.DeviceA.SharesDir, folder, saveName)
	deadline := time.Now().Add(45 * time.Second)
	for {
		got, readErr := os.ReadFile(devicePath)
		if readErr == nil && string(got) == string(replacement) {
			return // syncthing noticed despite the identical stat; no bug here
		}
		if time.Now().After(deadline) {
			t.Errorf("content change with a colliding mtime never reached the device.\n"+
				"  device holds: %q\n  want:         %q\n"+
				"REPRODUCTION of the issue #33 stall: syncthing's stat gate (mtime+size) "+
				"never saw a change, so the new bytes were never hashed or propagated. "+
				"RetroSync stamps published saves with the SOURCE file's mtime, so any "+
				"fan-out whose source mtime matches the destination's indexed mtime is "+
				"silently invisible.", got, replacement)
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
