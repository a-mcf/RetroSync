//go:build syncthing

// Package syncthinge2e holds end-to-end tests that run RetroSync's write path
// against a REAL syncthing mesh, rather than against localfs or fakereach.
//
// Everything else in the suite stops at the share boundary: it verifies that
// RetroSync wrote the right bytes to a directory. That is exactly the boundary
// where the failures we actually hit in production live — a save can be written
// correctly to a share replica and still never reach the device, with every
// syncthing status endpoint reporting success. Those bugs are invisible to CI
// by construction unless syncthing is in the loop.
//
// Run with: make test-syncthing
package syncthinge2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stInstance is one syncthing daemon in the test mesh. Addr is reachable from
// wherever the test runs; Alias is how OTHER instances address it (container
// name on the private podman network).
type stInstance struct {
	Name   string
	Addr   string // base URL, e.g. http://127.0.0.1:18384
	APIKey string
	Alias  string // container-network name peers dial
	// SharesDir is the host path of this instance's data tree as the TEST
	// process sees it. Only meaningful for instances whose tree is mounted into
	// the test container (the server always is).
	SharesDir string
	deviceID  string
}

// mesh is the set of instances under test.
type mesh struct {
	Server  *stInstance
	DeviceA *stInstance
	DeviceB *stInstance
}

// newMesh reads the environment the Makefile target exports. It skips (rather
// than fails) when the mesh is absent so a bare `go test -tags syncthing` is
// harmless.
func newMesh(t *testing.T) *mesh {
	t.Helper()
	root := os.Getenv("ST_ROOT")
	if root == "" {
		t.Skip("ST_ROOT not set; run via `make test-syncthing`")
	}
	m := &mesh{
		Server:  &stInstance{Name: "st-server", Addr: envOr("ST_SERVER_ADDR", "http://127.0.0.1:18384"), APIKey: "serverkey", Alias: "st-server", SharesDir: filepath.Join(root, "st-server", "data")},
		DeviceA: &stInstance{Name: "st-device-a", Addr: envOr("ST_DEVICE_A_ADDR", "http://127.0.0.1:18385"), APIKey: "deviceakey", Alias: "st-device-a", SharesDir: filepath.Join(root, "st-device-a", "data")},
		DeviceB: &stInstance{Name: "st-device-b", Addr: envOr("ST_DEVICE_B_ADDR", "http://127.0.0.1:18386"), APIKey: "devicebkey", Alias: "st-device-b", SharesDir: filepath.Join(root, "st-device-b", "data")},
	}
	for _, inst := range []*stInstance{m.Server, m.DeviceA, m.DeviceB} {
		id, err := inst.myID()
		if err != nil {
			t.Fatalf("%s: read device id: %v", inst.Name, err)
		}
		inst.deviceID = id
	}
	return m
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// --- REST plumbing -------------------------------------------------------

func (s *stInstance) do(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.Addr+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", s.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (s *stInstance) myID() (string, error) {
	out, err := s.do(http.MethodGet, "/rest/system/status", nil)
	if err != nil {
		return "", err
	}
	var v struct {
		MyID string `json:"myID"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", err
	}
	if v.MyID == "" {
		return "", fmt.Errorf("empty myID")
	}
	return v.MyID, nil
}

// addDevice introduces peer to s, dialing it by its container-network name.
// Static addresses only: discovery is off mesh-wide.
func (s *stInstance) addDevice(peer *stInstance) error {
	_, err := s.do(http.MethodPut, "/rest/config/devices/"+peer.deviceID, map[string]any{
		"deviceID":  peer.deviceID,
		"name":      peer.Name,
		"addresses": []string{"tcp://" + peer.Alias + ":22000"},
	})
	return err
}

// shareFolder creates (or replaces) folder id on s at the given container path,
// shared with the listed peers, with the watcher enabled and a caller-chosen
// rescan interval.
func (s *stInstance) shareFolder(id, containerPath string, rescanS int, peers ...*stInstance) error {
	return s.shareFolderWatch(id, containerPath, rescanS, true, peers...)
}

// shareFolderWatch is shareFolder with explicit control over the filesystem
// watcher. Turning the watcher OFF and the rescan interval up lets a test hold
// a folder in the state that matters: local content changed on disk while
// syncthing's index still describes the old bytes. That window is invisible to
// every status endpoint and is where suspected stalls live, so a test has to be
// able to enter it deliberately rather than race for it.
func (s *stInstance) shareFolderWatch(id, containerPath string, rescanS int, watcher bool, peers ...*stInstance) error {
	devs := []map[string]any{{"deviceID": s.deviceID}}
	for _, p := range peers {
		devs = append(devs, map[string]any{"deviceID": p.deviceID})
	}
	_, err := s.do(http.MethodPut, "/rest/config/folders/"+id, map[string]any{
		"id":               id,
		"label":            id,
		"path":             containerPath,
		"type":             "sendreceive",
		"devices":          devs,
		"rescanIntervalS":  rescanS,
		"fsWatcherEnabled": watcher,
		"fsWatcherDelayS":  1,
	})
	return err
}

// scan forces a scan of sub within folder. Without this a test would be at the
// mercy of the rescan interval; with it, "has syncthing looked yet?" becomes
// something the test controls rather than waits on.
func (s *stInstance) scan(folder, sub string) error {
	p := "/rest/db/scan?folder=" + folder
	if sub != "" {
		p += "&sub=" + sub
	}
	_, err := s.do(http.MethodPost, p, nil)
	return err
}

// fileInfo is the slice of /rest/db/file we care about: the version vector is
// the evidence for whether two copies are concurrent or one descends from the
// other.
type fileInfo struct {
	Global struct {
		Deleted    bool     `json:"deleted"`
		ModifiedBy string   `json:"modifiedBy"`
		Version    []string `json:"version"`
		Size       int64    `json:"size"`
	} `json:"global"`
	Local struct {
		Deleted    bool     `json:"deleted"`
		ModifiedBy string   `json:"modifiedBy"`
		Version    []string `json:"version"`
		Size       int64    `json:"size"`
	} `json:"local"`
}

func (s *stInstance) fileInfo(folder, file string) (*fileInfo, error) {
	out, err := s.do(http.MethodGet, "/rest/db/file?folder="+folder+"&file="+urlEscape(file), nil)
	if err != nil {
		return nil, err
	}
	var fi fileInfo
	if err := json.Unmarshal(out, &fi); err != nil {
		return nil, err
	}
	return &fi, nil
}

func urlEscape(s string) string {
	r := strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29", "#", "%23", "&", "%26", "+", "%2B")
	return r.Replace(s)
}

type completion struct {
	Completion float64 `json:"completion"`
	NeedBytes  int64   `json:"needBytes"`
	NeedItems  int64   `json:"needItems"`
}

func (s *stInstance) completion(folder, deviceID string) (*completion, error) {
	out, err := s.do(http.MethodGet, "/rest/db/completion?folder="+folder+"&device="+deviceID, nil)
	if err != nil {
		return nil, err
	}
	var c completion
	if err := json.Unmarshal(out, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// --- waiting helpers -----------------------------------------------------

// waitConnected blocks until s reports a live connection to peer.
func (s *stInstance) waitConnected(t *testing.T, peer *stInstance, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		out, err := s.do(http.MethodGet, "/rest/system/connections", nil)
		if err == nil {
			var v struct {
				Connections map[string]struct {
					Connected bool `json:"connected"`
				} `json:"connections"`
			}
			if json.Unmarshal(out, &v) == nil {
				if c, ok := v.Connections[peer.deviceID]; ok && c.Connected {
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s never connected to %s within %s", s.Name, peer.Name, within)
}

// waitFileContent polls a path on disk until it holds want, and reports what it
// actually saw on timeout — the diagnostic that matters is "what was there
// instead", not merely that the wait expired.
func waitFileContent(t *testing.T, path string, want []byte, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last []byte
	var lastErr error
	for time.Now().Before(deadline) {
		got, err := os.ReadFile(path)
		if err == nil {
			if bytes.Equal(got, want) {
				return
			}
			last, lastErr = got, nil
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("waiting for %s to hold %q: never readable: %v", path, want, lastErr)
	}
	t.Fatalf("waiting for %s to hold %q: still holds %q after %s", path, want, last, within)
}

// dumpVectors prints both ends' version vectors for a file. Called on failure:
// concurrent vectors versus one descending from the other is the evidence that
// distinguishes the competing explanations for the pre-existing-file stall.
func dumpVectors(t *testing.T, folder, file string, insts ...*stInstance) {
	t.Helper()
	for _, inst := range insts {
		fi, err := inst.fileInfo(folder, file)
		if err != nil {
			t.Logf("  %-12s file info error: %v", inst.Name, err)
			continue
		}
		t.Logf("  %-12s local: version=%v modifiedBy=%s size=%d deleted=%v",
			inst.Name, fi.Local.Version, fi.Local.ModifiedBy, fi.Local.Size, fi.Local.Deleted)
		t.Logf("  %-12s global: version=%v modifiedBy=%s size=%d deleted=%v",
			"", fi.Global.Version, fi.Global.ModifiedBy, fi.Global.Size, fi.Global.Deleted)
	}
}
