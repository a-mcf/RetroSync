package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/auth"
	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

// stubActioner records ResolveConflict / SmokeTest / BrowseNode calls and
// returns programmable errors / canned NodeStates, so handler tests assert on
// the call (or its absence) without an engine. Under auto-mirror there are no
// Activate/Deactivate methods — the only state-changing engine action the web
// layer drives is ResolveConflict.
type stubActioner struct {
	mu sync.Mutex

	// resolveErr is returned by ResolveConflict; resolved records each call.
	resolveErr error
	resolved   []resolveCall

	// restoreErr is returned by RestoreVersion; restored records each (syncID, seq)
	// call so tests assert the handler passed the PATH syncID it authorized.
	restoreErr error
	restored   []restoreCall
	// nodeStates is the canned slice returned by NodeStates.
	nodeStates []engine.NodeState

	// smokeErr is returned by SmokeTest; smokeTested records each probed node id.
	smokeErr    error
	smokeTested []string

	// browseErr / browseEntries program BrowseNode; browsed records each
	// (node,path) call so picker tests assert the handler reached the engine with
	// the right relative path.
	browseErr     error
	browseEntries []engine.DirEntry
	browsed       []browseCall

	// serverBrowseErr / serverBrowseEntries program BrowseServer; serverBrowsed
	// records each rel-path call so the folder-picker tests assert the handler
	// reached the engine with the right server-relative path.
	serverBrowseErr     error
	serverBrowseEntries []engine.DirEntry
	serverBrowsed       []string

	// discoverErr / discoverGames program DiscoverGames; discoverCalls counts the
	// calls so discovery tests assert the handler reached the engine (and that the
	// scan ran exactly once per page render).
	discoverErr   error
	discoverGames []engine.DiscoveredGame
	discoverCount int
}

type browseCall struct {
	nodeID, relPath string
}

type resolveCall struct {
	syncID, winnerNodeID string
}

type restoreCall struct {
	syncID string
	seq    int64
}

func (s *stubActioner) ResolveConflict(_ context.Context, syncID, winnerNodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved = append(s.resolved, resolveCall{syncID, winnerNodeID})
	return s.resolveErr
}

func (s *stubActioner) RestoreVersion(_ context.Context, syncID string, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restored = append(s.restored, restoreCall{syncID, seq})
	return s.restoreErr
}

func (s *stubActioner) restoreCalls() []restoreCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]restoreCall(nil), s.restored...)
}

func (s *stubActioner) NodeStates(_ context.Context, _ string) ([]engine.NodeState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]engine.NodeState(nil), s.nodeStates...), nil
}

func (s *stubActioner) SmokeTest(_ context.Context, nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.smokeTested = append(s.smokeTested, nodeID)
	return s.smokeErr
}

func (s *stubActioner) smokeTestedNodes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.smokeTested...)
}

func (s *stubActioner) BrowseNode(_ context.Context, nodeID, relPath string) ([]engine.DirEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.browsed = append(s.browsed, browseCall{nodeID, relPath})
	if s.browseErr != nil {
		return nil, s.browseErr
	}
	return append([]engine.DirEntry(nil), s.browseEntries...), nil
}

func (s *stubActioner) browseCalls() []browseCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]browseCall(nil), s.browsed...)
}

func (s *stubActioner) BrowseServer(_ context.Context, relPath string) ([]engine.DirEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serverBrowsed = append(s.serverBrowsed, relPath)
	if s.serverBrowseErr != nil {
		return nil, s.serverBrowseErr
	}
	return append([]engine.DirEntry(nil), s.serverBrowseEntries...), nil
}

func (s *stubActioner) serverBrowseCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.serverBrowsed...)
}

func (s *stubActioner) DiscoverGames(_ context.Context) ([]engine.DiscoveredGame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discoverCount++
	if s.discoverErr != nil {
		return nil, s.discoverErr
	}
	return append([]engine.DiscoveredGame(nil), s.discoverGames...), nil
}

func (s *stubActioner) resolveCalls() []resolveCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]resolveCall(nil), s.resolved...)
}

// actionFixture builds a server with an injected stubActioner over a memory
// store seeded with:
//   - admin user "bob" owning "bob-deck"
//   - regular user "carol" owning "carol-deck"
//   - shared node "mister" (no owner)
//   - game "super-metroid" with sync "sm-bob" whose members (+ manifests) are
//     bob-deck and carol-deck.
type actionFixture struct {
	srv   *Server
	store store.Store
	act   *stubActioner
}

func newActionFixture(t *testing.T) *actionFixture {
	t.Helper()
	st := memory.New()
	ctx := context.Background()

	hash, err := auth.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	mustUser := func(id string, role store.Role) {
		if err := st.CreateUser(ctx, store.User{ID: id, Display: id, PwHash: hash, Role: role}); err != nil {
			t.Fatalf("create user %s: %v", id, err)
		}
	}
	mustUser("bob", store.RoleAdmin)
	mustUser("carol", store.RoleUser)

	bob, carol := "bob", "carol"
	mustNode := func(id string, owner *string) {
		if err := st.CreateNode(ctx, store.Node{
			ID: id, OwnerUserID: owner, Display: id,
			Kind: store.KindDeck, Reach: store.ReachSyncthingShare,
		}); err != nil {
			t.Fatalf("create node %s: %v", id, err)
		}
	}
	mustNode("bob-deck", &bob)
	mustNode("carol-deck", &carol)
	mustNode("mister", nil)

	if err := st.CreateSync(ctx, store.Sync{ID: "sm-bob", Game: "Super Metroid", Name: "Bob's stream"}); err != nil {
		t.Fatalf("create sync: %v", err)
	}
	mtime := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	for _, n := range []string{"bob-deck", "carol-deck"} {
		if err := st.SetSyncMember(ctx, store.SyncMember{SyncID: "sm-bob", NodeID: n, Path: "sm.srm"}); err != nil {
			t.Fatalf("set member %s: %v", n, err)
		}
		if err := st.SetManifest(ctx, store.ManifestEntry{SyncID: "sm-bob", NodeID: n, Mtime: &mtime}); err != nil {
			t.Fatalf("set manifest %s: %v", n, err)
		}
	}

	act := &stubActioner{}
	srv, err := New(st, Options{Actioner: act, ShareRoot: "/shares"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &actionFixture{srv: srv, store: st, act: act}
}

// loginAs logs a user in and returns (cookie, csrfToken).
func loginAs(t *testing.T, f *actionFixture, user string) (*http.Cookie, string) {
	t.Helper()
	h := f.srv.Handler()
	form := url.Values{"username": {user}, "password": {testPassword}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login %s: status = %d, want 303", user, rec.Code)
	}
	c := sessionCookie(rec.Result().Cookies())
	if c == nil {
		t.Fatalf("login %s: no session cookie", user)
	}
	csrf, ok := f.srv.sessions.csrfToken(c.Value)
	if !ok || csrf == "" {
		t.Fatalf("login %s: no csrf token minted", user)
	}
	return c, csrf
}

// postForm sends an authenticated form POST with the given CSRF token (in the
// X-CSRF-Token header) and form fields.
func postForm(t *testing.T, f *actionFixture, c *http.Cookie, csrf, path string, fields url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(fields.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c != nil {
		req.AddCookie(c)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}
