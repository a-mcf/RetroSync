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

// syncIDsMap returns the set of all sync ids currently in the fixture store.
func syncIDsMap(t *testing.T, f *actionFixture) map[string]bool {
	t.Helper()
	syncs, err := f.store.ListSyncs(context.Background())
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

// seedConflict marks the sync sm-bob as conflicted, so the registry delete guard
// (delete-sync) has a conflicted sync to refuse against. Under auto-mirror that
// is the only registry guard left.
func seedConflict(t *testing.T, f *actionFixture) {
	t.Helper()
	conflict := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	if err := f.store.SetSyncConflict(context.Background(), "sm-bob", &conflict); err != nil {
		t.Fatalf("seed conflict: %v", err)
	}
}

// --- GET /syncs (admin) --------------------------------------------------

func TestSyncsPage_AdminSeesRegistry(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "bob") // admin

	req := httptest.NewRequest(http.MethodGet, "/syncs", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"sm-bob", "Super Metroid", "New sync", "csrf_token", "bob-deck"} {
		if !strings.Contains(body, want) {
			t.Errorf("syncs page missing %q", want)
		}
	}
}

func TestSyncsPage_NonAdminForbidden(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "carol") // regular user

	req := httptest.NewRequest(http.MethodGet, "/syncs", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET /syncs = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Admins only") {
		t.Errorf("expected an 'Admins only' page, got: %s", rec.Body.String())
	}
}

func TestSyncsPage_SearchFilter(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	if err := f.store.CreateSync(ctx, store.Sync{ID: "z-link", Game: "Zelda", Name: "Link's stream"}); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	c, _ := loginAs(t, f, "bob")

	// HX-Request so the handler returns just the filtered syncs-list fragment.
	req := httptest.NewRequest(http.MethodGet, "/syncs?q=zel", nil)
	req.AddCookie(c)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "z-link") {
		t.Error("search q=zel should match the Zelda sync")
	}
	if strings.Contains(body, "sm-bob") {
		t.Error("search q=zel should not match the Super Metroid sync")
	}
}

// --- POST /api/syncs (create) --------------------------------------------

func TestCreateSync_AutoID(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"game": {"Super Metroid"}, "name": {"Alice stream"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create sync status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	// Auto-id is slugify(game + "-" + name).
	if _, err := f.store.GetSync(context.Background(), "super-metroid-alice-stream"); err != nil {
		t.Fatalf("auto-id sync not created: %v\n%s", err, syncIDs(t, f))
	}
}

func TestCreateSync_ExplicitID(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"id": {"sm-alice"}, "game": {"Super Metroid"}, "name": {"Alice stream"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create sync status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	sy, err := f.store.GetSync(context.Background(), "sm-alice")
	if err != nil || sy.Name != "Alice stream" || sy.Game != "Super Metroid" {
		t.Fatalf("sync wrong: %+v err=%v", sy, err)
	}
}

// TestCreateSync_ArbitraryGameLabel asserts the game label is free text: a label
// naming no registry row (there is no registry) is accepted, not rejected.
func TestCreateSync_ArbitraryGameLabel(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"id": {"brand-new"}, "game": {"Some Brand New Title"}, "name": {"Stream"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("arbitrary label create status = %d, want 200 (label is free text)\n%s", rec.Code, rec.Body.String())
	}
	sy, err := f.store.GetSync(context.Background(), "brand-new")
	if err != nil || sy.Game != "Some Brand New Title" {
		t.Fatalf("sync wrong: %+v err=%v", sy, err)
	}
}

func TestCreateSync_DuplicateID_409(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"id": {"sm-bob"}, "game": {"Super Metroid"}, "name": {"Dupe"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate sync id status = %d, want 409", rec.Code)
	}
}

func TestCreateSync_MissingGame_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"name": {"Stream"}, // no game label
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing game label status = %d, want 400", rec.Code)
	}
}

func TestCreateSync_BadSlug_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	// Explicit id with spaces/caps -> invalid slug -> 400.
	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"id": {"Bad ID"}, "game": {"Super Metroid"}, "name": {"Stream"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad slug status = %d, want 400", rec.Code)
	}
}

func TestCreateSync_MissingName_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"game": {"Super Metroid"}, // no name
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing name status = %d, want 400", rec.Code)
	}
}

// --- POST /api/syncs/{id} (edit: rename + relabel) -----------------------

func TestRenameSync_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob", url.Values{
		"name": {"Renamed stream"}, "game": {"Super Metroid (USA)"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	sy, _ := f.store.GetSync(context.Background(), "sm-bob")
	if sy.Name != "Renamed stream" || sy.Game != "Super Metroid (USA)" {
		t.Errorf("rename/relabel did not apply: %+v", sy)
	}
}

func TestRenameSync_MissingName_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob", url.Values{"game": {"X"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rename missing name status = %d, want 400", rec.Code)
	}
}

func TestRenameSync_Missing_404(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs/ghost", url.Values{"name": {"X"}, "game": {"X"}})
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
	if err := f.store.CreateSync(ctx, store.Sync{ID: "sm-other", Game: "Super Metroid", Name: "Other"}); err != nil {
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

func TestSyncsMutations_NonAdminForbidden_StoreUntouched(t *testing.T) {
	mutations := []struct {
		name, path string
		fields     url.Values
	}{
		{"create-sync", "/api/syncs", url.Values{"id": {"hax-sync"}, "game": {"Super Metroid"}, "name": {"Hax"}}},
		{"rename-sync", "/api/syncs/sm-bob", url.Values{"name": {"Hijacked"}, "game": {"Hijacked"}}},
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

func TestSyncsMutations_CSRF(t *testing.T) {
	mutations := []struct {
		name, path string
		fields     url.Values
	}{
		{"create-sync", "/api/syncs", url.Values{"id": {"csrfsync"}, "game": {"Super Metroid"}, "name": {"X"}}},
		{"rename-sync", "/api/syncs/sm-bob", url.Values{"name": {"X"}, "game": {"X"}}},
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
