package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/store"
)

// errExample is a generic non-sentinel error used to exercise the smoke-test
// error-surfacing path (e.g. "the share path is missing").
var errExample = errors.New("boom: share path missing")

// nodeIDs returns the sorted-ish set of node ids currently in the fixture store.
func nodeIDs(t *testing.T, f *actionFixture) map[string]bool {
	t.Helper()
	nodes, err := f.store.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	out := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		out[n.ID] = true
	}
	return out
}

// --- GET /nodes (admin) --------------------------------------------------

func TestNodesPage_AdminSeesRegistry(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "bob") // bob is admin

	req := httptest.NewRequest(http.MethodGet, "/nodes", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"bob-deck", "carol-deck", "mister", "Add a device", "csrf_token"} {
		if !strings.Contains(body, want) {
			t.Errorf("nodes page missing %q", want)
		}
	}
}

func TestNodesPage_NonAdminForbidden(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "carol") // carol is a regular user

	req := httptest.NewRequest(http.MethodGet, "/nodes", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET /nodes = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Admins only") {
		t.Errorf("expected an 'Admins only' page, got: %s", rec.Body.String())
	}
}

// --- POST /api/nodes (create) --------------------------------------------

func TestCreateNode_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/nodes", url.Values{
		"id": {"new-deck"}, "display": {"New Deck"},
		"owner_user_id": {"bob"}, "kind": {"deck"},
		"reach": {"syncthing-share"}, "path": {"/srv/new"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	n, err := f.store.GetNode(context.Background(), "new-deck")
	if err != nil {
		t.Fatalf("node not created: %v", err)
	}
	if n.ReachConfig.Path != "/srv/new" || n.OwnerUserID == nil || *n.OwnerUserID != "bob" {
		t.Errorf("created node wrong: %+v", n)
	}
}

func TestCreateNode_DuplicateID_409(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/nodes", url.Values{
		"id": {"bob-deck"}, "display": {"Dupe"}, "kind": {"deck"},
		"reach": {"syncthing-share"}, "path": {"/srv/x"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate id status = %d, want 409", rec.Code)
	}
}

func TestCreateNode_BadKind_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/nodes", url.Values{
		"id": {"x"}, "display": {"X"}, "kind": {"toaster"},
		"reach": {"syncthing-share"}, "path": {"/srv/x"},
	})
	// A bad kind fails the local enum check first -> 400. (Store's ErrInvalidValue
	// would 422, but we validate before reaching the Store.)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind status = %d, want 400", rec.Code)
	}
	if nodeIDs(t, f)["x"] {
		t.Error("node was created despite bad kind")
	}
}

func TestCreateNode_BadReach_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/nodes", url.Values{
		"id": {"x"}, "display": {"X"}, "kind": {"deck"},
		"reach": {"carrier-pigeon"}, "path": {"/srv/x"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad reach status = %d, want 400", rec.Code)
	}
}

func TestCreateNode_SyncthingMissingPath_400(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/nodes", url.Values{
		"id": {"x"}, "display": {"X"}, "kind": {"deck"},
		"reach": {"syncthing-share"}, // no path
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing path status = %d, want 400", rec.Code)
	}
	if nodeIDs(t, f)["x"] {
		t.Error("node created despite missing path")
	}
}

// TestCreateNode_SSHRejected: `ssh` was dropped in 0010 (the MiSTer runs
// Syncthing, so the strategy never had a device and was never implemented).
// It must be REFUSED now, not accepted-and-inert — an accepted ssh node was a
// node that silently never synced, which is the trap this removal closes.
func TestCreateNode_SSHRejected(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/nodes", url.Values{
		"id": {"mister-2"}, "display": {"MiSTer 2"}, "kind": {"mister"},
		"reach": {"ssh"}, "host": {"10.0.0.9"}, "user": {"root"}, "secret_ref": {"mister-2-key"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ssh create status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	if nodeIDs(t, f)["mister-2"] {
		t.Error("an ssh node was created; the strategy no longer exists")
	}

	// A MiSTer is registered like any other device: it runs Syncthing.
	rec = postForm(t, f, c, csrf, "/api/nodes", url.Values{
		"id": {"mister-2"}, "display": {"MiSTer 2"}, "kind": {"mister"},
		"reach": {"syncthing-share"}, "path": {"/shares/mister-saves"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("mister create status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	n, err := f.store.GetNode(context.Background(), "mister-2")
	if err != nil {
		t.Fatalf("mister node not created: %v", err)
	}
	if n.Kind != store.KindMister || n.ReachConfig.Path != "/shares/mister-saves" {
		t.Errorf("mister node wrong: %+v", n)
	}
}

func TestCreateNode_BadOwnerFK_422(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/nodes", url.Values{
		"id": {"x"}, "display": {"X"}, "owner_user_id": {"ghost"},
		"kind": {"deck"}, "reach": {"syncthing-share"}, "path": {"/srv/x"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad owner FK status = %d, want 422", rec.Code)
	}
}

// --- POST /api/nodes/{id} (edit) -----------------------------------------

func TestEditNode_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/nodes/bob-deck", url.Values{
		"display": {"Bob's NEW Deck"}, "owner_user_id": {"bob"},
		"kind": {"deck"}, "reach": {"syncthing-share"}, "path": {"/srv/renamed"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	n, err := f.store.GetNode(context.Background(), "bob-deck")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if n.Display != "Bob's NEW Deck" || n.ReachConfig.Path != "/srv/renamed" {
		t.Errorf("edit did not apply: %+v", n)
	}
}

// --- POST /api/nodes/{id}/delete -----------------------------------------

func TestDeleteNode_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/nodes/mister/delete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if nodeIDs(t, f)["mister"] {
		t.Error("node not deleted")
	}
}

// TestDeleteNode_MemberNode_CascadesMembership: under auto-mirror a node that is
// a sync member is freely deletable — its membership (and manifest) cascade, and
// the removed member's file just stops mirroring. There is no active-binding FK
// to block it anymore.
func TestDeleteNode_MemberNode_CascadesMembership(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	c, csrf := loginAs(t, f, "bob")

	// bob-deck is a member of sm-bob in the fixture; deleting it should succeed.
	rec := postForm(t, f, c, csrf, "/api/nodes/bob-deck/delete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("member-node delete status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if nodeIDs(t, f)["bob-deck"] {
		t.Error("node not deleted")
	}
	// Its sync membership cascaded away.
	if _, err := f.store.GetSyncMember(ctx, "sm-bob", "bob-deck"); err != store.ErrNotFound {
		t.Errorf("membership not cascaded, err = %v", err)
	}
}

// --- POST /api/nodes/{id}/smoke-test -------------------------------------

// TestSmokeTest_Reachable: a reachable node returns the transient "reachable"
// result. The smoke-test is a live probe only — it persists nothing (there is no
// last_seen / reachable storage anymore).
func TestSmokeTest_Reachable(t *testing.T) {
	f := newActionFixture(t)
	f.act.smokeErr = nil // reachable
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/nodes/bob-deck/smoke-test", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("smoke-test status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "reachable") {
		t.Errorf("expected 'reachable', got: %s", rec.Body.String())
	}
	if got := f.act.smokeTestedNodes(); len(got) != 1 || got[0] != "bob-deck" {
		t.Errorf("SmokeTest called for %v, want [bob-deck]", got)
	}
}

func TestSmokeTest_ErrorSurfaced(t *testing.T) {
	f := newActionFixture(t)
	f.act.smokeErr = errExample
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/nodes/bob-deck/smoke-test", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("smoke-test status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "not reachable") || !strings.Contains(body, "boom") {
		t.Errorf("expected surfaced error, got: %s", body)
	}
}

// A reach the resolver cannot serve reports as a message rather than an error
// pill. Every strategy the registry accepts has an adapter, so this now means a
// node whose stored reach is one nothing can serve — a data fault, but the admin
// still gets a sentence instead of a 500.
func TestSmokeTest_UnsupportedReachMessage(t *testing.T) {
	f := newActionFixture(t)
	f.act.smokeErr = engine.ErrSmokeTestUnsupported
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/nodes/mister/smoke-test", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("smoke-test status = %d, want 200", rec.Code)
	}
	// Assert on an apostrophe-free substring: html/template escapes ' to &#39;,
	// so copy containing one will not match a naive Contains.
	if !strings.Contains(rec.Body.String(), "reach setting RetroSync cannot use") {
		t.Errorf("expected an unsupported-reach message, got: %s", rec.Body.String())
	}
}

// --- admin-gating across every mutation ----------------------------------

// cleanBrowseDir must flatten only genuinely escaping values: ".." itself or a
// "../" prefix. A directory NAME that merely starts with dots ("..data" — k8s
// volume mounts create these) is legitimate under safepath and must survive, or
// the folder picker's "Use this folder" value silently points at the wrong
// directory (the share root instead of root/..data).
func TestCleanBrowseDir(t *testing.T) {
	cases := map[string]string{
		"":                   "",
		".":                  "",
		"/":                  "",
		"..":                 "",
		"../etc":             "",
		"a/../../etc":        "",
		"/etc/passwd":        "",
		"saves":              "saves",
		"saves/deep":         "saves/deep",
		"saves/./deep":       "saves/deep",
		"..data":             "..data",
		"..data/saves":       "..data/saves",
		"saves/..2025_01_01": "saves/..2025_01_01",
	}
	for raw, want := range cases {
		if got := cleanBrowseDir(raw); got != want {
			t.Errorf("cleanBrowseDir(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestNodesMutations_NonAdminForbidden_StoreUntouched(t *testing.T) {
	mutations := []struct {
		name, path string
		fields     url.Values
	}{
		{"create", "/api/nodes", url.Values{"id": {"hax"}, "display": {"Hax"}, "kind": {"deck"}, "reach": {"syncthing-share"}, "path": {"/srv/x"}}},
		{"edit", "/api/nodes/bob-deck", url.Values{"display": {"Hijacked"}, "kind": {"deck"}, "reach": {"syncthing-share"}, "path": {"/srv/x"}}},
		{"delete", "/api/nodes/mister/delete", nil},
		{"smoke-test", "/api/nodes/bob-deck/smoke-test", nil},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			f := newActionFixture(t)
			c, csrf := loginAs(t, f, "carol") // regular user, with a valid CSRF token

			rec := postForm(t, f, c, csrf, m.path, m.fields)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s as non-admin = %d, want 403", m.name, rec.Code)
			}
			// Store untouched: the new node never appears; bob-deck keeps its name;
			// mister still exists; SmokeTest never called.
			ids := nodeIDs(t, f)
			if ids["hax"] {
				t.Error("non-admin created a node")
			}
			if !ids["mister"] {
				t.Error("non-admin deleted a node")
			}
			n, _ := f.store.GetNode(context.Background(), "bob-deck")
			if n.Display == "Hijacked" {
				t.Error("non-admin edited a node")
			}
			if len(f.act.smokeTestedNodes()) != 0 {
				t.Error("non-admin reached SmokeTest")
			}
		})
	}
}

// --- CSRF across every mutation ------------------------------------------

func TestNodesMutations_CSRF(t *testing.T) {
	mutations := []struct{ name, path string }{
		{"create", "/api/nodes"},
		{"edit", "/api/nodes/bob-deck"},
		{"delete", "/api/nodes/mister/delete"},
		{"smoke-test", "/api/nodes/bob-deck/smoke-test"},
	}
	for _, m := range mutations {
		for _, tc := range []struct{ label, token string }{
			{"no-token", ""},
			{"wrong-token", "not-the-real-token"},
		} {
			t.Run(m.name+"/"+tc.label, func(t *testing.T) {
				f := newActionFixture(t)
				c, _ := loginAs(t, f, "bob") // admin, but bad/no CSRF token

				fields := url.Values{"id": {"csrfnode"}, "display": {"X"}, "kind": {"deck"}, "reach": {"syncthing-share"}, "path": {"/srv/x"}}
				rec := postForm(t, f, c, tc.token, m.path, fields)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s %s = %d, want 403", m.name, tc.label, rec.Code)
				}
				// Store untouched.
				ids := nodeIDs(t, f)
				if ids["csrfnode"] {
					t.Error("CSRF-less create mutated the store")
				}
				if !ids["mister"] {
					t.Error("CSRF-less delete mutated the store")
				}
				if len(f.act.smokeTestedNodes()) != 0 {
					t.Error("CSRF-less request reached SmokeTest")
				}
			})
		}
	}
}

// --- secrets discipline --------------------------------------------------

// TestSecretNeverEchoedOrStored confirms a stray cleartext "password"/"secret"
// field submitted alongside a node is NOT stored and NOT echoed back. The form
// parser reads only the fields it knows; anything else a browser, a script, or a
// mistaken operator sends must fall on the floor rather than land in
// reach_config or come back in the response.
func TestSecretNeverEchoedOrStored(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	const leaked = "hunter2-SUPER-SECRET-PASSWORD"
	rec := postForm(t, f, c, csrf, "/api/nodes", url.Values{
		"id": {"mister-x"}, "display": {"MiSTer X"}, "kind": {"mister"},
		"reach": {"syncthing-share"}, "path": {"/shares/mister-x"},
		// Attacker/operator mistakenly pastes a real secret into extra fields.
		"password": {leaked}, "secret": {leaked}, "key": {leaked},
		// Fields belonging to the dropped ssh strategy are likewise ignored.
		"host": {"10.0.0.5"}, "user": {"root"}, "secret_ref": {leaked},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	// The cleartext must not be echoed in the response fragment.
	if strings.Contains(rec.Body.String(), leaked) {
		t.Error("response echoed a cleartext secret")
	}
	// The cleartext must not be stored anywhere in reach_config.
	n, err := f.store.GetNode(context.Background(), "mister-x")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if n.ReachConfig.Path != "/shares/mister-x" {
		t.Errorf("path = %q, want the submitted share path", n.ReachConfig.Path)
	}
	if strings.Contains(n.ReachConfig.Path, leaked) {
		t.Error("a cleartext secret was stored in reach_config")
	}
	// And it must not appear when the node is rendered on the registry page.
	page := httptest.NewRequest(http.MethodGet, "/nodes", nil)
	page.AddCookie(c)
	prec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(prec, page)
	if strings.Contains(prec.Body.String(), leaked) {
		t.Error("nodes page rendered a cleartext secret")
	}
}
