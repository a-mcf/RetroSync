package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/auth"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

const testPassword = "hunter2-but-longer"

// newTestServer builds a Server over a memory store seeded with one user
// (bob/admin) plus a node, game, and a sync sm-bob with a member + manifest +
// active binding on bob-deck, so the dashboard and JSON endpoints have data to
// render.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	st := memory.New()
	ctx := context.Background()

	hash, err := auth.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := st.CreateUser(ctx, store.User{ID: "bob", Display: "Bob", PwHash: hash, Role: store.RoleAdmin}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	owner := "bob"
	seen := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	if err := st.CreateNode(ctx, store.Node{
		ID: "bob-deck", OwnerUserID: &owner, Display: "Bob's Deck",
		Kind: store.KindDeck, Reach: store.ReachSyncthingShare,
		ReachConfig: store.ReachConfig{Path: "/srv/saves"}, LastSeenAt: &seen,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := st.CreateGame(ctx, store.Game{ID: "super-metroid", Display: "Super Metroid", System: "snes"}); err != nil {
		t.Fatalf("create game: %v", err)
	}
	// The sync (the PLAY + registry unit): member + manifest + active binding on bob-deck.
	if err := st.CreateSync(ctx, store.Sync{ID: "sm-bob", GameID: "super-metroid", Name: "Bob's stream"}); err != nil {
		t.Fatalf("create sync: %v", err)
	}
	if err := st.SetSyncMember(ctx, store.SyncMember{SyncID: "sm-bob", NodeID: "bob-deck", Path: "sm.srm"}); err != nil {
		t.Fatalf("set member: %v", err)
	}
	mtime := time.Date(2026, 6, 21, 11, 30, 0, 0, time.UTC)
	if err := st.SetManifest(ctx, store.ManifestEntry{SyncID: "sm-bob", NodeID: "bob-deck", Mtime: &mtime}); err != nil {
		t.Fatalf("set manifest: %v", err)
	}
	if err := st.CreateBinding(ctx, store.ActiveBinding{
		SyncID: "sm-bob", PrimaryNode: "bob-deck",
		StartedAt: time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC),
		Direction: "from-primary",
	}); err != nil {
		t.Fatalf("create binding: %v", err)
	}

	srv, err := New(st, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// login posts good credentials and returns the session cookie.
func login(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := postLogin(h, "bob", testPassword)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login: status = %d, want 303", rec.Code)
	}
	c := sessionCookie(rec.Result().Cookies())
	if c == nil {
		t.Fatal("login: no session cookie set")
	}
	return c
}

func postLogin(h http.Handler, user, pass string) *httptest.ResponseRecorder {
	form := url.Values{"username": {user}, "password": {pass}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(cookies []*http.Cookie) *http.Cookie {
	for _, c := range cookies {
		if c.Name == sessionCookieName && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestLoginBadCredsNoSession(t *testing.T) {
	h := newTestServer(t).Handler()
	for _, tc := range []struct{ name, user, pass string }{
		{"wrong-password", "bob", "nope"},
		{"unknown-user", "nobody", testPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postLogin(h, tc.user, tc.pass)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if c := sessionCookie(rec.Result().Cookies()); c != nil {
				t.Fatalf("bad creds set a session cookie: %v", c)
			}
		})
	}
}

func TestLoginGoodCredsSetsCookieAndRedirects(t *testing.T) {
	h := newTestServer(t).Handler()
	rec := postLogin(h, "bob", testPassword)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("redirect = %q, want /", loc)
	}
	c := sessionCookie(rec.Result().Cookies())
	if c == nil {
		t.Fatal("no session cookie set on good login")
	}
	if !c.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if !c.Secure {
		t.Error("session cookie is not Secure")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", c.SameSite)
	}
}

func TestRequireAuthBlocksDashboard(t *testing.T) {
	h := newTestServer(t).Handler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 redirect", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("redirect = %q, want /login", loc)
	}
}

func TestRequireAuthBlocksAPI401(t *testing.T) {
	h := newTestServer(t).Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestLogoutClearsSession(t *testing.T) {
	h := newTestServer(t).Handler()
	c := login(t, h)

	// Logout.
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", rec.Code)
	}

	// The old cookie must no longer grant access.
	req2 := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req2.AddCookie(c)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("after logout /api/status = %d, want 401", rec2.Code)
	}
}

func TestSessionTokenRegeneratesOnLogin(t *testing.T) {
	h := newTestServer(t).Handler()
	c1 := login(t, h)
	c2 := login(t, h)
	if c1.Value == c2.Value {
		t.Fatal("session token did not change between logins (fixation risk)")
	}
}

func TestDashboardRendersSeededData(t *testing.T) {
	h := newTestServer(t).Handler()
	c := login(t, h)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Super Metroid", "bob-deck", "Bob"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard body missing %q", want)
		}
	}
}

func TestAPIStatusShape(t *testing.T) {
	h := newTestServer(t).Handler()
	c := login(t, h)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want json", ct)
	}

	var got statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if len(got.Active) != 1 {
		t.Fatalf("active len = %d, want 1", len(got.Active))
	}
	a := got.Active[0]
	if a.SyncID != "sm-bob" || a.PrimaryNode != "bob-deck" || a.Conflict {
		t.Errorf("active[0] = %+v, unexpected", a)
	}
	if a.Since == "" {
		t.Error("active[0].since empty")
	}
	if len(got.Nodes) != 1 {
		t.Fatalf("nodes len = %d, want 1", len(got.Nodes))
	}
	n := got.Nodes[0]
	if n.ID != "bob-deck" || !n.Reachable || n.LastSeen == nil {
		t.Errorf("nodes[0] = %+v, unexpected", n)
	}
}

func TestAPIGamesSyncsShape(t *testing.T) {
	// /api/games lists each game's syncs (id, name, active-binding state, and
	// members with node + path + last-known manifest mtime). The seeded fixture
	// has game super-metroid -> sync sm-bob (active on bob-deck) with one member
	// (bob-deck, mtime present). We add a second member with no manifest to assert
	// mtime is present-or-null per member.
	srv := newTestServer(t)
	ctx := context.Background()
	if err := srv.store.CreateNode(ctx, store.Node{
		ID: "alice-deck", Display: "Alice's Deck",
		Kind: store.KindDeck, Reach: store.ReachSyncthingShare,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	// A second member of sm-bob with no manifest -> its mtime must serialize null.
	if err := srv.store.SetSyncMember(ctx, store.SyncMember{SyncID: "sm-bob", NodeID: "alice-deck", Path: "alice-sm.srm"}); err != nil {
		t.Fatalf("set member: %v", err)
	}

	h := srv.Handler()
	c := login(t, h)
	req := httptest.NewRequest(http.MethodGet, "/api/games", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var games []gameResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &games); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if len(games) != 1 {
		t.Fatalf("games len = %d, want 1", len(games))
	}
	g := games[0]
	if g.ID != "super-metroid" || g.Display != "Super Metroid" {
		t.Errorf("game = %+v, unexpected", g)
	}
	if len(g.Syncs) != 1 {
		t.Fatalf("syncs len = %d, want 1", len(g.Syncs))
	}
	sy := g.Syncs[0]
	if sy.ID != "sm-bob" || sy.Name != "Bob's stream" {
		t.Errorf("sync = %+v, unexpected", sy)
	}
	if sy.Active == nil || sy.Active.PrimaryNode != "bob-deck" || sy.Active.Conflict {
		t.Errorf("sync.active = %+v, want active on bob-deck, no conflict", sy.Active)
	}
	if len(sy.Members) != 2 {
		t.Fatalf("members len = %d, want 2", len(sy.Members))
	}
	// Re-key by node for assertions (members are sorted by node id).
	byNode := map[string]syncMemberOutput{}
	for _, m := range sy.Members {
		byNode[m.NodeID] = m
	}
	bob, ok := byNode["bob-deck"]
	if !ok || bob.Path != "sm.srm" || bob.Mtime == nil {
		t.Errorf("bob-deck member = %+v, want path sm.srm + present mtime", bob)
	}
	alice, ok := byNode["alice-deck"]
	if !ok || alice.Path != "alice-sm.srm" || alice.Mtime != nil {
		t.Errorf("alice-deck member = %+v, want path alice-sm.srm + null mtime", alice)
	}

	// The mtime KEY must always be present in the raw JSON (present-or-null).
	var raw []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	for _, rg := range raw {
		syncs, _ := rg["syncs"].([]any)
		for _, sv := range syncs {
			sm, _ := sv.(map[string]any)
			members, _ := sm["members"].([]any)
			for _, mv := range members {
				m, _ := mv.(map[string]any)
				if _, ok := m["mtime"]; !ok {
					t.Fatalf("member object missing mtime key: %v", m)
				}
			}
		}
	}
}

func TestHealthzNoAuth(t *testing.T) {
	h := newTestServer(t).Handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
}

func TestStaticAssetsServed(t *testing.T) {
	h := newTestServer(t).Handler()
	for _, path := range []string{"/static/app.css", "/static/htmx.min.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s served empty body", path)
		}
	}
}
