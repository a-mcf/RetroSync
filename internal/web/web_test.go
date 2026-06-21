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
// (bob/admin) plus a node, game, path, binding, and manifest so the dashboard
// and JSON endpoints have data to render.
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
	if err := st.SetGamePath(ctx, store.GamePath{GameID: "super-metroid", NodeID: "bob-deck", Path: "sm.srm"}); err != nil {
		t.Fatalf("set path: %v", err)
	}
	mtime := time.Date(2026, 6, 21, 11, 30, 0, 0, time.UTC)
	if err := st.SetManifest(ctx, store.ManifestEntry{GameID: "super-metroid", NodeID: "bob-deck", Mtime: &mtime}); err != nil {
		t.Fatalf("set manifest: %v", err)
	}
	if err := st.CreateBinding(ctx, store.ActiveBinding{
		GameID: "super-metroid", PrimaryNode: "bob-deck",
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
	if a.GameID != "super-metroid" || a.PrimaryNode != "bob-deck" || a.Conflict {
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

func TestAPIGamesMtimeAlwaysPresent(t *testing.T) {
	// docs/api.md shows mtime always present on each path (a value or null).
	// Add a second path with no manifest entry so we exercise the null case.
	srv := newTestServer(t)
	ctx := context.Background()
	if err := srv.store.CreateNode(ctx, store.Node{
		ID: "alice-deck", Display: "Alice's Deck",
		Kind: store.KindDeck, Reach: store.ReachSyncthingShare,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := srv.store.SetGamePath(ctx, store.GamePath{GameID: "super-metroid", NodeID: "alice-deck", Path: "sm.srm"}); err != nil {
		t.Fatalf("set path: %v", err)
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

	// Every path object must carry an "mtime" key (present-or-null), never absent.
	var games []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &games); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	var sawKnown, sawNull bool
	for _, g := range games {
		paths, _ := g["paths"].([]any)
		for _, pv := range paths {
			p, _ := pv.(map[string]any)
			mv, ok := p["mtime"]
			if !ok {
				t.Fatalf("path object missing mtime key: %v", p)
			}
			switch p["node_id"] {
			case "bob-deck":
				if mv == nil {
					t.Error("bob-deck mtime is null, want a value")
				}
				sawKnown = true
			case "alice-deck":
				if mv != nil {
					t.Errorf("alice-deck mtime = %v, want null", mv)
				}
				sawNull = true
			}
		}
	}
	if !sawKnown || !sawNull {
		t.Fatalf("did not exercise both mtime cases (known=%v null=%v)", sawKnown, sawNull)
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
