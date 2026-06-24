package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/auth"
	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/fakereach"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

// seedVersions captures a couple of save versions for bob-deck in sm-bob so the
// history page has rows to render. Returns the seq of the newest version.
func seedVersions(t *testing.T, f *actionFixture) int64 {
	t.Helper()
	ctx := context.Background()
	if err := f.store.PutSaveVersion(ctx, "sm-bob", "bob-deck", "h-old", []byte("old-bytes"), "propagate"); err != nil {
		t.Fatalf("put version old: %v", err)
	}
	if err := f.store.PutSaveVersion(ctx, "sm-bob", "bob-deck", "h-new", []byte("newer-bytes"), "conflict-resolve"); err != nil {
		t.Fatalf("put version new: %v", err)
	}
	vs, err := f.store.ListSaveVersions(ctx, "sm-bob", "bob-deck", 0)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(vs) == 0 {
		t.Fatal("no versions seeded")
	}
	return vs[0].Seq // newest-first
}

// --- History page --------------------------------------------------------

// TestHistoryPage_RendersVersions asserts the history page lists each member's
// captured versions (newest-first) with a Restore button that POSTs to the
// restore endpoint with the version's seq.
func TestHistoryPage_RendersVersions(t *testing.T) {
	f := newActionFixture(t)
	newest := seedVersions(t, f)
	c, _ := loginAs(t, f, "carol")

	req := httptest.NewRequest(http.MethodGet, "/syncs/sm-bob/history", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The member node and the two reasons render.
	for _, want := range []string{"bob-deck", "propagate", "conflict-resolve"} {
		if !strings.Contains(body, want) {
			t.Errorf("history page missing %q\n%s", want, body)
		}
	}
	// A Restore button POSTs to the restore endpoint with the newest version's seq.
	wantPost := "/api/syncs/sm-bob/versions/" + itoa(newest) + "/restore"
	if !strings.Contains(body, `hx-post="`+wantPost+`"`) {
		t.Errorf("history page missing restore POST to %q\n%s", wantPost, body)
	}
	// CSRF embedded.
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Errorf("history page missing csrf_token field\n%s", body)
	}
}

// TestHistoryPage_ViewableByNonOwner asserts the read-only history page is NOT
// owner-gated (any authenticated user may view it, like the conflict modal).
func TestHistoryPage_ViewableByNonOwner(t *testing.T) {
	f := newActionFixture(t)
	seedVersions(t, f)
	// carol does not own bob-deck but may still view history.
	c, _ := loginAs(t, f, "carol")
	req := httptest.NewRequest(http.MethodGet, "/syncs/sm-bob/history", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-owner history status = %d, want 200", rec.Code)
	}
}

func TestHistoryPage_NoSuchSync_404(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "bob")
	req := httptest.NewRequest(http.MethodGet, "/syncs/ghost/history", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("history of missing sync = %d, want 404", rec.Code)
	}
}

// --- POST restore --------------------------------------------------------

func TestRestoreVersion_MemberOwner_CallsActionerAndRefreshes(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "carol") // owns carol-deck, a member of sm-bob

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/versions/7/restore", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	calls := f.act.restoreCalls()
	if len(calls) != 1 || calls[0].seq != 7 || calls[0].syncID != "sm-bob" {
		t.Fatalf("restore calls = %v, want [{sm-bob 7}]", calls)
	}
	if !strings.Contains(rec.Body.String(), `id="dashboard"`) {
		t.Error("restore success did not return the dashboard fragment")
	}
}

func TestRestoreVersion_Admin_Allowed(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob") // admin
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/versions/3/restore", url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin restore status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.restoreCalls()); n != 1 {
		t.Fatalf("restore calls = %d, want 1", n)
	}
}

// TestRestoreVersion_NonMemberOwner_403_NoActionerCall: restore is destructive,
// so it is member-owner/admin-gated with zero engine calls on rejection.
func TestRestoreVersion_NonMemberOwner_403_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	// Remove carol's node so carol owns NO member of sm-bob.
	if err := f.store.DeleteSyncMember(context.Background(), "sm-bob", "carol-deck"); err != nil {
		t.Fatalf("remove carol-deck: %v", err)
	}
	c, csrf := loginAs(t, f, "carol")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/versions/7/restore", url.Values{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner restore status = %d, want 403\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.restoreCalls()); n != 0 {
		t.Fatalf("actioner called %d times for unauthorized restore, want 0", n)
	}
}

func TestRestoreVersion_CSRF_NoToken_403_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "carol")

	rec := postForm(t, f, c, "" /* no csrf */, "/api/syncs/sm-bob/versions/7/restore", url.Values{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no-token restore status = %d, want 403", rec.Code)
	}
	if n := len(f.act.restoreCalls()); n != 0 {
		t.Fatalf("actioner called %d times on missing CSRF, want 0", n)
	}
}

func TestRestoreVersion_CSRF_WrongToken_403_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "carol")

	rec := postForm(t, f, c, "totally-wrong-token", "/api/syncs/sm-bob/versions/7/restore", url.Values{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-token restore status = %d, want 403", rec.Code)
	}
	if n := len(f.act.restoreCalls()); n != 0 {
		t.Fatalf("actioner called %d times on wrong CSRF, want 0", n)
	}
}

// TestRestoreVersion_Pruned_410 maps a store.ErrNotFound from the engine (the
// version was pruned/gone) to a friendly 410 Gone.
func TestRestoreVersion_Pruned_410(t *testing.T) {
	f := newActionFixture(t)
	f.act.restoreErr = store.ErrNotFound
	c, csrf := loginAs(t, f, "carol")

	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/versions/7/restore", url.Values{})
	if rec.Code != http.StatusGone {
		t.Fatalf("pruned restore status = %d, want 410\n%s", rec.Code, rec.Body.String())
	}
}

// TestRestoreVersion_BadSeq_400_NoActionerCall: a non-numeric seq is a 400 and
// never reaches the engine (but only AFTER the authz check passes).
func TestRestoreVersion_BadSeq_400_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob") // admin so authz passes
	rec := postForm(t, f, c, csrf, "/api/syncs/sm-bob/versions/notanumber/restore", url.Values{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad-seq restore status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.restoreCalls()); n != 0 {
		t.Fatalf("actioner called %d times on bad seq, want 0", n)
	}
}

// TestRestoreVersion_NoSuchSync_404_NoActionerCall asserts restoring under a
// non-existent sync is a 404 and never reaches the engine.
func TestRestoreVersion_NoSuchSync_404_NoActionerCall(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob") // admin
	rec := postForm(t, f, c, csrf, "/api/syncs/ghost/versions/7/restore", url.Values{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no-such-sync restore status = %d, want 404\n%s", rec.Code, rec.Body.String())
	}
	if n := len(f.act.restoreCalls()); n != 0 {
		t.Fatalf("actioner called %d times on missing-sync restore, want 0", n)
	}
}

// TestRestoreVersion_CrossSync_Rejected_SyncBUntouched is the CRITICAL authz
// regression: a user authorized on sync A must NOT be able to restore a version
// belonging to sync B by POSTing B's (guessable, bigserial) seq under A's path.
// It wires a REAL engine over a memory store + fakereach so the assertion is
// end-to-end: the cross-sync restore is rejected AND sync B is provably untouched
// — no member of B overwritten, B's manifest and conflict flag unchanged, no new
// version captured against B.
func TestRestoreVersion_CrossSync_Rejected_SyncBUntouched(t *testing.T) {
	ctx := context.Background()
	st := memory.New()

	hash, err := auth.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	// carol owns node-a (a member of sync A only); she owns NOTHING in sync B.
	mustU := func(id string, role store.Role) {
		if err := st.CreateUser(ctx, store.User{ID: id, Display: id, PwHash: hash, Role: role}); err != nil {
			t.Fatalf("create user %s: %v", id, err)
		}
	}
	mustU("carol", store.RoleUser)
	carol := "carol"
	mustN := func(id string, owner *string) {
		if err := st.CreateNode(ctx, store.Node{ID: id, OwnerUserID: owner, Display: id, Kind: store.KindDeck, Reach: store.ReachSyncthingShare}); err != nil {
			t.Fatalf("create node %s: %v", id, err)
		}
	}
	mustN("node-a", &carol)
	mustN("node-b", nil) // carol does NOT own node-b
	for _, id := range []string{"sync-a", "sync-b"} {
		if err := st.CreateSync(ctx, store.Sync{ID: id, Game: "G"}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if err := st.SetSyncMember(ctx, store.SyncMember{SyncID: "sync-a", NodeID: "node-a", Path: "a.srm"}); err != nil {
		t.Fatalf("member a: %v", err)
	}
	if err := st.SetSyncMember(ctx, store.SyncMember{SyncID: "sync-b", NodeID: "node-b", Path: "b.srm"}); err != nil {
		t.Fatalf("member b: %v", err)
	}

	// Sync B's live member holds "B-LIVE"; seed a DIFFERENT captured version
	// ("B-OLD") whose seq the attacker will target.
	bMtime := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	fakeA := fakereach.New()
	fakeA.Put("a.srm", []byte("A-LIVE"), bMtime)
	fakeB := fakereach.New()
	fakeB.Put("b.srm", []byte("B-LIVE"), bMtime)
	bOld := []byte("B-OLD")
	bOldHash := hex.EncodeToString(func() []byte { s := sha256.Sum256(bOld); return s[:] }())
	if err := st.PutSaveVersion(ctx, "sync-b", "node-b", bOldHash, bOld, "propagate"); err != nil {
		t.Fatalf("seed B version: %v", err)
	}
	// Record B's manifest so we can assert it does not move.
	if err := st.SetManifest(ctx, store.ManifestEntry{SyncID: "sync-b", NodeID: "node-b", Mtime: &bMtime}); err != nil {
		t.Fatalf("seed B manifest: %v", err)
	}
	vs, err := st.ListSaveVersions(ctx, "sync-b", "node-b", 0)
	if err != nil || len(vs) != 1 {
		t.Fatalf("seed B version list: %v (%d)", err, len(vs))
	}
	seqOfB := vs[0].Seq

	// A REAL engine is the Actioner.
	resolve := func(n store.Node) (reach.Reach, error) {
		switch n.ID {
		case "node-a":
			return fakeA, nil
		case "node-b":
			return fakeB, nil
		}
		t.Fatalf("no fake for %s", n.ID)
		return nil, nil
	}
	eng := engine.New(st, resolve, nil)
	srv, err := New(st, Options{Actioner: eng})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f := &actionFixture{srv: srv, store: st}

	// carol logs in and POSTs a restore of B's seq under sync A's path.
	c, csrf := loginAs(t, f, "carol")
	rec := postForm(t, f, c, csrf, "/api/syncs/sync-a/versions/"+itoa(seqOfB)+"/restore", url.Values{})

	// Rejected: the seq does not belong to sync A, so the store binding makes it
	// not-found (mapped to 410 Gone). NOT a 200, and certainly not a 500.
	if rec.Code != http.StatusGone {
		t.Fatalf("cross-sync restore status = %d, want 410 (rejected as not-found)\n%s", rec.Code, rec.Body.String())
	}

	// Sync B is provably UNTOUCHED. node-b still holds its live bytes.
	if data, err := fakeB.Read(ctx, "b.srm"); err != nil || string(data) != "B-LIVE" {
		t.Fatalf("sync B member overwritten by cross-sync restore: %q (err %v)", data, err)
	}
	if w := fakeB.Writes(); len(w) != 0 {
		t.Fatalf("engine wrote to sync B during a cross-sync restore: %+v", w)
	}
	// B's manifest mtime did not move.
	if m, err := st.GetManifest(ctx, "sync-b", "node-b"); err != nil || m.Mtime == nil || !m.Mtime.Equal(bMtime) {
		t.Fatalf("sync B manifest moved: %+v (err %v)", m, err)
	}
	// B was never conflicted and stays that way; no spurious last_synced advance.
	syB, err := st.GetSync(ctx, "sync-b")
	if err != nil {
		t.Fatalf("get sync B: %v", err)
	}
	if syB.ConflictAt != nil || syB.LastSynced != nil {
		t.Fatalf("sync B runtime state changed: conflict=%v lastSynced=%v", syB.ConflictAt, syB.LastSynced)
	}
	// No new version captured against B (its single seeded version is unchanged).
	if vs, _ := st.ListSaveVersions(ctx, "sync-b", "node-b", 0); len(vs) != 1 {
		t.Fatalf("cross-sync restore captured a new version against B: now %d", len(vs))
	}
}

// itoa is a tiny base-10 int64 formatter to keep the test free of strconv noise
// in the assertion strings.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
