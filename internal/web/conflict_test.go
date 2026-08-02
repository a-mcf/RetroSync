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
)

// seedConflictedSync marks sync sm-bob as conflicted (conflict_at set), so the
// dashboard banner and the resolve handler's authorization path have a
// conflicted sync to act on. Authority under auto-mirror is owning a member node
// (bob-deck / carol-deck are the seeded members), so the primaryNode arg of the
// old binding-based helper is gone.
func seedConflictedSync(t *testing.T, f *actionFixture) {
	t.Helper()
	conflict := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	if err := f.store.SetSyncConflict(context.Background(), "sm-bob", &conflict); err != nil {
		t.Fatalf("seed conflicted sync: %v", err)
	}
}

// seedSyncedSync marks sync sm-bob as having completed a mirror pass
// (last_synced set), which is what distinguishes a MID-LIFE fork from a
// never-synced sync's first conflict in the UI copy. The fixture's sync is
// freshly created, so last_synced is nil unless a test calls this.
func seedSyncedSync(t *testing.T, f *actionFixture) {
	t.Helper()
	synced := time.Date(2026, 6, 21, 11, 30, 0, 0, time.UTC)
	if err := f.store.MarkSyncSynced(context.Background(), "sm-bob", synced); err != nil {
		t.Fatalf("seed synced sync: %v", err)
	}
}

// --- Conflict modal ------------------------------------------------------

// TestConflictModal_RendersLiveStatePerNode_WinnerButtonOnlyWhenPresent asserts
// the modal lists each in-scope node's CURRENT mtime+size with a "Use this node"
// button ONLY for nodes that hold a file; an absent node shows "no save" and no
// button (you cannot pick an empty file).
func TestConflictModal_RendersLiveStatePerNode_WinnerButtonOnlyWhenPresent(t *testing.T) {
	f := newActionFixture(t)
	seedSyncedSync(t, f) // mid-life fork: exercises the "won't choose for you" copy
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

// TestConflictModal_FirstSyncVsMidLifeCopy asserts the modal reframes the FIRST
// conflict of a never-synced sync (LastSynced == nil) as the last setup step —
// "choose your starting save" — instead of the (literally false) "more than one
// device changed since the last sync". A sync that HAS synced keeps the paused
// fork copy. Same forms, same winner buttons in both.
func TestConflictModal_FirstSyncVsMidLifeCopy(t *testing.T) {
	tests := []struct {
		name       string
		everSynced bool
		want       []string
		absent     []string
	}{
		{
			name:       "never synced",
			everSynced: false,
			want: []string{
				"choose your starting save",
				"different saves for this game",
				"Pick the one to",
				`aria-label="Choose your starting save for`,
			},
			absent: []string{"sync paused", "since the last sync", "won't choose for you"},
		},
		{
			name:       "mid-life fork",
			everSynced: true,
			want:       []string{"sync paused", "More than one device changed since the last sync", "won't choose for you"},
			absent:     []string{"choose your starting save"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newActionFixture(t)
			seedConflictedSync(t, f)
			if tc.everSynced {
				seedSyncedSync(t, f)
			}
			f.act.nodeStates = []engine.NodeState{
				{NodeID: "bob-deck", Present: true, Mtime: time.Date(2026, 6, 21, 11, 56, 0, 0, time.UTC), Size: 1024},
				{NodeID: "carol-deck", Present: true, Mtime: time.Date(2026, 6, 21, 11, 58, 0, 0, time.UTC), Size: 2048},
			}
			c, _ := loginAs(t, f, "carol")

			req := httptest.NewRequest(http.MethodGet, "/syncs/sm-bob/conflict", nil)
			req.AddCookie(c)
			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("modal status = %d, want 200\n%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("modal missing %q\n%s", want, body)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(body, bad) {
					t.Errorf("modal should not contain %q\n%s", bad, body)
				}
			}
			// Both variants keep the identical per-device winner forms.
			for _, want := range []string{
				`hx-post="/api/syncs/sm-bob/resolve-conflict"`,
				`name="winner_node_id" value="bob-deck"`,
				`name="winner_node_id" value="carol-deck"`,
				"Use bob-deck",
				`href="/syncs/sm-bob/history"`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("modal missing %q\n%s", want, body)
				}
			}
			// Display strings say "device", never "node" (slice-30 wording rule).
			// The identifiers (winner_node_id, node ids) are exempt.
			for _, prose := range []string{"one node", "every other node", "other node's"} {
				if strings.Contains(body, prose) {
					t.Errorf("modal prose still says %q (should say device)\n%s", prose, body)
				}
			}
		})
	}
}

// --- POST resolve-conflict -----------------------------------------------

func TestResolveConflict_MemberOwner_CallsActionerAndRefreshes(t *testing.T) {
	f := newActionFixture(t)
	seedConflictedSync(t, f) // carol owns carol-deck, a member of sm-bob
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
	seedConflictedSync(t, f)
	c, csrf := loginAs(t, f, "bob") // admin
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

// TestResolveConflict_NonMemberOwner_403_NoActionerCall: the most destructive
// action must be member-owner/admin-gated, with zero engine calls on rejection.
// carol owns no member of this sync (we remove carol-deck so only bob-deck, which
// carol does not own, remains).
func TestResolveConflict_NonMemberOwner_403_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	seedConflictedSync(t, f)
	// Remove carol's node from the sync so carol owns NO member node of it.
	if err := f.store.DeleteSyncMember(context.Background(), "sm-bob", "carol-deck"); err != nil {
		t.Fatalf("remove carol-deck: %v", err)
	}
	c, csrf := loginAs(t, f, "carol") // owns no member of sm-bob now

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
	seedConflictedSync(t, f)
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
	seedConflictedSync(t, f)
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
	seedConflictedSync(t, f)
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
	seedConflictedSync(t, f)
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
	seedConflictedSync(t, f)
	f.act.resolveErr = engine.ErrSourceMissing
	c, csrf := loginAs(t, f, "carol")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/resolve-conflict", url.Values{
		"winner_node_id": {"carol-deck"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("source-missing status = %d, want 422\n%s", rec.Code, rec.Body.String())
	}
}

// TestResolveConflict_NoSuchSync_409 asserts that resolving a non-existent sync
// is a 409 (nothing to resolve) and never reaches the engine — without leaking
// whether the sync exists.
func TestResolveConflict_NoSuchSync_409(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob") // admin
	rec := postForm(t, f, c, csrf, "/api/syncs/ghost-sync/resolve-conflict", url.Values{
		"winner_node_id": {"bob-deck"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("no-such-sync resolve status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.resolveCalls()); n != 0 {
		t.Fatalf("actioner called %d times on missing-sync resolve, want 0", n)
	}
}

// --- Dashboard conflict banner -------------------------------------------

// TestDashboard_ConflictBanner_ShownWhenConflicted asserts the red banner and a
// modal-open button render when conflict_at is set on a sync that has synced
// before (a mid-life fork).
func TestDashboard_ConflictBanner_ShownWhenConflicted(t *testing.T) {
	f := newActionFixture(t)
	seedConflictedSync(t, f)
	seedSyncedSync(t, f)
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
	// The seeded sync is in sync (not conflicted) by default.
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

// TestDashboard_ConflictBanner_FirstSyncVsMidLife asserts the card banner splits
// on last_synced: a never-synced sync's first conflict gets the amber
// "almost there — pick your starting save" framing (the setup path guarantees
// the devices differ), while a sync that has synced before keeps the red
// "sync paused" one. Both open the SAME conflict modal.
func TestDashboard_ConflictBanner_FirstSyncVsMidLife(t *testing.T) {
	tests := []struct {
		name       string
		everSynced bool
		want       []string
		absent     []string
	}{
		{
			name:       "never synced",
			everSynced: false,
			want:       []string{`class="conflict first-sync"`, "Almost there", "pick your starting save", "Choose starting save"},
			absent:     []string{"Sync paused", "Resolve conflict"},
		},
		{
			name:       "mid-life fork",
			everSynced: true,
			want:       []string{`class="conflict"`, "Sync paused", "Resolve conflict"},
			absent:     []string{"first-sync", "Almost there", "Choose starting save"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newActionFixture(t)
			seedConflictedSync(t, f)
			if tc.everSynced {
				seedSyncedSync(t, f)
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
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("dashboard missing %q\n%s", want, body)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(body, bad) {
					t.Errorf("dashboard should not contain %q\n%s", bad, body)
				}
			}
			// Same modal-open button either way.
			if !strings.Contains(body, `hx-get="/syncs/sm-bob/conflict"`) {
				t.Errorf("conflict banner missing modal-open button\n%s", body)
			}
			// Banner prose says "device", not "node".
			if strings.Contains(body, "more than one node") {
				t.Errorf("banner prose still says node\n%s", body)
			}
		})
	}
}
