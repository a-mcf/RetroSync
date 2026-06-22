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
	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/fakereach"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

// stubActioner records Activate/Deactivate/ResolveConflict calls and returns
// programmable errors / canned NodeStates, so handler tests assert on the call
// (or its absence) without an engine.
type stubActioner struct {
	mu          sync.Mutex
	activateErr error
	calls       []activateCall
	deactivated []string

	// resolveErr is returned by ResolveConflict; resolved records each call.
	resolveErr error
	resolved   []resolveCall
	// nodeStates is the canned slice returned by NodeStates.
	nodeStates []engine.NodeState

	// smokeErr is returned by SmokeTest; smokeTested records each probed node id.
	smokeErr    error
	smokeTested []string
}

type activateCall struct {
	syncID, primaryNode, direction string
	force                          bool
}

type resolveCall struct {
	syncID, winnerNodeID string
}

func (s *stubActioner) Activate(_ context.Context, syncID, primaryNode, direction string, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, activateCall{syncID, primaryNode, direction, force})
	return s.activateErr
}

func (s *stubActioner) Deactivate(_ context.Context, syncID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deactivated = append(s.deactivated, syncID)
	return nil
}

func (s *stubActioner) ResolveConflict(_ context.Context, syncID, winnerNodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved = append(s.resolved, resolveCall{syncID, winnerNodeID})
	return s.resolveErr
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

func (s *stubActioner) resolveCalls() []resolveCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]resolveCall(nil), s.resolved...)
}

func (s *stubActioner) activateCalls() []activateCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]activateCall(nil), s.calls...)
}

func (s *stubActioner) deactivateCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deactivated...)
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

	if err := st.CreateGame(ctx, store.Game{ID: "super-metroid", Display: "Super Metroid", System: "snes"}); err != nil {
		t.Fatalf("create game: %v", err)
	}
	if err := st.CreateSync(ctx, store.Sync{ID: "sm-bob", GameID: "super-metroid", Name: "Bob's stream"}); err != nil {
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
		// Also seed the registry game_path (the /games admin UI still manages
		// game_paths, orphaned from the engine — TODO(slice-registry-sync)).
		if err := st.SetGamePath(ctx, store.GamePath{GameID: "super-metroid", NodeID: n, Path: "sm.srm"}); err != nil {
			t.Fatalf("set path %s: %v", n, err)
		}
	}

	act := &stubActioner{}
	srv, err := New(st, Options{Actioner: act})
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

// --- Activation modal ----------------------------------------------------

func TestActivateModal_MultiSource_ListsEveryMtime_DefaultBindingNode(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "bob")

	req := httptest.NewRequest(http.MethodGet, "/syncs/sm-bob/activate?node=bob-deck", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("modal status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Every node with a path is offered.
	for _, want := range []string{"bob-deck", "carol-deck", "2026-06-21 11:00"} {
		if !strings.Contains(body, want) {
			t.Errorf("modal missing %q\n%s", want, body)
		}
	}
	// The default radio is the binding node (from-primary, checked).
	if !strings.Contains(body, `value="from-primary"`) {
		t.Error("modal missing from-primary radio")
	}
	if !strings.Contains(body, `value="from-peer-carol-deck"`) {
		t.Error("modal missing from-peer-carol-deck radio")
	}
	// The primary radio is the checked one.
	if !checkedRadioHasValue(body, "from-primary") {
		t.Errorf("default checked radio is not from-primary\n%s", body)
	}
	// CSRF token embedded.
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Error("modal missing csrf_token field")
	}
}

func TestActivateModal_SingleSource_PreselectsAndListsMtimes(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	// Remove carol-deck's save so only bob-deck has one (still two configured
	// sources, but a single SAVE). Per docs/ui.md the lone save is pre-selected.
	if err := f.store.SetManifest(ctx, store.ManifestEntry{SyncID: "sm-bob", NodeID: "carol-deck"}); err != nil {
		t.Fatalf("clear carol manifest: %v", err)
	}
	c, _ := loginAs(t, f, "bob")

	// Bind from carol-deck (which has no save); the lone save (bob-deck) should
	// be pre-selected, not carol-deck.
	req := httptest.NewRequest(http.MethodGet, "/syncs/sm-bob/activate?node=carol-deck", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("modal status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no save yet") {
		t.Errorf("modal should show 'no save yet' for carol-deck\n%s", body)
	}
	// The lone save (bob-deck, a peer relative to carol-deck) is preselected.
	if !checkedRadioHasValue(body, "from-peer-bob-deck") {
		t.Errorf("lone save not preselected; checked radio not from-peer-bob-deck\n%s", body)
	}
}

// TestDashboard_TakeoverRow_OpensModal_NotDirectForcePost asserts that when a
// game is bound on another node, the owned-node action button hx-GETs the
// activation modal (which surfaces every node's mtime) rather than posting
// force=true directly. A one-click force POST would overwrite peers' saves with
// no mtime disclosure (BLOCKER 1, data-loss).
func TestDashboard_TakeoverRow_OpensModal_NotDirectForcePost(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	// Bind super-metroid on carol-deck so bob-deck (bob's owned node) becomes a
	// takeover row on bob's dashboard.
	if err := f.store.CreateBinding(ctx, store.ActiveBinding{
		SyncID: "sm-bob", PrimaryNode: "carol-deck",
		StartedAt: time.Now(), Direction: "from-primary",
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	c, _ := loginAs(t, f, "bob")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The takeover button must hx-get the modal for bob-deck.
	if !strings.Contains(body, `hx-get="/syncs/sm-bob/activate?node=bob-deck"`) {
		t.Errorf("takeover row does not hx-get the activation modal\n%s", body)
	}
	// And it must read as a takeover.
	if !strings.Contains(body, "Take over from carol-deck") {
		t.Errorf("takeover button label missing\n%s", body)
	}
	// It must NOT embed a direct force POST anywhere in the dashboard.
	if strings.Contains(body, `"force": "true"`) || strings.Contains(body, `"force":"true"`) {
		t.Errorf("dashboard embeds a direct force=true POST; takeover must go through the modal\n%s", body)
	}
}

// --- POST activate -------------------------------------------------------

func TestActivate_Success_CallsActionerAndRefreshes(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/activate", url.Values{
		"primary_node": {"bob-deck"},
		"direction":    {"from-peer-carol-deck"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("activate status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	calls := f.act.activateCalls()
	if len(calls) != 1 {
		t.Fatalf("activate calls = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.syncID != "sm-bob" || got.primaryNode != "bob-deck" ||
		got.direction != "from-peer-carol-deck" || got.force {
		t.Fatalf("activate call = %+v, unexpected", got)
	}
	// Refresh returns the dashboard fragment.
	if !strings.Contains(rec.Body.String(), `id="dashboard"`) {
		t.Error("activate success did not return the dashboard fragment")
	}
}

func TestActivate_ForceTakeover_409ThenForce(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	// First activate returns ErrConflict (game already bound elsewhere).
	f.act.activateErr = store.ErrConflict
	// Seed a binding so renderTakeover can name the "other" primary.
	if err := f.store.CreateBinding(context.Background(), store.ActiveBinding{
		SyncID: "sm-bob", PrimaryNode: "carol-deck",
		StartedAt: time.Now(), Direction: "from-primary",
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/activate", url.Values{
		"primary_node": {"bob-deck"},
		"direction":    {"from-primary"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Take over") || !strings.Contains(body, "carol-deck") {
		t.Errorf("409 body missing takeover offer naming carol-deck\n%s", body)
	}
	if !strings.Contains(body, `name="force"`) || !strings.Contains(body, `value="true"`) {
		t.Errorf("takeover fragment missing force=true\n%s", body)
	}
	if got := rec.Header().Get("HX-Retarget"); got != "#modal" {
		t.Errorf("HX-Retarget = %q, want #modal", got)
	}

	// Now re-POST with force=true; the Actioner should be called with force.
	f.act.activateErr = nil
	rec2 := postForm(t, f, c, csrf, "/api/syncs/sm-bob/activate", url.Values{
		"primary_node": {"bob-deck"},
		"direction":    {"from-primary"},
		"force":        {"true"},
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("force activate status = %d, want 200\n%s", rec2.Code, rec2.Body.String())
	}
	calls := f.act.activateCalls()
	last := calls[len(calls)-1]
	if !last.force {
		t.Fatalf("last activate call not forced: %+v", last)
	}
}

// --- Deactivate ----------------------------------------------------------

func TestDeactivate_CallsActioner_Idempotent(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	for i := 0; i < 2; i++ {
		rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/deactivate", url.Values{})
		if rec.Code != http.StatusOK {
			t.Fatalf("deactivate[%d] status = %d, want 200\n%s", i, rec.Code, rec.Body.String())
		}
	}
	if got := f.act.deactivateCalls(); len(got) != 2 {
		t.Fatalf("deactivate calls = %d, want 2 (idempotent at handler, engine no-ops)", len(got))
	}
}

// TestDeactivate_NonOwner_403_NoActionerCall asserts that a regular user who
// does not own a session's primary node cannot stop it (docs/auth.md: users may
// see but not modify other users' sessions). The Actioner must NOT be called.
func TestDeactivate_NonOwner_403_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	// Session is bound on bob-deck (owned by bob, the admin).
	if err := f.store.CreateBinding(ctx, store.ActiveBinding{
		SyncID: "sm-bob", PrimaryNode: "bob-deck",
		StartedAt: time.Now(), Direction: "from-primary",
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	// carol (regular user) does not own bob-deck.
	c, csrf := loginAs(t, f, "carol")
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/deactivate", url.Values{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner deactivate status = %d, want 403\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.deactivateCalls()); n != 0 {
		t.Fatalf("actioner called %d times for unauthorized deactivate, want 0", n)
	}
}

// TestDeactivate_Owner_Allowed asserts the owner of a session's primary node may
// stop it.
func TestDeactivate_Owner_Allowed(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	// Session bound on carol-deck (owned by carol).
	if err := f.store.CreateBinding(ctx, store.ActiveBinding{
		SyncID: "sm-bob", PrimaryNode: "carol-deck",
		StartedAt: time.Now(), Direction: "from-primary",
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	c, csrf := loginAs(t, f, "carol")
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/deactivate", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner deactivate status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.deactivateCalls()); n != 1 {
		t.Fatalf("actioner deactivate calls = %d, want 1", n)
	}
}

// TestDeactivate_Admin_Allowed asserts an admin may stop a session bound on a
// node they do not own.
func TestDeactivate_Admin_Allowed(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	// Session bound on carol-deck; bob is admin and does not own it.
	if err := f.store.CreateBinding(ctx, store.ActiveBinding{
		SyncID: "sm-bob", PrimaryNode: "carol-deck",
		StartedAt: time.Now(), Direction: "from-primary",
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	c, csrf := loginAs(t, f, "bob") // admin
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/deactivate", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin deactivate status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.deactivateCalls()); n != 1 {
		t.Fatalf("actioner deactivate calls = %d, want 1", n)
	}
}

// TestDeactivate_IdleGame_IdempotentSuccess asserts that deactivating a game
// with no binding is a harmless no-op for any authenticated user (no 403), per
// api.md idempotency.
func TestDeactivate_IdleGame_IdempotentSuccess(t *testing.T) {
	f := newActionFixture(t)
	// No binding seeded; carol (regular user) deactivates an idle game.
	c, csrf := loginAs(t, f, "carol")
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/deactivate", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("idle deactivate status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.deactivateCalls()); n != 1 {
		t.Fatalf("actioner deactivate calls = %d, want 1 (idempotent no-op), got %d", n, n)
	}
}

// --- CSRF ----------------------------------------------------------------

func TestActivate_CSRF_NoToken_403_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "bob")

	rec := postForm(t, f, c, "" /* no csrf */, "/api/syncs/sm-bob/activate", url.Values{
		"primary_node": {"bob-deck"}, "direction": {"from-primary"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no-token status = %d, want 403", rec.Code)
	}
	if n := len(f.act.activateCalls()); n != 0 {
		t.Fatalf("actioner called %d times on missing CSRF, want 0", n)
	}
}

func TestActivate_CSRF_WrongToken_403_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "bob")

	rec := postForm(t, f, c, "totally-wrong-token", "/api/syncs/sm-bob/activate", url.Values{
		"primary_node": {"bob-deck"}, "direction": {"from-primary"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-token status = %d, want 403", rec.Code)
	}
	if n := len(f.act.activateCalls()); n != 0 {
		t.Fatalf("actioner called %d times on wrong CSRF, want 0", n)
	}
}

func TestDeactivate_CSRF_NoToken_403(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "bob")
	rec := postForm(t, f, c, "", "/api/syncs/sm-bob/deactivate", url.Values{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if n := len(f.act.deactivateCalls()); n != 0 {
		t.Fatalf("actioner called %d times on missing CSRF, want 0", n)
	}
}

// --- Authorization -------------------------------------------------------

func TestActivate_User_CannotBindOnNodeTheyDontOwn_403(t *testing.T) {
	f := newActionFixture(t)
	// carol (regular user) tries to bind onto bob-deck (owned by bob).
	c, csrf := loginAs(t, f, "carol")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/activate", url.Values{
		"primary_node": {"bob-deck"},
		"direction":    {"from-primary"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.activateCalls()); n != 0 {
		t.Fatalf("actioner called %d times for unauthorized bind, want 0", n)
	}
}

func TestActivate_User_CanBindOwnNode(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "carol")
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/activate", url.Values{
		"primary_node": {"carol-deck"},
		"direction":    {"from-primary"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.activateCalls()); n != 1 {
		t.Fatalf("actioner calls = %d, want 1", n)
	}
}

// TestActivate_OwnedButNonMemberPrimary_Rejected_NoBinding is the authority-gap
// regression: a regular user who OWNS a node that is NOT a member of the target
// sync must NOT be able to seize that sync's binding by naming the owned
// non-member as primary. The web ownership check PASSES (they own the node);
// the engine's member gate is what must reject it. This test wires the REAL
// engine (not the recording stub) so the gate actually runs end-to-end, and
// asserts the request is rejected (not 200) with NO binding created.
func TestActivate_OwnedButNonMemberPrimary_Rejected_NoBinding(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	hash, err := auth.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := st.CreateUser(ctx, store.User{ID: "carol", Display: "carol", PwHash: hash, Role: store.RoleUser}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	carol := "carol"
	// carol-deck is a member of the sync; carol-outsider is owned by carol but is
	// NOT a member of the sync.
	for _, id := range []string{"carol-deck", "carol-outsider"} {
		if err := st.CreateNode(ctx, store.Node{
			ID: id, OwnerUserID: &carol, Display: id,
			Kind: store.KindDeck, Reach: store.ReachSyncthingShare,
		}); err != nil {
			t.Fatalf("create node %s: %v", id, err)
		}
	}
	if err := st.CreateGame(ctx, store.Game{ID: "super-metroid", Display: "Super Metroid", System: "snes"}); err != nil {
		t.Fatalf("create game: %v", err)
	}
	if err := st.CreateSync(ctx, store.Sync{ID: "sm-bob", GameID: "super-metroid", Name: "Bob's stream"}); err != nil {
		t.Fatalf("create sync: %v", err)
	}
	// Only carol-deck is a member of the sync (with a save so activation would
	// otherwise be viable). carol-outsider is deliberately NOT a member.
	mtime := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	if err := st.SetSyncMember(ctx, store.SyncMember{SyncID: "sm-bob", NodeID: "carol-deck", Path: "sm.srm"}); err != nil {
		t.Fatalf("set member: %v", err)
	}
	if err := st.SetManifest(ctx, store.ManifestEntry{SyncID: "sm-bob", NodeID: "carol-deck", Mtime: &mtime}); err != nil {
		t.Fatalf("set manifest: %v", err)
	}

	// Real engine with a fakereach per node; carol-deck holds a save.
	fakes := map[string]*fakereach.Fake{
		"carol-deck":     fakereach.New().Put("sm.srm", []byte("SAVE"), mtime),
		"carol-outsider": fakereach.New().Put("o.srm", []byte("OUTSIDER"), mtime),
	}
	resolve := func(n store.Node) (reach.Reach, error) {
		f, ok := fakes[n.ID]
		if !ok {
			t.Fatalf("no fake for node %q", n.ID)
		}
		return f, nil
	}
	eng := engine.New(st, resolve, nil)
	srv, err := New(st, Options{Actioner: eng})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f := &actionFixture{srv: srv, store: st, act: nil}

	c, csrf := loginAs(t, f, "carol")
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/activate", url.Values{
		"primary_node": {"carol-outsider"}, // owned by carol, NOT a member
		"direction":    {"from-peer-carol-deck"},
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("non-member primary must be rejected, got 200\n%s", rec.Body.String())
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422\n%s", rec.Code, rec.Body.String())
	}
	// No binding may have been created for the sync.
	if _, err := st.GetBinding(ctx, "sm-bob"); err == nil {
		t.Fatal("a binding was created for a sync the caller is not a member of")
	}
}

func TestActivate_Admin_CanBindAnyNode(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob") // admin
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/activate", url.Values{
		"primary_node": {"carol-deck"}, // not bob's node
		"direction":    {"from-primary"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin bind status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.activateCalls()); n != 1 {
		t.Fatalf("actioner calls = %d, want 1", n)
	}
}

// checkedRadioHasValue reports whether the input bearing `checked` carries the
// given value attribute. Tolerant of attribute order within a single tag.
func checkedRadioHasValue(html, value string) bool {
	for _, tag := range splitTags(html, "<input") {
		if strings.Contains(tag, "checked") && strings.Contains(tag, `value="`+value+`"`) {
			return true
		}
	}
	return false
}

// splitTags returns substrings starting at each occurrence of start up to the
// next '>' — a crude tag scanner sufficient for these template assertions.
func splitTags(html, start string) []string {
	var out []string
	for i := 0; ; {
		j := strings.Index(html[i:], start)
		if j < 0 {
			break
		}
		j += i
		end := strings.IndexByte(html[j:], '>')
		if end < 0 {
			out = append(out, html[j:])
			break
		}
		out = append(out, html[j:j+end+1])
		i = j + end + 1
	}
	return out
}
