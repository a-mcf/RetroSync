package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/store"
)

// gameIDs returns the set of game ids currently in the fixture store.
func gameIDs(t *testing.T, f *actionFixture) map[string]bool {
	t.Helper()
	games, err := f.store.ListGames(context.Background(), store.GameFilter{})
	if err != nil {
		t.Fatalf("list games: %v", err)
	}
	out := make(map[string]bool, len(games))
	for _, g := range games {
		out[g.ID] = true
	}
	return out
}

// syncIDsMap returns the set of sync ids of super-metroid currently in the store.
func syncIDsMap(t *testing.T, f *actionFixture) map[string]bool {
	t.Helper()
	syncs, err := f.store.ListSyncsByGame(context.Background(), "super-metroid")
	if err != nil {
		t.Fatalf("list syncs: %v", err)
	}
	out := make(map[string]bool, len(syncs))
	for _, sy := range syncs {
		out[sy.ID] = true
	}
	return out
}

// syncIDs is a string rendering of syncIDsMap for failure messages.
func syncIDs(t *testing.T, f *actionFixture) string {
	t.Helper()
	out := make([]string, 0)
	for id := range syncIDsMap(t, f) {
		out = append(out, id)
	}
	return strings.Join(out, ",")
}

// seedConflict marks the sync sm-bob (of game super-metroid) as conflicted, so
// the registry delete guards (delete-game / delete-sync) have a conflicted sync
// to refuse against. Under auto-mirror that is the only registry guard left.
func seedConflict(t *testing.T, f *actionFixture) {
	t.Helper()
	conflict := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	if err := f.store.SetSyncConflict(context.Background(), "sm-bob", &conflict); err != nil {
		t.Fatalf("seed conflict: %v", err)
	}
}

// --- GET /games (admin) --------------------------------------------------

func TestGamesPage_AdminSeesRegistry(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "bob") // admin

	req := httptest.NewRequest(http.MethodGet, "/games", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"super-metroid", "Super Metroid", "Add a game", "csrf_token", "bob-deck"} {
		if !strings.Contains(body, want) {
			t.Errorf("games page missing %q", want)
		}
	}
}

func TestGamesPage_NonAdminForbidden(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "carol") // regular user

	req := httptest.NewRequest(http.MethodGet, "/games", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET /games = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Admins only") {
		t.Errorf("expected an 'Admins only' page, got: %s", rec.Body.String())
	}
}

func TestGamesPage_SearchFilter(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	if err := f.store.CreateGame(ctx, store.Game{ID: "zelda", Display: "Zelda", System: "nes"}); err != nil {
		t.Fatalf("seed game: %v", err)
	}
	c, _ := loginAs(t, f, "bob")

	// Use the HX-Request header so the handler returns just the filtered
	// games-list fragment (the full page embeds a placeholder "super-metroid"
	// in the add-game form, which is not part of the result set).
	req := httptest.NewRequest(http.MethodGet, "/games?q=zel", nil)
	req.AddCookie(c)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "zelda") {
		t.Error("search q=zel should match zelda")
	}
	if strings.Contains(body, "super-metroid") {
		t.Error("search q=zel should not match super-metroid")
	}
}

// --- POST /api/games (create) --------------------------------------------

func TestCreateGame_ExplicitID(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/games", url.Values{
		"id": {"chrono-trigger"}, "display": {"Chrono Trigger"}, "system": {"snes"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	g, err := f.store.GetGame(context.Background(), "chrono-trigger")
	if err != nil {
		t.Fatalf("game not created: %v", err)
	}
	if g.Display != "Chrono Trigger" || g.System != "snes" {
		t.Errorf("created game wrong: %+v", g)
	}
}

func TestCreateGame_AutoSlugFromDisplay(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	// No id; display has caps, spaces, and punctuation.
	rec := postForm(t, f, c, csrf, "/api/games", url.Values{
		"display": {"Super Mario Bros. 3!"}, "system": {"nes"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.GetGame(context.Background(), "super-mario-bros-3"); err != nil {
		t.Fatalf("auto-slug game not created as 'super-mario-bros-3': %v\nids: %v",
			err, gameIDs(t, f))
	}
}

func TestCreateGame_DuplicateID_409(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/games", url.Values{
		"id": {"super-metroid"}, "display": {"Dupe"}, "system": {"snes"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate id status = %d, want 409", rec.Code)
	}
}

func TestCreateGame_InvalidExplicitID_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/games", url.Values{
		"id": {"Super Metroid"}, "display": {"Super Metroid"}, "system": {"snes"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d, want 400", rec.Code)
	}
	if gameIDs(t, f)["Super Metroid"] {
		t.Error("game created despite invalid id")
	}
}

func TestCreateGame_MissingDisplay_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/games", url.Values{
		"id": {"x"}, "system": {"snes"}, // no display
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing display status = %d, want 400", rec.Code)
	}
}

// --- POST /api/games/{id} (edit) -----------------------------------------

func TestEditGame_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/games/super-metroid", url.Values{
		"display": {"Super Metroid (USA)"}, "system": {"snes"}, "notes": {"the good one"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	g, err := f.store.GetGame(context.Background(), "super-metroid")
	if err != nil {
		t.Fatalf("get game: %v", err)
	}
	if g.Display != "Super Metroid (USA)" || g.Notes != "the good one" {
		t.Errorf("edit did not apply: %+v", g)
	}
}

// --- POST /api/games/{id}/delete -----------------------------------------

func TestDeleteGame_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/games/super-metroid/delete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if gameIDs(t, f)["super-metroid"] {
		t.Error("game not deleted")
	}
}

func TestDeleteGame_Conflicted_Friendly409(t *testing.T) {
	f := newActionFixture(t)
	seedConflict(t, f)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/games/super-metroid/delete", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflicted delete status = %d, want 409 (not 500)", rec.Code)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "conflict") {
		t.Errorf("expected a friendly 'conflict' message, got: %s", rec.Body.String())
	}
	if !gameIDs(t, f)["super-metroid"] {
		t.Error("game deleted despite a conflicted sync")
	}
}

// --- POST /api/syncs (create) --------------------------------------------

func TestCreateSync_AutoID(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"game_id": {"super-metroid"}, "name": {"Alice stream"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create sync status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	// Auto-id is slugify(game_id + "-" + name).
	if _, err := f.store.GetSync(context.Background(), "super-metroid-alice-stream"); err != nil {
		t.Fatalf("auto-id sync not created: %v\n%s", err, syncIDs(t, f))
	}
}

func TestCreateSync_ExplicitID(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"id": {"sm-alice"}, "game_id": {"super-metroid"}, "name": {"Alice stream"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create sync status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	sy, err := f.store.GetSync(context.Background(), "sm-alice")
	if err != nil || sy.Name != "Alice stream" || sy.GameID != "super-metroid" {
		t.Fatalf("sync wrong: %+v err=%v", sy, err)
	}
}

func TestCreateSync_DuplicateID_409(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"id": {"sm-bob"}, "game_id": {"super-metroid"}, "name": {"Dupe"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate sync id status = %d, want 409", rec.Code)
	}
}

func TestCreateSync_MissingGame_422(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"game_id": {"ghost-game"}, "name": {"Stream"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing game status = %d, want 422", rec.Code)
	}
}

func TestCreateSync_BadSlug_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	// Explicit id with spaces/caps -> invalid slug -> 400.
	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"id": {"Bad ID"}, "game_id": {"super-metroid"}, "name": {"Stream"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad slug status = %d, want 400", rec.Code)
	}
}

func TestCreateSync_MissingName_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"game_id": {"super-metroid"}, // no name
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing name status = %d, want 400", rec.Code)
	}
}

// --- POST /api/syncs/{id} (rename) ---------------------------------------

func TestRenameSync_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob", url.Values{"name": {"Renamed stream"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	sy, _ := f.store.GetSync(context.Background(), "sm-bob")
	if sy.Name != "Renamed stream" || sy.GameID != "super-metroid" {
		t.Errorf("rename did not apply / lost game_id: %+v", sy)
	}
}

func TestRenameSync_Missing_404(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs/ghost", url.Values{"name": {"X"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("rename missing status = %d, want 404", rec.Code)
	}
}

// --- POST /api/syncs/{id}/delete -----------------------------------------

func TestDeleteSync_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/delete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete sync status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.GetSync(context.Background(), "sm-bob"); err != store.ErrNotFound {
		t.Errorf("sync not deleted, err = %v", err)
	}
}

func TestDeleteSync_Conflicted_Friendly409(t *testing.T) {
	f := newActionFixture(t)
	seedConflict(t, f) // sm-bob is now conflicted
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/delete", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflicted delete status = %d, want 409 (not 500)", rec.Code)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "conflict") {
		t.Errorf("expected a friendly 'conflict' message, got: %s", rec.Body.String())
	}
	if _, err := f.store.GetSync(context.Background(), "sm-bob"); err != nil {
		t.Error("conflicted sync deleted despite guard")
	}
}

// --- POST /api/syncs/{id}/members/{node_id} (set member) -----------------

func TestSetSyncMember_AddAndUpdate(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")
	ctx := context.Background()

	// Add a new member on mister (a distinct path, not claimed elsewhere).
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/members/mister", url.Values{
		"path": {"SNES/Super Metroid.sav"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("add member status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	m, err := f.store.GetSyncMember(ctx, "sm-bob", "mister")
	if err != nil || m.Path != "SNES/Super Metroid.sav" {
		t.Fatalf("member not added: %+v err=%v", m, err)
	}

	// Update the same member (upsert).
	rec = postForm(t, f, c, csrf, "/api/syncs/sm-bob/members/mister", url.Values{
		"path": {"SNES/SM.sav"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update member status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	m, _ = f.store.GetSyncMember(ctx, "sm-bob", "mister")
	if m.Path != "SNES/SM.sav" {
		t.Errorf("member not updated: %+v", m)
	}
}

func TestSetSyncMember_FileInAnotherSync_409(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	// Create a second sync, then claim (mister, "shared.sav") there.
	if err := f.store.CreateSync(ctx, store.Sync{ID: "sm-other", GameID: "super-metroid", Name: "Other"}); err != nil {
		t.Fatalf("create sync: %v", err)
	}
	if err := f.store.SetSyncMember(ctx, store.SyncMember{SyncID: "sm-other", NodeID: "mister", Path: "shared.sav"}); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	c, csrf := loginAs(t, f, "bob")

	// Try to claim the SAME (mister, "shared.sav") in sm-bob -> UNIQUE(node,path).
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/members/mister", url.Values{
		"path": {"shared.sav"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("cross-sync file claim status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "another sync") {
		t.Errorf("expected 'already in another sync' message, got: %s", rec.Body.String())
	}
}

func TestSetSyncMember_MissingNode_422(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/members/ghost-node", url.Values{
		"path": {"x.srm"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing node status = %d, want 422", rec.Code)
	}
}

func TestSetSyncMember_EmptyPath_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/members/mister", url.Values{
		"path": {"  "},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty path status = %d, want 400", rec.Code)
	}
}

// --- POST /api/syncs/{id}/members/{node_id}/delete (remove member) -------

func TestDeleteSyncMember_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/members/carol-deck/delete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete member status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.GetSyncMember(context.Background(), "sm-bob", "carol-deck"); err != store.ErrNotFound {
		t.Errorf("member not removed, err = %v", err)
	}
}

// TestDeleteSyncMember_AnyMemberRemovable: under auto-mirror there is no "active
// primary" to protect. Any member is removable — even from a conflicted sync —
// and a removed member's file simply stops mirroring.
func TestDeleteSyncMember_AnyMemberRemovable(t *testing.T) {
	f := newActionFixture(t)
	seedConflict(t, f) // even conflicted, the guard no longer blocks removal
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/members/bob-deck/delete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("member remove status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.GetSyncMember(context.Background(), "sm-bob", "bob-deck"); err != store.ErrNotFound {
		t.Errorf("member not removed: %v", err)
	}
}

// --- admin-gating across every mutation ----------------------------------

func TestGamesMutations_NonAdminForbidden_StoreUntouched(t *testing.T) {
	mutations := []struct {
		name, path string
		fields     url.Values
	}{
		{"create-game", "/api/games", url.Values{"id": {"hax"}, "display": {"Hax"}, "system": {"snes"}}},
		{"edit-game", "/api/games/super-metroid", url.Values{"display": {"Hijacked"}, "system": {"snes"}}},
		{"delete-game", "/api/games/super-metroid/delete", nil},
		{"create-sync", "/api/syncs", url.Values{"id": {"hax-sync"}, "game_id": {"super-metroid"}, "name": {"Hax"}}},
		{"rename-sync", "/api/syncs/sm-bob", url.Values{"name": {"Hijacked"}}},
		{"delete-sync", "/api/syncs/sm-bob/delete", nil},
		{"set-member", "/api/syncs/sm-bob/members/mister", url.Values{"path": {"x.sav"}}},
		{"del-member", "/api/syncs/sm-bob/members/carol-deck/delete", nil},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			f := newActionFixture(t)
			c, csrf := loginAs(t, f, "carol") // regular user, valid CSRF

			rec := postForm(t, f, c, csrf, m.path, m.fields)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s as non-admin = %d, want 403", m.name, rec.Code)
			}
			ids := gameIDs(t, f)
			if ids["hax"] {
				t.Error("non-admin created a game")
			}
			if !ids["super-metroid"] {
				t.Error("non-admin deleted a game")
			}
			g, _ := f.store.GetGame(context.Background(), "super-metroid")
			if g.Display == "Hijacked" {
				t.Error("non-admin edited a game")
			}
			if syncIDsMap(t, f)["hax-sync"] {
				t.Error("non-admin created a sync")
			}
			sy, _ := f.store.GetSync(context.Background(), "sm-bob")
			if sy.Name == "Hijacked" {
				t.Error("non-admin renamed a sync")
			}
			if _, err := f.store.GetSync(context.Background(), "sm-bob"); err != nil {
				t.Error("non-admin deleted a sync")
			}
			if _, err := f.store.GetSyncMember(context.Background(), "sm-bob", "mister"); err == nil {
				t.Error("non-admin added a member")
			}
			if _, err := f.store.GetSyncMember(context.Background(), "sm-bob", "carol-deck"); err != nil {
				t.Error("non-admin removed a member")
			}
		})
	}
}

// --- CSRF across every mutation ------------------------------------------

func TestGamesMutations_CSRF(t *testing.T) {
	mutations := []struct {
		name, path string
		fields     url.Values
	}{
		{"create-game", "/api/games", url.Values{"id": {"csrfgame"}, "display": {"X"}, "system": {"snes"}}},
		{"edit-game", "/api/games/super-metroid", url.Values{"display": {"X"}, "system": {"snes"}}},
		{"delete-game", "/api/games/super-metroid/delete", nil},
		{"create-sync", "/api/syncs", url.Values{"id": {"csrfsync"}, "game_id": {"super-metroid"}, "name": {"X"}}},
		{"rename-sync", "/api/syncs/sm-bob", url.Values{"name": {"X"}}},
		{"delete-sync", "/api/syncs/sm-bob/delete", nil},
		{"set-member", "/api/syncs/sm-bob/members/mister", url.Values{"path": {"x.sav"}}},
		{"del-member", "/api/syncs/sm-bob/members/carol-deck/delete", nil},
	}
	for _, m := range mutations {
		for _, tc := range []struct{ label, token string }{
			{"no-token", ""},
			{"wrong-token", "not-the-real-token"},
		} {
			t.Run(m.name+"/"+tc.label, func(t *testing.T) {
				f := newActionFixture(t)
				c, _ := loginAs(t, f, "bob") // admin, bad/no CSRF

				rec := postForm(t, f, c, tc.token, m.path, m.fields)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s %s = %d, want 403", m.name, tc.label, rec.Code)
				}
				if gameIDs(t, f)["csrfgame"] {
					t.Error("CSRF-less create-game mutated the store")
				}
				if !gameIDs(t, f)["super-metroid"] {
					t.Error("CSRF-less delete-game mutated the store")
				}
				if syncIDsMap(t, f)["csrfsync"] {
					t.Error("CSRF-less create-sync mutated the store")
				}
				if _, err := f.store.GetSync(context.Background(), "sm-bob"); err != nil {
					t.Error("CSRF-less delete-sync mutated the store")
				}
				if _, err := f.store.GetSyncMember(context.Background(), "sm-bob", "mister"); err == nil {
					t.Error("CSRF-less set-member mutated the store")
				}
				if _, err := f.store.GetSyncMember(context.Background(), "sm-bob", "carol-deck"); err != nil {
					t.Error("CSRF-less del-member mutated the store")
				}
			})
		}
	}
}

// --- slug helper unit tests ----------------------------------------------

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Super Metroid", "super-metroid"},
		{"Super Mario Bros. 3!", "super-mario-bros-3"},
		{"  Chrono  Trigger  ", "chrono-trigger"},
		{"The_Legend of Zelda", "the-legend-of-zelda"},
		{"!!!", ""},
	}
	for _, c := range cases {
		if got := slugify(c.in); got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidSlug(t *testing.T) {
	valid := []string{"super-metroid", "a", "x1", "bob-deck", "a-b-c-1"}
	invalid := []string{"", "Super-Metroid", "-leading", "with space", "with_underscore", "tilde~"}
	for _, s := range valid {
		if !validSlug(s) {
			t.Errorf("validSlug(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if validSlug(s) {
			t.Errorf("validSlug(%q) = true, want false", s)
		}
	}
}
