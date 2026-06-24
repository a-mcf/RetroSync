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

// seedDiscovered programs the stub actioner with two discovered games so the
// /discover page has groups to render.
func seedDiscovered(f *actionFixture) {
	f.act.discoverGames = []engine.DiscoveredGame{
		{
			Name: "Super Metroid",
			Candidates: []engine.DiscoveredCandidate{
				{NodeID: "bob-deck", Path: "Super Metroid (USA).srm", Size: 8192, Mtime: time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)},
				{NodeID: "carol-deck", Path: "Super Metroid (Europe).srm", Size: 8192, Mtime: time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)},
			},
		},
		{
			Name: "Chrono Trigger",
			Candidates: []engine.DiscoveredCandidate{
				{NodeID: "bob-deck", Path: "Chrono Trigger.sav", Size: 2048, Mtime: time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)},
			},
		},
	}
}

// candidateValue builds the "<node>\x1f<path>" form value the page emits per
// candidate, so create tests submit exactly what the template would.
func candidateValue(nodeID, path string) string { return nodeID + candidateSep + path }

// --- GET /discover -------------------------------------------------------

func TestDiscoverPage_AdminRendersGroups(t *testing.T) {
	f := newActionFixture(t)
	seedDiscovered(f)
	c, _ := loginAs(t, f, "bob") // admin

	req := httptest.NewRequest(http.MethodGet, "/discover", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Super Metroid", "Chrono Trigger",
		"bob-deck", "carol-deck",
		"Super Metroid (USA).srm", "Create sync", "csrf_token",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("discover page missing %q", want)
		}
	}
	if f.act.discoverCount != 1 {
		t.Errorf("DiscoverGames called %d times, want 1", f.act.discoverCount)
	}
}

func TestDiscoverPage_NonAdminForbidden(t *testing.T) {
	f := newActionFixture(t)
	seedDiscovered(f)
	c, _ := loginAs(t, f, "carol") // regular user

	req := httptest.NewRequest(http.MethodGet, "/discover", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET /discover = %d, want 403", rec.Code)
	}
	if f.act.discoverCount != 0 {
		t.Errorf("non-admin reached DiscoverGames (%d calls)", f.act.discoverCount)
	}
}

func TestDiscoverPage_EmptyRendersNote(t *testing.T) {
	f := newActionFixture(t) // no discovered games seeded
	c, _ := loginAs(t, f, "bob")

	req := httptest.NewRequest(http.MethodGet, "/discover", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No un-synced save files") {
		t.Errorf("empty discover page should show a friendly note, got: %s", rec.Body.String())
	}
}

// --- POST /api/discover/create-sync --------------------------------------

func TestDiscoverCreateSync_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")
	ctx := context.Background()

	rec := postForm(t, f, c, csrf, "/api/discover/create-sync", url.Values{
		"game": {"Super Metroid"},
		"name": {"Main"},
		"id":   {"sm-discovered"},
		"candidate": {
			candidateValue("bob-deck", "Super Metroid (USA).srm"),
			candidateValue("carol-deck", "Super Metroid (Europe).srm"),
		},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/syncs" {
		t.Errorf("redirect = %q, want /syncs", loc)
	}

	sy, err := f.store.GetSync(ctx, "sm-discovered")
	if err != nil || sy.Game != "Super Metroid" || sy.Name != "Main" {
		t.Fatalf("sync wrong: %+v err=%v", sy, err)
	}
	members, err := f.store.ListSyncMembers(ctx, "sm-discovered")
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members len = %d, want 2: %+v", len(members), members)
	}
	byNode := map[string]string{}
	for _, m := range members {
		byNode[m.NodeID] = m.Path
	}
	if byNode["bob-deck"] != "Super Metroid (USA).srm" || byNode["carol-deck"] != "Super Metroid (Europe).srm" {
		t.Errorf("members wrong: %+v", byNode)
	}
}

func TestDiscoverCreateSync_HXRedirect(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	req := httptest.NewRequest(http.MethodPost, "/api/discover/create-sync",
		strings.NewReader(url.Values{
			"game":      {"Zelda"},
			"id":        {"zelda-main"},
			"candidate": {candidateValue("bob-deck", "Zelda.srm")},
		}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.AddCookie(c)
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HX create status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("HX-Redirect"); loc != "/syncs" {
		t.Errorf("HX-Redirect = %q, want /syncs", loc)
	}
	if _, err := f.store.GetSync(context.Background(), "zelda-main"); err != nil {
		t.Errorf("sync not created: %v", err)
	}
}

// Default name "Main" is applied when blank, and the id auto-slugs from game+name.
func TestDiscoverCreateSync_DefaultsNameAndAutoID(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/discover/create-sync", url.Values{
		"game":      {"Chrono Trigger"},
		"candidate": {candidateValue("bob-deck", "Chrono Trigger.sav")},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	// id = slugify("Chrono Trigger" + "-" + "Main")
	sy, err := f.store.GetSync(context.Background(), "chrono-trigger-main")
	if err != nil || sy.Name != "Main" {
		t.Fatalf("auto-id/default-name sync wrong: %+v err=%v", sy, err)
	}
}

func TestDiscoverCreateSync_EmptySelection_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/discover/create-sync", url.Values{
		"game": {"Super Metroid"},
		"name": {"Main"},
		// no candidate fields
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty selection status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	// No sync should have been created.
	if syncIDsMap(t, f)["super-metroid-main"] {
		t.Error("empty-selection create still made a sync")
	}
}

func TestDiscoverCreateSync_MissingGame_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/discover/create-sync", url.Values{
		"candidate": {candidateValue("bob-deck", "x.srm")},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing game status = %d, want 400", rec.Code)
	}
}

func TestDiscoverCreateSync_DuplicateID_409(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/discover/create-sync", url.Values{
		"game":      {"Super Metroid"},
		"id":        {"sm-bob"}, // already exists in the fixture
		"candidate": {candidateValue("mister", "x.srm")},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("dup id status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
}

// A candidate whose (node,path) is already in ANOTHER sync -> 409 (UNIQUE(node,path)).
func TestDiscoverCreateSync_CandidateAlreadyInSync_409(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	c, csrf := loginAs(t, f, "bob")

	// The fixture's sm-bob already claims (bob-deck, "sm.srm"). Trying to add that
	// same (node,path) into a new sync must 409 on the UNIQUE(node,path) invariant.
	rec := postForm(t, f, c, csrf, "/api/discover/create-sync", url.Values{
		"game":      {"Super Metroid"},
		"id":        {"sm-dupe-member"},
		"candidate": {candidateValue("bob-deck", "sm.srm")},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("candidate-already-in-sync status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "another sync") {
		t.Errorf("expected 'already in another sync' message, got: %s", rec.Body.String())
	}
	// The new sync row was created (the conflict was on member-add); its existence is
	// acceptable (a half-built sync the admin can fix), but the colliding member must
	// NOT have been moved off sm-bob.
	if m, err := f.store.GetSyncMember(ctx, "sm-bob", "bob-deck"); err != nil || m.Path != "sm.srm" {
		t.Errorf("original member disturbed: %+v err=%v", m, err)
	}
}

func TestDiscoverCreateSync_MissingNode_422(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/discover/create-sync", url.Values{
		"game":      {"Super Metroid"},
		"id":        {"sm-ghost"},
		"candidate": {candidateValue("ghost-node", "x.srm")},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing-node candidate status = %d, want 422\n%s", rec.Code, rec.Body.String())
	}
}

// --- admin-gating + CSRF -------------------------------------------------

func TestDiscoverCreateSync_NonAdminForbidden_StoreUntouched(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "carol") // regular user, valid CSRF

	rec := postForm(t, f, c, csrf, "/api/discover/create-sync", url.Values{
		"game":      {"Super Metroid"},
		"id":        {"hax-discovered"},
		"candidate": {candidateValue("bob-deck", "x.srm")},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin create = %d, want 403", rec.Code)
	}
	if syncIDsMap(t, f)["hax-discovered"] {
		t.Error("non-admin created a sync via discovery")
	}
}

func TestDiscoverCreateSync_CSRF(t *testing.T) {
	for _, tc := range []struct{ label, token string }{
		{"no-token", ""},
		{"wrong-token", "not-the-real-token"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			f := newActionFixture(t)
			c, _ := loginAs(t, f, "bob") // admin, bad/no CSRF

			rec := postForm(t, f, c, tc.token, "/api/discover/create-sync", url.Values{
				"game":      {"Super Metroid"},
				"id":        {"csrf-discovered"},
				"candidate": {candidateValue("bob-deck", "x.srm")},
			})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s = %d, want 403", tc.label, rec.Code)
			}
			if _, err := f.store.GetSync(context.Background(), "csrf-discovered"); err != store.ErrNotFound {
				t.Error("CSRF-less create-sync mutated the store")
			}
		})
	}
}
