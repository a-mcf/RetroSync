package web

import (
	"errors"
	"html"
	"strings"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/engine"
)

// The server folder picker (slice-29): GET /api/server/browse lists the SERVER's
// share root so an admin can point-and-click a node's mount path. These mirror
// browse_test.go but assert the folder-picker specifics: the absolute "Use this
// folder" target, inert (non-selectable) file rows, and the /api/server/browse
// hx-get base.

// TestBrowseServer_AdminListsEntries: an admin GET returns the folder-picker
// fragment with the seeded entries, a "Use this folder" button carrying the
// ABSOLUTE path (share root joined with the browsed dir), and file rows that are
// inert (no data-rel — not selectable in folder mode).
func TestBrowseServer_AdminListsEntries(t *testing.T) {
	f := newActionFixture(t) // fixture ShareRoot is /shares
	mt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	f.act.serverBrowseEntries = []engine.DirEntry{
		{Name: "bob-deck-saves", IsDir: true},
		{Name: "readme.txt", IsDir: false, Size: 42, Mtime: mt},
	}
	c, _ := loginAs(t, f, "bob") // admin

	rec := getBrowse(t, f, c, "/api/server/browse?path=")
	if rec.Code != 200 {
		t.Fatalf("server browse status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"bob-deck-saves",
		"readme.txt",
		"Use this folder",
		`data-abs="/shares"`,        // root's absolute path
		"/api/server/browse?path=",  // folder/up links target the server route
		`class="browse-file-inert"`, // files are shown but inert
	} {
		if !strings.Contains(body, want) {
			t.Errorf("server browse body missing %q\n%s", want, body)
		}
	}
	// File rows are NOT selectable in folder mode: no data-rel / .browse-file.
	if strings.Contains(body, "data-rel=") {
		t.Errorf("folder-mode file row should be inert (no data-rel)\n%s", body)
	}
	// The handler reached the engine with the (empty) root path.
	if calls := f.act.serverBrowseCalls(); len(calls) != 1 || calls[0] != "" {
		t.Fatalf("server browse calls = %+v, want one \"\"", calls)
	}
}

// TestBrowseServer_SubdirAbsolutePathAndUpLink: browsing a subdir passes the rel
// path to the engine, renders the ABSOLUTE "Use this folder" target for that
// subdir, and renders an up-link to the parent.
func TestBrowseServer_SubdirAbsolutePathAndUpLink(t *testing.T) {
	f := newActionFixture(t)
	f.act.serverBrowseEntries = []engine.DirEntry{
		{Name: "sm.srm", IsDir: false, Size: 10},
	}
	c, _ := loginAs(t, f, "bob")

	rec := getBrowse(t, f, c, "/api/server/browse?path=bob-deck-saves")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-abs="/shares/bob-deck-saves"`) {
		t.Errorf("absolute Use-this-folder target not share-root-joined:\n%s", body)
	}
	if !strings.Contains(body, "/api/server/browse?path=") || !strings.Contains(body, "up") {
		t.Errorf("expected an up-link at a subdir:\n%s", body)
	}
	if calls := f.act.serverBrowseCalls(); len(calls) != 1 || calls[0] != "bob-deck-saves" {
		t.Fatalf("server browse calls = %+v, want relPath bob-deck-saves", calls)
	}
}

// TestBrowseServer_SpecialCharAbsolutePath: a browsed dir with URL/HTML
// metacharacters is rendered into data-abs so the browser decodes it back to the
// real absolute path when JS reads getAttribute.
func TestBrowseServer_SpecialCharAbsolutePath(t *testing.T) {
	f := newActionFixture(t)
	f.act.serverBrowseEntries = []engine.DirEntry{{Name: "x.srm", Size: 1}}
	c, _ := loginAs(t, f, "bob")

	rec := getBrowse(t, f, c, "/api/server/browse?path=Mario+%26+Luigi")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	// html/template HTML-escapes "&" in the attribute; the browser decodes it.
	if !strings.Contains(html.UnescapeString(rec.Body.String()), `data-abs="/shares/Mario & Luigi"`) {
		t.Fatalf("special-char absolute path not rendered intact:\n%s", rec.Body.String())
	}
}

// TestBrowseServer_TraversalRejected400: an unsafe path (ErrBrowseUnsafePath) maps
// to 400, and the hostile path is forwarded verbatim for safepath to reject.
func TestBrowseServer_TraversalRejected400(t *testing.T) {
	f := newActionFixture(t)
	f.act.serverBrowseErr = engine.ErrBrowseUnsafePath
	c, _ := loginAs(t, f, "bob")

	rec := getBrowse(t, f, c, "/api/server/browse?path=../../etc")
	if rec.Code != 400 {
		t.Fatalf("traversal status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	if calls := f.act.serverBrowseCalls(); len(calls) != 1 || calls[0] != "../../etc" {
		t.Fatalf("server browse calls = %+v, want the raw hostile path forwarded", calls)
	}
}

// TestBrowseServer_UnsupportedFriendly: an engine with no share root
// (ErrBrowseUnsupported) returns 200 with a friendly "not configured" message.
func TestBrowseServer_UnsupportedFriendly(t *testing.T) {
	f := newActionFixture(t)
	f.act.serverBrowseErr = engine.ErrBrowseUnsupported
	c, _ := loginAs(t, f, "bob")

	rec := getBrowse(t, f, c, "/api/server/browse?path=")
	if rec.Code != 200 {
		t.Fatalf("unsupported status = %d, want 200 + message", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Errorf("expected friendly 'not configured' message, got: %s", rec.Body.String())
	}
}

// TestBrowseServer_MissingDirFriendly: a missing/not-a-directory share path
// (generic engine error) returns 200 with a friendly in-place note, not a 500.
func TestBrowseServer_MissingDirFriendly(t *testing.T) {
	f := newActionFixture(t)
	f.act.serverBrowseErr = errors.New("localfs: list: no such file or directory")
	c, _ := loginAs(t, f, "bob")

	rec := getBrowse(t, f, c, "/api/server/browse?path=")
	if rec.Code != 200 {
		t.Fatalf("missing-dir status = %d, want 200 + message", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "could not browse") {
		t.Errorf("expected friendly 'could not browse' message, got: %s", rec.Body.String())
	}
}

// TestBrowseServer_NonAdminForbidden: a regular user is 403'd and never reaches
// the engine (the picker exposes the server's directory listing).
func TestBrowseServer_NonAdminForbidden(t *testing.T) {
	f := newActionFixture(t)
	f.act.serverBrowseEntries = []engine.DirEntry{{Name: "x", IsDir: true}}
	c, _ := loginAs(t, f, "carol") // regular user

	rec := getBrowse(t, f, c, "/api/server/browse?path=")
	if rec.Code != 403 {
		t.Fatalf("non-admin status = %d, want 403", rec.Code)
	}
	if calls := f.act.serverBrowseCalls(); len(calls) != 0 {
		t.Fatalf("non-admin request reached the engine: %+v", calls)
	}
}

// TestBrowseServer_Unauthenticated401: no session yields a 401 on the /api/ route.
func TestBrowseServer_Unauthenticated401(t *testing.T) {
	f := newActionFixture(t)
	rec := getBrowse(t, f, nil, "/api/server/browse?path=")
	if rec.Code != 401 {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
}
