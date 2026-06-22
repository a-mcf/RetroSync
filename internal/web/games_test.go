package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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

// seedBinding makes the sync sm-bob (of game super-metroid) active with the
// given primary node — so the game's registry delete/remove-path guards have an
// active sync to refuse against.
func seedBinding(t *testing.T, f *actionFixture, primary string) {
	t.Helper()
	if err := f.store.CreateBinding(context.Background(), store.ActiveBinding{
		SyncID: "sm-bob", PrimaryNode: primary, Direction: "from-primary",
	}); err != nil {
		t.Fatalf("create binding: %v", err)
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

func TestDeleteGame_Active_Friendly409(t *testing.T) {
	f := newActionFixture(t)
	seedBinding(t, f, "bob-deck")
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/games/super-metroid/delete", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("active delete status = %d, want 409 (not 500)", rec.Code)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "played right now") {
		t.Errorf("expected a friendly 'played right now' message, got: %s", rec.Body.String())
	}
	if !gameIDs(t, f)["super-metroid"] {
		t.Error("game deleted despite being active")
	}
}

// --- POST /api/games/{id}/paths/{node_id} (add/update) -------------------

func TestSetGamePath_AddAndUpdate(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")
	ctx := context.Background()

	// Add a new mapping on mister (not seeded with one).
	rec := postForm(t, f, c, csrf, "/api/games/super-metroid/paths/mister", url.Values{
		"path": {"SNES/Super Metroid.sav"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("add path status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	gp, err := f.store.GetGamePath(ctx, "super-metroid", "mister")
	if err != nil || gp.Path != "SNES/Super Metroid.sav" {
		t.Fatalf("path not added: %+v err=%v", gp, err)
	}

	// Update the same mapping (upsert).
	rec = postForm(t, f, c, csrf, "/api/games/super-metroid/paths/mister", url.Values{
		"path": {"SNES/SM.sav"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update path status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	gp, _ = f.store.GetGamePath(ctx, "super-metroid", "mister")
	if gp.Path != "SNES/SM.sav" {
		t.Errorf("path not updated: %+v", gp)
	}
}

func TestSetGamePath_MissingNode_422(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/games/super-metroid/paths/ghost-node", url.Values{
		"path": {"x.srm"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing node status = %d, want 422", rec.Code)
	}
}

func TestSetGamePath_EmptyPath_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/games/super-metroid/paths/mister", url.Values{
		"path": {"  "},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty path status = %d, want 400", rec.Code)
	}
}

// --- POST /api/games/{id}/paths/{node_id}/delete (remove) ----------------

func TestDeleteGamePath_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/games/super-metroid/paths/carol-deck/delete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete path status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.GetGamePath(context.Background(), "super-metroid", "carol-deck"); err != store.ErrNotFound {
		t.Errorf("path not removed, err = %v", err)
	}
}

func TestDeleteGamePath_ActivePrimary_Friendly409(t *testing.T) {
	f := newActionFixture(t)
	seedBinding(t, f, "bob-deck") // bob-deck is the active primary
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/games/super-metroid/paths/bob-deck/delete", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("active-primary remove status = %d, want 409", rec.Code)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "active primary") {
		t.Errorf("expected a friendly 'active primary' message, got: %s", rec.Body.String())
	}
	// The mapping must remain.
	if _, err := f.store.GetGamePath(context.Background(), "super-metroid", "bob-deck"); err != nil {
		t.Errorf("active-primary path was removed despite guard: %v", err)
	}
}

func TestDeleteGamePath_NonPrimaryWhileActive_OK(t *testing.T) {
	f := newActionFixture(t)
	seedBinding(t, f, "bob-deck") // bob-deck primary; carol-deck is a peer
	c, csrf := loginAs(t, f, "bob")

	// Removing carol-deck (not the primary) is allowed even while active.
	rec := postForm(t, f, c, csrf, "/api/games/super-metroid/paths/carol-deck/delete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-primary remove status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.GetGamePath(context.Background(), "super-metroid", "carol-deck"); err != store.ErrNotFound {
		t.Errorf("non-primary path not removed: %v", err)
	}
}

// --- admin-gating across every mutation ----------------------------------

func TestGamesMutations_NonAdminForbidden_StoreUntouched(t *testing.T) {
	mutations := []struct {
		name, path string
		fields     url.Values
	}{
		{"create", "/api/games", url.Values{"id": {"hax"}, "display": {"Hax"}, "system": {"snes"}}},
		{"edit", "/api/games/super-metroid", url.Values{"display": {"Hijacked"}, "system": {"snes"}}},
		{"delete", "/api/games/super-metroid/delete", nil},
		{"set-path", "/api/games/super-metroid/paths/mister", url.Values{"path": {"x.sav"}}},
		{"del-path", "/api/games/super-metroid/paths/carol-deck/delete", nil},
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
			if _, err := f.store.GetGamePath(context.Background(), "super-metroid", "mister"); err == nil {
				t.Error("non-admin added a path mapping")
			}
			if _, err := f.store.GetGamePath(context.Background(), "super-metroid", "carol-deck"); err != nil {
				t.Error("non-admin removed a path mapping")
			}
		})
	}
}

// --- CSRF across every mutation ------------------------------------------

func TestGamesMutations_CSRF(t *testing.T) {
	mutations := []struct{ name, path string }{
		{"create", "/api/games"},
		{"edit", "/api/games/super-metroid"},
		{"delete", "/api/games/super-metroid/delete"},
		{"set-path", "/api/games/super-metroid/paths/mister"},
		{"del-path", "/api/games/super-metroid/paths/carol-deck/delete"},
	}
	for _, m := range mutations {
		for _, tc := range []struct{ label, token string }{
			{"no-token", ""},
			{"wrong-token", "not-the-real-token"},
		} {
			t.Run(m.name+"/"+tc.label, func(t *testing.T) {
				f := newActionFixture(t)
				c, _ := loginAs(t, f, "bob") // admin, bad/no CSRF

				fields := url.Values{"id": {"csrfgame"}, "display": {"X"}, "system": {"snes"}, "path": {"x.sav"}}
				rec := postForm(t, f, c, tc.token, m.path, fields)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s %s = %d, want 403", m.name, tc.label, rec.Code)
				}
				if gameIDs(t, f)["csrfgame"] {
					t.Error("CSRF-less create mutated the store")
				}
				if !gameIDs(t, f)["super-metroid"] {
					t.Error("CSRF-less delete mutated the store")
				}
				if _, err := f.store.GetGamePath(context.Background(), "super-metroid", "mister"); err == nil {
					t.Error("CSRF-less set-path mutated the store")
				}
				if _, err := f.store.GetGamePath(context.Background(), "super-metroid", "carol-deck"); err != nil {
					t.Error("CSRF-less del-path mutated the store")
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
