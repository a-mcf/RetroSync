package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/store"
)

// seedConflictBinding seeds sync sm-bob as bound on primaryNode with conflict_at
// set, so the dashboard banner and the resolve handler's authorization path have
// a conflicted binding to act on.
func seedConflictBinding(t *testing.T, f *actionFixture, primaryNode string) {
	t.Helper()
	conflict := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	if err := f.store.CreateBinding(context.Background(), store.ActiveBinding{
		SyncID: "sm-bob", PrimaryNode: primaryNode,
		StartedAt:  time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC),
		Direction:  "from-primary",
		ConflictAt: &conflict,
	}); err != nil {
		t.Fatalf("seed conflict binding: %v", err)
	}
}

// --- Conflict modal ------------------------------------------------------

// TestConflictModal_RendersLiveStatePerNode_WinnerButtonOnlyWhenPresent asserts
// the modal lists each in-scope node's CURRENT mtime+size with a "Use this node"
// button ONLY for nodes that hold a file; an absent node shows "no save" and no
// button (you cannot pick an empty file).
func TestConflictModal_RendersLiveStatePerNode_WinnerButtonOnlyWhenPresent(t *testing.T) {
	f := newActionFixture(t)
	mtime := time.Date(2026, 6, 21, 11, 56, 0, 0, time.UTC)
	f.act.nodeStates = []engine.NodeState{
		{NodeID: "bob-deck", Present: true, Mtime: mtime, Size: 65 * 1024},
		{NodeID: "carol-deck", Present: false},
	}
	c, _ := loginAs(t, f, "bob")

	req := httptest.NewRequest(http.MethodGet, "/syncs/sm-bob/conflict", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("conflict modal status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Present node: mtime, size, and a "Use bob-deck" winner button.
	for _, want := range []string{"bob-deck", "2026-06-21 11:56", "65 KB", "Use bob-deck"} {
		if !strings.Contains(body, want) {
			t.Errorf("modal missing %q\n%s", want, body)
		}
	}
	// Winner button POSTs to resolve-conflict with the node as winner_node_id.
	if !strings.Contains(body, `hx-post="/api/syncs/sm-bob/resolve-conflict"`) {
		t.Errorf("modal winner button does not POST resolve-conflict\n%s", body)
	}
	if !strings.Contains(body, `name="winner_node_id" value="bob-deck"`) {
		t.Errorf("modal missing winner_node_id for bob-deck\n%s", body)
	}
	// CSRF embedded.
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Errorf("modal missing csrf_token field\n%s", body)
	}
	// Absent node: "no save", and NO winner button for it.
	if !strings.Contains(body, "no save") {
		t.Errorf("modal should show 'no save' for absent node\n%s", body)
	}
	if strings.Contains(body, `value="carol-deck"`) {
		t.Errorf("absent node carol-deck must not get a winner button\n%s", body)
	}
	// The copy makes clear retrosync won't choose.
	if !strings.Contains(body, "won't choose for you") {
		t.Errorf("modal missing the 'won't choose for you' copy\n%s", body)
	}
}

// TestConflictModal_ViewableByNonOwner asserts the read-only modal is NOT
// owner-gated: any authenticated user may view a conflict (brief D, consistent
// with "can see others' sessions").
func TestConflictModal_ViewableByNonOwner(t *testing.T) {
	f := newActionFixture(t)
	f.act.nodeStates = []engine.NodeState{
		{NodeID: "bob-deck", Present: true, Mtime: time.Now(), Size: 1024},
	}
	// carol (regular user) does not own bob-deck, but may still VIEW the modal.
	c, _ := loginAs(t, f, "carol")
	req := httptest.NewRequest(http.MethodGet, "/syncs/sm-bob/conflict", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-owner conflict modal status = %d, want 200", rec.Code)
	}
}

// --- POST resolve-conflict -----------------------------------------------

func TestResolveConflict_Owner_CallsActionerAndRefreshes(t *testing.T) {
	f := newActionFixture(t)
	seedConflictBinding(t, f, "carol-deck") // owned by carol
	c, csrf := loginAs(t, f, "carol")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/resolve-conflict", url.Values{
		"winner_node_id": {"carol-deck"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	calls := f.act.resolveCalls()
	if len(calls) != 1 {
		t.Fatalf("resolve calls = %d, want 1", len(calls))
	}
	if calls[0].syncID != "sm-bob" || calls[0].winnerNodeID != "carol-deck" {
		t.Fatalf("resolve call = %+v, unexpected", calls[0])
	}
	if !strings.Contains(rec.Body.String(), `id="dashboard"`) {
		t.Error("resolve success did not return the dashboard fragment")
	}
}

func TestResolveConflict_Admin_Allowed(t *testing.T) {
	f := newActionFixture(t)
	seedConflictBinding(t, f, "carol-deck") // not bob's node
	c, csrf := loginAs(t, f, "bob")         // admin
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/resolve-conflict", url.Values{
		"winner_node_id": {"carol-deck"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin resolve status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.resolveCalls()); n != 1 {
		t.Fatalf("resolve calls = %d, want 1", n)
	}
}

// TestResolveConflict_NonOwner_403_NoActionerCall: the most destructive action
// must be owner/admin-gated, with zero engine calls on rejection.
func TestResolveConflict_NonOwner_403_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	seedConflictBinding(t, f, "bob-deck") // owned by bob (admin)
	c, csrf := loginAs(t, f, "carol")     // does not own bob-deck

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/resolve-conflict", url.Values{
		"winner_node_id": {"bob-deck"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner resolve status = %d, want 403\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.resolveCalls()); n != 0 {
		t.Fatalf("actioner called %d times for unauthorized resolve, want 0", n)
	}
}

func TestResolveConflict_CSRF_NoToken_403_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	seedConflictBinding(t, f, "carol-deck")
	c, _ := loginAs(t, f, "carol")

	rec := postForm(t, f, c, "" /* no csrf */, "/api/syncs/sm-bob/resolve-conflict", url.Values{
		"winner_node_id": {"carol-deck"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no-token resolve status = %d, want 403", rec.Code)
	}
	if n := len(f.act.resolveCalls()); n != 0 {
		t.Fatalf("actioner called %d times on missing CSRF, want 0", n)
	}
}

func TestResolveConflict_CSRF_WrongToken_403_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	seedConflictBinding(t, f, "carol-deck")
	c, _ := loginAs(t, f, "carol")

	rec := postForm(t, f, c, "totally-wrong-token", "/api/syncs/sm-bob/resolve-conflict", url.Values{
		"winner_node_id": {"carol-deck"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-token resolve status = %d, want 403", rec.Code)
	}
	if n := len(f.act.resolveCalls()); n != 0 {
		t.Fatalf("actioner called %d times on wrong CSRF, want 0", n)
	}
}

// TestResolveConflict_EmptyWinner_400_NoActionerCall: a missing winner_node_id
// is a 400 and never reaches the engine.
func TestResolveConflict_EmptyWinner_400_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	seedConflictBinding(t, f, "carol-deck")
	c, csrf := loginAs(t, f, "carol")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/resolve-conflict", url.Values{
		"winner_node_id": {""},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty-winner status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.resolveCalls()); n != 0 {
		t.Fatalf("actioner called %d times on empty winner, want 0", n)
	}
}

// TestResolveConflict_NotConflicted_409Refresh maps engine.ErrNotConflicted to a
// 409 that still refreshes the dashboard (already resolved).
func TestResolveConflict_NotConflicted_409Refresh(t *testing.T) {
	f := newActionFixture(t)
	seedConflictBinding(t, f, "carol-deck")
	f.act.resolveErr = engine.ErrNotConflicted
	c, csrf := loginAs(t, f, "carol")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/resolve-conflict", url.Values{
		"winner_node_id": {"carol-deck"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("already-resolved status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="dashboard"`) {
		t.Error("409 refresh did not return the dashboard fragment")
	}
}

// TestResolveConflict_SourceMissing_422 maps engine.ErrSourceMissing to 422.
func TestResolveConflict_SourceMissing_422(t *testing.T) {
	f := newActionFixture(t)
	seedConflictBinding(t, f, "carol-deck")
	f.act.resolveErr = engine.ErrSourceMissing
	c, csrf := loginAs(t, f, "carol")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/resolve-conflict", url.Values{
		"winner_node_id": {"carol-deck"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("source-missing status = %d, want 422\n%s", rec.Code, rec.Body.String())
	}
}

// TestResolveConflict_NoBinding_409 asserts that resolving an idle game (no
// binding) is a 409 (nothing to resolve) and never reaches the engine.
func TestResolveConflict_NoBinding_409(t *testing.T) {
	f := newActionFixture(t)
	// No binding seeded.
	c, csrf := loginAs(t, f, "bob") // admin
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/resolve-conflict", url.Values{
		"winner_node_id": {"bob-deck"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("idle resolve status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.resolveCalls()); n != 0 {
		t.Fatalf("actioner called %d times on idle resolve, want 0", n)
	}
}

// --- Dashboard conflict banner -------------------------------------------

// TestDashboard_ConflictBanner_ShownWhenConflicted asserts the red banner and a
// modal-open button render when conflict_at is set.
func TestDashboard_ConflictBanner_ShownWhenConflicted(t *testing.T) {
	f := newActionFixture(t)
	seedConflictBinding(t, f, "carol-deck")
	c, _ := loginAs(t, f, "carol")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Sync paused") {
		t.Errorf("conflict banner copy missing\n%s", body)
	}
	if !strings.Contains(body, `hx-get="/syncs/sm-bob/conflict"`) {
		t.Errorf("conflict banner missing modal-open button\n%s", body)
	}
}

// TestDashboard_NoConflictBanner_WhenClean asserts the banner is absent for a
// clean (idle/non-conflicted) game.
func TestDashboard_NoConflictBanner_WhenClean(t *testing.T) {
	f := newActionFixture(t)
	// Active but NOT conflicted.
	if err := f.store.CreateBinding(context.Background(), store.ActiveBinding{
		SyncID: "sm-bob", PrimaryNode: "carol-deck",
		StartedAt: time.Now(), Direction: "from-primary",
	}); err != nil {
		t.Fatalf("seed clean binding: %v", err)
	}
	c, _ := loginAs(t, f, "carol")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Sync paused") {
		t.Errorf("conflict banner should be absent for a clean game\n%s", body)
	}
	if strings.Contains(body, `hx-get="/syncs/sm-bob/conflict"`) {
		t.Errorf("modal-open button should be absent for a clean game\n%s", body)
	}
}
