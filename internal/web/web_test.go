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
	if err := st.CreateNode(ctx, store.Node{
		ID: "bob-deck", OwnerUserID: &owner, Display: "Bob's Deck",
		Kind: store.KindDeck, Reach: store.ReachSyncthingShare,
		ReachConfig: store.ReachConfig{Path: "/srv/saves"},
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	// The sync (the mirror + registry unit): member + manifest on bob-deck, and a
	// last_synced timestamp (it has mirrored at least once) — but NOT conflicted.
	// "Super Metroid" is the sync's free-text game label (no games table).
	if err := st.CreateSync(ctx, store.Sync{ID: "sm-bob", Game: "Super Metroid", Name: "Bob's stream"}); err != nil {
		t.Fatalf("create sync: %v", err)
	}
	if err := st.SetSyncMember(ctx, store.SyncMember{SyncID: "sm-bob", NodeID: "bob-deck", Path: "sm.srm"}); err != nil {
		t.Fatalf("set member: %v", err)
	}
	mtime := time.Date(2026, 6, 21, 11, 30, 0, 0, time.UTC)
	if err := st.SetManifest(ctx, store.ManifestEntry{SyncID: "sm-bob", NodeID: "bob-deck", Mtime: &mtime}); err != nil {
		t.Fatalf("set manifest: %v", err)
	}
	if err := st.MarkSyncSynced(ctx, "sm-bob", time.Date(2026, 6, 21, 11, 30, 0, 0, time.UTC)); err != nil {
		t.Fatalf("mark synced: %v", err)
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

// TestLoginSemaphoreFull_CancelledContextReturnsPromptly: the login verify
// semaphore (the argon2id memory-DoS cap) must respect context cancellation.
// With every slot held, a login request whose context is already cancelled has
// to return promptly with an error status — not queue forever — and must not
// mint a session.
func TestLoginSemaphoreFull_CancelledContextReturnsPromptly(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	// Hold every semaphore slot so acquisition can only complete via ctx.Done.
	for i := 0; i < cap(srv.loginSem); i++ {
		srv.loginSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(srv.loginSem); i++ {
			<-srv.loginSem
		}
	}()

	form := url.Values{"username": {"bob"}, "password": {testPassword}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx, cancel := context.WithCancel(req.Context())
	cancel() // the client is already gone
	req = req.WithContext(ctx)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		done <- rec
	}()
	select {
	case rec := <-done:
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if c := sessionCookie(rec.Result().Cookies()); c != nil {
			t.Fatal("cancelled login set a session cookie")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("login deadlocked on a full verify semaphore")
	}
}

// TestDashboard_AdminSeesAllSyncs: an admin who owns ZERO nodes still sees
// every sync on the dashboard (docs/ui.md) — otherwise a household whose nodes
// have no owner set has no UI path to the conflict Resolve button. The
// conflicted sync's card must render the Resolve button, and no member line
// gets the "(yours)" marker (the admin owns none of the nodes).
func TestDashboard_AdminSeesAllSyncs(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()

	// An admin owning no nodes at all.
	hash, err := auth.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := f.store.CreateUser(ctx, store.User{ID: "root", Display: "Root", PwHash: hash, Role: store.RoleAdmin}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	// A conflicted sync whose only member is on the unowned node "mister".
	if err := f.store.CreateSync(ctx, store.Sync{ID: "z-link", Game: "Zelda", Name: "Link's stream"}); err != nil {
		t.Fatalf("create sync: %v", err)
	}
	if err := f.store.SetSyncMember(ctx, store.SyncMember{SyncID: "z-link", NodeID: "mister", Path: "zelda.srm"}); err != nil {
		t.Fatalf("set member: %v", err)
	}
	conflict := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	if err := f.store.SetSyncConflict(ctx, "z-link", &conflict); err != nil {
		t.Fatalf("set conflict: %v", err)
	}

	c, _ := loginAs(t, f, "root")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"z-link", "sm-bob", "Resolve conflict"} {
		if !strings.Contains(body, want) {
			t.Errorf("admin dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "(yours)") {
		t.Error("admin owning no nodes got a \"(yours)\" member marker")
	}
}

// TestDashboard_NonAdminSeesOnlyOwnedMemberSyncs: a regular user's dashboard
// keeps the owned-member filter — a sync with no member on any node they own
// stays hidden.
func TestDashboard_NonAdminSeesOnlyOwnedMemberSyncs(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	if err := f.store.CreateSync(ctx, store.Sync{ID: "z-link", Game: "Zelda", Name: "Link's stream"}); err != nil {
		t.Fatalf("create sync: %v", err)
	}
	if err := f.store.SetSyncMember(ctx, store.SyncMember{SyncID: "z-link", NodeID: "mister", Path: "zelda.srm"}); err != nil {
		t.Fatalf("set member: %v", err)
	}

	c, _ := loginAs(t, f, "carol") // regular user, owns carol-deck (member of sm-bob)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "sm-bob") {
		t.Error("carol's dashboard should show sm-bob (member on her carol-deck)")
	}
	if strings.Contains(body, "z-link") {
		t.Error("carol's dashboard leaked z-link (no member on any node she owns)")
	}
	if !strings.Contains(body, "(yours)") {
		t.Error("carol's own member line should carry the \"(yours)\" marker")
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
	if len(got.Syncs) != 1 {
		t.Fatalf("syncs len = %d, want 1", len(got.Syncs))
	}
	a := got.Syncs[0]
	if a.SyncID != "sm-bob" || a.Game != "Super Metroid" || a.Conflict {
		t.Errorf("syncs[0] = %+v, unexpected", a)
	}
	if a.LastSynced == nil {
		t.Error("syncs[0].last_synced should be set (the sync has mirrored)")
	}
	if a.ConflictAt != nil {
		t.Error("syncs[0].conflict_at should be null (not conflicted)")
	}
	if len(got.Nodes) != 1 {
		t.Fatalf("nodes len = %d, want 1", len(got.Nodes))
	}
	n := got.Nodes[0]
	if n.ID != "bob-deck" {
		t.Errorf("nodes[0] = %+v, unexpected", n)
	}
}

func TestAPISyncsShape(t *testing.T) {
	// /api/syncs lists every sync as a flat list (id, free-text game label, name,
	// auto-mirror state, and members with node + path + last-known manifest mtime).
	// The seeded fixture has sync sm-bob (label "Super Metroid") with one member
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
	req := httptest.NewRequest(http.MethodGet, "/api/syncs", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var syncs []syncResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &syncs); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if len(syncs) != 1 {
		t.Fatalf("syncs len = %d, want 1", len(syncs))
	}
	sy := syncs[0]
	if sy.ID != "sm-bob" || sy.Game != "Super Metroid" || sy.Name != "Bob's stream" {
		t.Errorf("sync = %+v, unexpected", sy)
	}
	if sy.State.Conflict || sy.State.ConflictAt != nil {
		t.Errorf("sync.state = %+v, want not conflicted", sy.State)
	}
	if sy.State.LastSynced == nil {
		t.Errorf("sync.state.last_synced should be set (the sync has mirrored)")
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
	for _, sm := range raw {
		members, _ := sm["members"].([]any)
		for _, mv := range members {
			m, _ := mv.(map[string]any)
			if _, ok := m["mtime"]; !ok {
				t.Fatalf("member object missing mtime key: %v", m)
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
