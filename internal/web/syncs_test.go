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
	for _, want := range []string{"sm-bob", "Super Metroid", "Create sync", "csrf_token", "bob-deck"} {
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

// TestCreateSync_WithMemberRows_HappyPath: the slice-30 one-shot form submits
// game + name + repeated node_id/path rows; the sync and all its members are
// created together in a single POST.
func TestCreateSync_WithMemberRows_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")
	ctx := context.Background()

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"id":      {"sm-oneshot"},
		"game":    {"Super Metroid"},
		"name":    {"One shot"},
		"node_id": {"mister", "bob-deck"},
		"path":    {"a.srm", "b.srm"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.GetSync(ctx, "sm-oneshot"); err != nil {
		t.Fatalf("sync not created: %v", err)
	}
	members, err := f.store.ListSyncMembers(ctx, "sm-oneshot")
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
	if byNode["mister"] != "a.srm" || byNode["bob-deck"] != "b.srm" {
		t.Errorf("members wrong: %+v", byNode)
	}
}

// TestCreateSync_ZeroMembers_BlankRow: the single default member row left blank
// (a node selected, empty path) must still create a memberless sync — the
// pre-slice-30 behavior.
func TestCreateSync_ZeroMembers_BlankRow(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")
	ctx := context.Background()

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"id":      {"sm-empty"},
		"game":    {"Super Metroid"},
		"name":    {"Empty"},
		"node_id": {"mister"},
		"path":    {""}, // blank row adds no member
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.GetSync(ctx, "sm-empty"); err != nil {
		t.Fatalf("sync not created: %v", err)
	}
	members, err := f.store.ListSyncMembers(ctx, "sm-empty")
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("members len = %d, want 0: %+v", len(members), members)
	}
}

// TestCreateSync_WithMemberRows_InvalidNode_422_SyncLeftInPlace: a member naming
// a missing device 422s, and the created sync deliberately STAYS (no compensating
// DeleteSync — it could cascade-destroy an engine capture; see handleCreateSync).
// The message must say the sync exists so the admin can fix it in the registry.
func TestCreateSync_WithMemberRows_InvalidNode_422_SyncLeftInPlace(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"id":      {"sm-ghost"},
		"game":    {"Super Metroid"},
		"name":    {"Ghost"},
		"node_id": {"ghost-node"},
		"path":    {"a.srm"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid-node status = %d, want 422\n%s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.GetSync(context.Background(), "sm-ghost"); err != nil {
		t.Errorf("partial sync should be left in place, err = %v", err)
	}
	if !strings.Contains(rec.Body.String(), "the sync was created") {
		t.Errorf("message should say the sync was created, got: %s", rec.Body.String())
	}
}

// TestCreateSync_WithMemberRows_DuplicateMember_409_SyncLeftInPlace: a member
// whose (node,path) is already in ANOTHER sync 409s; the created sync stays in
// place (see handleCreateSync for why there is no rollback) and, critically, the
// original claiming sync's member is untouched.
func TestCreateSync_WithMemberRows_DuplicateMember_409_SyncLeftInPlace(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	// The fixture's sm-bob already claims (bob-deck, "sm.srm").
	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"id":      {"sm-dupe"},
		"game":    {"Super Metroid"},
		"name":    {"Dupe"},
		"node_id": {"bob-deck"},
		"path":    {"sm.srm"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("dup-member status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "another sync") {
		t.Errorf("expected 'already in another sync' message, got: %s", rec.Body.String())
	}
	if _, err := f.store.GetSync(context.Background(), "sm-dupe"); err != nil {
		t.Errorf("partial sync should be left in place, err = %v", err)
	}
	// The original member must be undisturbed.
	if m, err := f.store.GetSyncMember(context.Background(), "sm-bob", "bob-deck"); err != nil || m.Path != "sm.srm" {
		t.Errorf("original member disturbed: %+v err=%v", m, err)
	}
}

// TestCreateSync_MemberRow_BadPath_400_NoSync: a member row with an absolute or
// traversal path is rejected (lexical gate) before any sync is created.
func TestCreateSync_MemberRow_BadPath_400_NoSync(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/syncs", url.Values{
		"id":      {"sm-evil"},
		"game":    {"Super Metroid"},
		"name":    {"Evil"},
		"node_id": {"bob-deck"},
		"path":    {"../../etc/passwd"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad-path status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.GetSync(context.Background(), "sm-evil"); err != store.ErrNotFound {
		t.Errorf("sync created despite a bad member path, err = %v", err)
	}
}

// TestSyncsPage_DiscoverCalloutAndManualForm: the /syncs page fronts Discover
// with a callout and demotes manual creation into a collapsed <details>.
func TestSyncsPage_DiscoverCalloutAndManualForm(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "bob")

	req := httptest.NewRequest(http.MethodGet, "/syncs", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Discover is the easy way",
		"/discover",
		"Create a sync manually",
		// The row cloner is link-styled and verb-less so it cannot be read as a
		// save; assert the class too, since the wording alone is the thing that
		// confused a real user.
		`class="linkbtn add-member-row"`,
		"+ another device",
		// The greyed-until-valid submit is a CSS rule keyed on
		// `.sync-create:has(:invalid)`, so it only works while the form carries
		// that class AND its two required fields are marked required. Pin both;
		// dropping either silently disables the affordance.
		`class="sync-form sync-create card"`,
		`name="game" required`,
		`name="name" required`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("syncs page missing %q", want)
		}
	}
}

// TestSyncsPage_EmptyStatePointsToDiscover: with no syncs and no active search,
// the empty list points at Discover; an empty *search* result stays a plain
// "No syncs match."
func TestSyncsPage_EmptyStatePointsToDiscover(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	// Drop the seeded sync so the registry is empty.
	if err := f.store.DeleteSync(ctx, "sm-bob"); err != nil {
		t.Fatalf("delete sync: %v", err)
	}
	c, _ := loginAs(t, f, "bob")

	// No query: point at Discover.
	req := httptest.NewRequest(http.MethodGet, "/syncs", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if body := rec.Body.String(); !strings.Contains(body, "No syncs yet") || !strings.Contains(body, "/discover") {
		t.Errorf("empty (no-query) state should point at Discover, got: %s", body)
	}

	// Active search with no match: stay a plain "No syncs match."
	req = httptest.NewRequest(http.MethodGet, "/syncs?q=zzz", nil)
	req.AddCookie(c)
	req.Header.Set("HX-Request", "true")
	rec = httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if body := rec.Body.String(); !strings.Contains(body, "No syncs match.") {
		t.Errorf("empty search result should stay a plain no-match, got: %s", body)
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

// TestSetSyncMember_PathValidation: member paths are lexically validated at
// form-parse time (mirrors safepath's rule): absolute paths, ".." segments,
// and backslashes are 400'd before the Store, since a persisted bad path would
// silently halt polling for the whole sync. Legal-but-odd names ("Mario &
// Luigi.srm") stay accepted.
func TestSetSyncMember_PathValidation(t *testing.T) {
	cases := []struct {
		name, path string
		wantStatus int
	}{
		{"plain-file", "sm2.srm", http.StatusOK},
		{"subdir", "saves/sm2.srm", http.StatusOK},
		{"ampersand-name", "Mario & Luigi.srm", http.StatusOK},
		{"dotdot-prefix", "../x.srm", http.StatusBadRequest},
		{"absolute", "/etc/passwd", http.StatusBadRequest},
		{"dotdot-inner", "a/../../x.srm", http.StatusBadRequest},
		{"dotdot-collapsible", "a/../b.srm", http.StatusBadRequest},
		{"bare-dotdot", "..", http.StatusBadRequest},
		{"backslash", "saves\\sm2.srm", http.StatusBadRequest},
		{"dot-slash-only", "./", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newActionFixture(t)
			c, csrf := loginAs(t, f, "bob")

			rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/members/mister", url.Values{
				"path": {tc.path},
			})
			if rec.Code != tc.wantStatus {
				t.Fatalf("path %q status = %d, want %d\n%s", tc.path, rec.Code, tc.wantStatus, rec.Body.String())
			}
			_, err := f.store.GetSyncMember(context.Background(), "sm-bob", "mister")
			if tc.wantStatus == http.StatusOK && err != nil {
				t.Errorf("accepted path %q not stored: %v", tc.path, err)
			}
			if tc.wantStatus != http.StatusOK && err == nil {
				t.Errorf("rejected path %q reached the store", tc.path)
			}
		})
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

// --- shared refresh ---------------------------------------------------------

// TestRefreshSyncsList_PreservesFilterFromHXCurrentURL: a mutation POST's own
// URL never carries ?q= — HTMX puts the page URL (with the live search filter)
// in the HX-Current-URL header. The refreshed fragment must honor that filter
// instead of silently unfiltering the list.
func TestRefreshSyncsList_PreservesFilterFromHXCurrentURL(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	if err := f.store.CreateSync(ctx, store.Sync{ID: "z-link", Game: "Zelda", Name: "Link's stream"}); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	c, csrf := loginAs(t, f, "bob")

	form := url.Values{"name": {"Renamed stream"}, "game": {"Super Metroid"}}
	req := httptest.NewRequest(http.MethodPost, "/api/syncs/sm-bob", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost/syncs?q=metroid")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mutation status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "sm-bob") {
		t.Error("filtered refresh should include the matching (metroid) sync")
	}
	if strings.Contains(body, "z-link") {
		t.Error("mutation dropped the q=metroid filter: the Zelda sync leaked into the refreshed list")
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

func TestValidMemberPath(t *testing.T) {
	valid := []string{"sm.srm", "saves/sm.srm", "a/b/c.srm", "Mario & Luigi.srm", "with space.sav"}
	invalid := []string{"", "/abs.srm", "../x", "a/../../x", "a/../b", "..", "a\\b", ".", "./"}
	for _, p := range valid {
		if !validMemberPath(p) {
			t.Errorf("validMemberPath(%q) = false, want true", p)
		}
	}
	for _, p := range invalid {
		if validMemberPath(p) {
			t.Errorf("validMemberPath(%q) = true, want false", p)
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
