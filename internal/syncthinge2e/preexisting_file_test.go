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

// TestPreExistingFile_FanOutReachesDevice reproduces the setup that stalled
// twice in production (issue #33): a sync is created over a save that ALREADY
// EXISTS on both members, each copy having originated independently on its own
// device. RetroSync then writes one side's content over the other's share
// replica, exactly as a fan-out does.
//
// The question this test answers is the one no other test in the suite can:
// does that write actually reach the DEVICE, or does it stall in syncthing
// while every status endpoint reports success?
//
// It deliberately drives the real localfs WriteAtomic rather than writing the
// file itself, so the temp-file-plus-rename publish — the thing syncthing
// observes — is the code under test.
func TestPreExistingFile_FanOutReachesDevice(t *testing.T) {
	m := newMesh(t)
	ctx := context.Background()

	const (
		folderA  = "folder-a"
		folderB  = "folder-b"
		saveName = "ActRaiser (USA).srm"
	)
	deviceContent := []byte("SAVE-FROM-DEVICE-A")
	incomingContent := []byte("SAVE-FROM-DEVICE-B")

	// Server shares folder-a with device A and folder-b with device B — the
	// real topology: one syncthing folder per device, all meeting on the
	// machine RetroSync runs on.
	for _, pair := range []struct {
		peer *stInstance
		id   string
	}{{m.DeviceA, folderA}, {m.DeviceB, folderB}} {
		if err := m.Server.addDevice(pair.peer); err != nil {
			t.Fatalf("server addDevice %s: %v", pair.peer.Name, err)
		}
		if err := pair.peer.addDevice(m.Server); err != nil {
			t.Fatalf("%s addDevice server: %v", pair.peer.Name, err)
		}
		if err := m.Server.shareFolder(pair.id, "/data/"+pair.id, 10, pair.peer); err != nil {
			t.Fatalf("server shareFolder %s: %v", pair.id, err)
		}
		if err := pair.peer.shareFolder(pair.id, "/data/"+pair.id, 10, m.Server); err != nil {
			t.Fatalf("%s shareFolder %s: %v", pair.peer.Name, pair.id, err)
		}
	}
	m.Server.waitConnected(t, m.DeviceA, 60*time.Second)
	m.Server.waitConnected(t, m.DeviceB, 60*time.Second)

	// Each device independently creates its own copy of the same save — this is
	// what "played the game on both devices" produces, and it is the state
	// discovery is designed to find.
	seedOnDevice(t, m.DeviceA, folderA, saveName, deviceContent)
	seedOnDevice(t, m.DeviceB, folderB, saveName, incomingContent)

	// Both copies must reach the server before RetroSync would ever see them.
	serverAPath := filepath.Join(m.Server.SharesDir, folderA, saveName)
	serverBPath := filepath.Join(m.Server.SharesDir, folderB, saveName)
	waitFileContent(t, serverAPath, deviceContent, 60*time.Second)
	waitFileContent(t, serverBPath, incomingContent, 60*time.Second)

	// THE FAN-OUT. RetroSync decides device B's copy is the one to keep and
	// publishes it over device A's share replica, through the production write
	// path.
	fs, err := localfs.New(filepath.Join(m.Server.SharesDir, folderA))
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}
	if _, err := fs.WriteAtomic(ctx, saveName, incomingContent, time.Now()); err != nil {
		t.Fatalf("fan-out WriteAtomic: %v", err)
	}
	// RetroSync writes through a mount syncthing's watcher may not observe, so
	// the production fix nudges syncthing explicitly. Do the same here rather
	// than waiting out a rescan interval.
	if err := m.Server.scan(folderA, ""); err != nil {
		t.Fatalf("server scan %s: %v", folderA, err)
	}

	// The assertion that matters: the bytes are on the DEVICE, not merely in
	// the share replica the fan-out wrote.
	devicePath := filepath.Join(m.DeviceA.SharesDir, folderA, saveName)
	deadline := time.Now().Add(90 * time.Second)
	for {
		got, err := os.ReadFile(devicePath)
		if err == nil && string(got) == string(incomingContent) {
			return // delivered
		}
		if time.Now().After(deadline) {
			t.Errorf("fan-out never reached device A: %s still holds %q, want %q", devicePath, got, incomingContent)
			if c, cerr := m.Server.completion(folderA, m.DeviceA.deviceID); cerr == nil {
				t.Logf("server's view of device A: completion=%.2f needBytes=%d needItems=%d",
					c.Completion, c.NeedBytes, c.NeedItems)
			}
			t.Logf("version vectors for %q in %s:", saveName, folderA)
			dumpVectors(t, folderA, saveName, m.Server, m.DeviceA)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// seedOnDevice writes content into a device's folder and forces a scan so the
// device indexes it immediately instead of on its rescan interval.
func seedOnDevice(t *testing.T, inst *stInstance, folder, name string, content []byte) {
	t.Helper()
	dir := filepath.Join(inst.SharesDir, folder)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("%s mkdir %s: %v", inst.Name, dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
		t.Fatalf("%s seed %s: %v", inst.Name, name, err)
	}
	if err := inst.scan(folder, ""); err != nil {
		t.Fatalf("%s scan %s: %v", inst.Name, folder, err)
	}
}
