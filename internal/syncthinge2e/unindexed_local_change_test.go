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

// TestUnindexedLocalChange_FanOutStillReachesDevice probes the second candidate
// explanation for issue #33.
//
// Syncthing deliberately refuses to clobber a local file whose on-disk content
// does not match its own database — it will not destroy bytes it has never
// indexed. In production, RetroArch rewrites a save whenever a game is opened
// or closed, so a device can easily hold a local modification its syncthing has
// not scanned yet. If an incoming fan-out is blocked by that, the file stalls in
// BOTH directions with no error anywhere, which is exactly the reported
// symptom.
//
// The test enters that window deliberately: device A's folder has its watcher
// off and a long rescan interval, so a local write stays unindexed until the
// test says otherwise. Then the fan-out runs.
//
// This test asserts the CURRENT behavior so a future change is visible. If it
// ever fails, that failure is the reproduction we have been missing.
func TestUnindexedLocalChange_FanOutStillReachesDevice(t *testing.T) {
	m := newMesh(t)
	ctx := context.Background()

	const (
		folderA  = "folder-unindexed"
		saveName = "Chrono Trigger (USA).srm"
	)
	initial := []byte("SAVE-INITIAL")
	localEdit := []byte("SAVE-EDITED-ON-DEVICE-NOT-YET-SCANNED")
	incoming := []byte("SAVE-FROM-THE-OTHER-DEVICE")

	if err := m.Server.addDevice(m.DeviceA); err != nil {
		t.Fatalf("server addDevice: %v", err)
	}
	if err := m.DeviceA.addDevice(m.Server); err != nil {
		t.Fatalf("device addDevice: %v", err)
	}
	if err := m.Server.shareFolder(folderA, "/data/"+folderA, 10, m.DeviceA); err != nil {
		t.Fatalf("server shareFolder: %v", err)
	}
	// Watcher OFF and a very long rescan on the device: a local write will not
	// be indexed until this test forces it.
	if err := m.DeviceA.shareFolderWatch(folderA, "/data/"+folderA, 86400, false, m.Server); err != nil {
		t.Fatalf("device shareFolder: %v", err)
	}
	m.Server.waitConnected(t, m.DeviceA, 60*time.Second)

	// Establish a shared baseline both ends agree on.
	seedOnDevice(t, m.DeviceA, folderA, saveName, initial)
	serverPath := filepath.Join(m.Server.SharesDir, folderA, saveName)
	waitFileContent(t, serverPath, initial, 60*time.Second)

	// The device now changes the file locally — an emulator writing SRAM — and
	// syncthing on that device does NOT learn about it.
	devicePath := filepath.Join(m.DeviceA.SharesDir, folderA, saveName)
	if err := os.WriteFile(devicePath, localEdit, 0o644); err != nil {
		t.Fatalf("local edit on device: %v", err)
	}

	// Meanwhile RetroSync fans out different content over the server replica.
	fs, err := localfs.New(filepath.Join(m.Server.SharesDir, folderA))
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}
	if _, err := fs.WriteAtomic(ctx, saveName, incoming, time.Now()); err != nil {
		t.Fatalf("fan-out WriteAtomic: %v", err)
	}
	if err := m.Server.scan(folderA, ""); err != nil {
		t.Fatalf("server scan: %v", err)
	}

	// Does the fan-out land on the device, or does the unindexed local change
	// block it? Either answer is worth knowing; the test records which.
	deadline := time.Now().Add(60 * time.Second)
	for {
		got, readErr := os.ReadFile(devicePath)
		if readErr == nil && string(got) == string(incoming) {
			t.Logf("fan-out overwrote the unindexed local change (no stall)")
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("fan-out did not reach device A within the deadline: %s holds %q, want %q\n"+
				"THIS IS THE ISSUE #33 REPRODUCTION — capture the vectors below", devicePath, got, incoming)
			if c, cerr := m.Server.completion(folderA, m.DeviceA.deviceID); cerr == nil {
				t.Logf("server's view of device A: completion=%.2f needBytes=%d needItems=%d",
					c.Completion, c.NeedBytes, c.NeedItems)
			}
			t.Logf("version vectors for %q in %s:", saveName, folderA)
			dumpVectors(t, folderA, saveName, m.Server, m.DeviceA)
			listConflictCopies(t, filepath.Dir(devicePath))
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// listConflictCopies reports any .sync-conflict-* siblings, which distinguish
// "syncthing resolved this as a conflict" from "syncthing did nothing at all".
func listConflictCopies(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Logf("  (could not list %s: %v)", dir, err)
		return
	}
	found := false
	for _, e := range entries {
		t.Logf("  dir entry: %s", e.Name())
		found = true
	}
	if !found {
		t.Logf("  (directory empty)")
	}
}
