package web

import (
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/store"
)

// getBrowse issues an authenticated GET against the save-file picker endpoint
// and returns the recorder. cookie==nil sends no session (unauthenticated).
func getBrowse(t *testing.T, f *actionFixture, c *http.Cookie, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestBrowse_AdminListsEntries: an admin GET returns the picker fragment with
// the seeded entries (a folder + a file), and the file row carries the full
// node-relative path it would fill in.
func TestBrowse_AdminListsEntries(t *testing.T) {
	f := newActionFixture(t)
	mt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	f.act.browseEntries = []engine.DirEntry{
		{Name: "saves", IsDir: true},
		{Name: "alpha.srm", IsDir: false, Size: 64, Mtime: mt},
	}
	c, _ := loginAs(t, f, "bob") // admin

	rec := getBrowse(t, f, c, "/api/nodes/bob-deck/browse?path=")
	if rec.Code != http.StatusOK {
		t.Fatalf("browse status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"saves", "alpha.srm", `data-rel="alpha.srm"`} {
		if !strings.Contains(body, want) {
			t.Errorf("browse body missing %q\n%s", want, body)
		}
	}
	// The handler reached the engine with the node id and the (empty) root path.
	calls := f.act.browseCalls()
	if len(calls) != 1 || calls[0].nodeID != "bob-deck" || calls[0].relPath != "" {
		t.Fatalf("browse calls = %+v, want one (bob-deck, \"\")", calls)
	}
}

// TestBrowse_SubdirRelPathPropagatesAndUpLink: browsing a subdir passes the rel
// path to the engine, renders an up-link, and file links carry the joined path.
func TestBrowse_SubdirRelPathPropagatesAndUpLink(t *testing.T) {
	f := newActionFixture(t)
	f.act.browseEntries = []engine.DirEntry{
		{Name: "game.srm", IsDir: false, Size: 10},
	}
	c, _ := loginAs(t, f, "bob")

	rec := getBrowse(t, f, c, "/api/nodes/bob-deck/browse?path=saves")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The file's selectable rel path is the dir joined with the name.
	if !strings.Contains(body, `data-rel="saves/game.srm"`) {
		t.Errorf("file rel path not joined under saves/: %s", body)
	}
	// An up-link to the parent (root) is present.
	if !strings.Contains(body, "browse?path=") || !strings.Contains(body, "up") {
		t.Errorf("expected an up-link at a subdir: %s", body)
	}
	calls := f.act.browseCalls()
	if len(calls) != 1 || calls[0].relPath != "saves" {
		t.Fatalf("browse calls = %+v, want relPath saves", calls)
	}
}

// TestBrowse_SpecialCharDirNavigates: a directory whose name contains URL
// metacharacters ("Mario & Luigi") must render a folder link with a
// URL-ENCODED ?path= (hx-get is not an href, so html/template does no URL
// escaping of its own), and following that link must deliver the decoded path
// to the engine intact.
func TestBrowse_SpecialCharDirNavigates(t *testing.T) {
	f := newActionFixture(t)
	f.act.browseEntries = []engine.DirEntry{
		{Name: "Mario & Luigi", IsDir: true},
	}
	c, _ := loginAs(t, f, "bob")

	rec := getBrowse(t, f, c, "/api/nodes/bob-deck/browse?path=")
	if rec.Code != http.StatusOK {
		t.Fatalf("root browse status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	// html/template additionally HTML-escapes the attribute value (e.g. "+" ->
	// "&#43;"); the browser decodes that before HTMX requests the URL, so
	// assert on the browser-decoded attribute text.
	if !strings.Contains(html.UnescapeString(rec.Body.String()), "browse?path=Mario+%26+Luigi") {
		t.Fatalf("folder link not URL-encoded:\n%s", rec.Body.String())
	}

	// Follow the rendered link: the engine must receive the decoded directory.
	f.act.browseEntries = []engine.DirEntry{{Name: "game.srm", Size: 8}}
	rec = getBrowse(t, f, c, "/api/nodes/bob-deck/browse?path=Mario+%26+Luigi")
	if rec.Code != http.StatusOK {
		t.Fatalf("subdir browse status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	calls := f.act.browseCalls()
	if got := calls[len(calls)-1].relPath; got != "Mario & Luigi" {
		t.Fatalf("engine relPath = %q, want %q", got, "Mario & Luigi")
	}
}

// TestBrowse_SpecialCharParentUpLinkEncoded: browsing a subdirectory OF a
// special-char directory renders an up-link whose ?path= is the URL-encoded
// parent, so the up navigation survives & + % etc. too.
func TestBrowse_SpecialCharParentUpLinkEncoded(t *testing.T) {
	f := newActionFixture(t)
	f.act.browseEntries = []engine.DirEntry{{Name: "game.srm", Size: 8}}
	c, _ := loginAs(t, f, "bob")

	rec := getBrowse(t, f, c, "/api/nodes/bob-deck/browse?path=Mario+%26+Luigi%2Fsaves")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	// See TestBrowse_SpecialCharDirNavigates: compare the browser-decoded
	// attribute text (html/template HTML-escapes "+" inside the attribute).
	if !strings.Contains(html.UnescapeString(rec.Body.String()), "browse?path=Mario+%26+Luigi\"") {
		t.Fatalf("up-link parent not URL-encoded:\n%s", rec.Body.String())
	}
}

// TestBrowse_TraversalRejected400: a path that the engine rejects as unsafe
// (ErrBrowseUnsafePath, the safepath containment failure) maps to 400.
func TestBrowse_TraversalRejected400(t *testing.T) {
	f := newActionFixture(t)
	f.act.browseErr = engine.ErrBrowseUnsafePath
	c, _ := loginAs(t, f, "bob")

	rec := getBrowse(t, f, c, "/api/nodes/bob-deck/browse?path=../../etc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal browse status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	// The hostile path was passed through to the engine verbatim for safepath to
	// reject (the handler does not silently sanitize it away).
	calls := f.act.browseCalls()
	if len(calls) != 1 || calls[0].relPath != "../../etc" {
		t.Fatalf("browse calls = %+v, want the raw hostile path forwarded", calls)
	}
}

// TestBrowse_UnsupportedReachFriendly: a node whose reach cannot be browsed
// returns 200 with a friendly message rather than an error status, so the picker
// explains itself in place instead of failing the request.
func TestBrowse_UnsupportedReachFriendly(t *testing.T) {
	f := newActionFixture(t)
	f.act.browseErr = engine.ErrBrowseUnsupported
	c, _ := loginAs(t, f, "bob")

	rec := getBrowse(t, f, c, "/api/nodes/mister/browse?path=")
	if rec.Code != http.StatusOK {
		t.Fatalf("unsupported browse status = %d, want 200 + message", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "reach setting RetroSync cannot browse") {
		t.Errorf("expected a friendly unsupported-reach message, got: %s", rec.Body.String())
	}
}

// TestBrowse_MissingNode404: a missing node returns 404.
func TestBrowse_MissingNode404(t *testing.T) {
	f := newActionFixture(t)
	f.act.browseErr = store.ErrNotFound
	c, _ := loginAs(t, f, "bob")

	rec := getBrowse(t, f, c, "/api/nodes/ghost/browse?path=")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing-node browse status = %d, want 404", rec.Code)
	}
}

// TestBrowse_NonAdminForbidden: a regular (non-admin) user is rejected with 403
// and never reaches the engine (the picker exposes a node's directory listing).
func TestBrowse_NonAdminForbidden(t *testing.T) {
	f := newActionFixture(t)
	f.act.browseEntries = []engine.DirEntry{{Name: "alpha.srm"}}
	c, _ := loginAs(t, f, "carol") // regular user

	rec := getBrowse(t, f, c, "/api/nodes/bob-deck/browse?path=")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin browse status = %d, want 403", rec.Code)
	}
	if calls := f.act.browseCalls(); len(calls) != 0 {
		t.Fatalf("non-admin request reached the engine: %+v", calls)
	}
}

// TestBrowse_Unauthenticated401: no session yields a 401 on the /api/ route.
func TestBrowse_Unauthenticated401(t *testing.T) {
	f := newActionFixture(t)
	rec := getBrowse(t, f, nil, "/api/nodes/bob-deck/browse?path=")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated browse status = %d, want 401", rec.Code)
	}
}

// TestBrowse_NoContentsLeak: the picker fragment renders names/sizes/mtimes but
// never any file CONTENTS (DirEntry carries no content; the handler cannot leak
// what it never receives). We assert a seeded file's bytes never appear.
func TestBrowse_NoContentsLeak(t *testing.T) {
	f := newActionFixture(t)
	f.act.browseEntries = []engine.DirEntry{
		{Name: "secret.srm", IsDir: false, Size: 5, Mtime: time.Unix(0, 0)},
	}
	c, _ := loginAs(t, f, "bob")
	rec := getBrowse(t, f, c, "/api/nodes/bob-deck/browse?path=")
	body := rec.Body.String()
	if !strings.Contains(body, "secret.srm") {
		t.Fatalf("name not rendered: %s", body)
	}
	// The size is shown but the actual bytes are not (there is no content field to
	// render). Sanity: a marker that would only exist if contents were embedded.
	if strings.Contains(body, "CONTENT-BYTES") {
		t.Error("file contents leaked into the picker fragment")
	}
}
