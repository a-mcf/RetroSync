package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/store"
)

// templateFuncs are the html/template helpers the templates reference. Kept
// minimal: nodeRowCtx bundles one node row with the page-level enum/owner option
// sets + CSRF so the "node-row" template can render an edit form with correct
// pre-selected <option>s without each row re-deriving them.
var templateFuncs = template.FuncMap{
	"nodeRowCtx":  nodeRowCtx,
	"groupRowCtx": groupRowCtx,
	"userRowCtx":  userRowCtx,
}

// nodeRowContext is the per-row template context: one node plus the shared
// page-level option sets and CSRF token.
type nodeRowContext struct {
	Node    nodeAdminRow
	Owners  []string
	Kinds   []string
	Reaches []string
	CSRF    string
}

// nodeRowCtx builds a nodeRowContext from the page data and a single row. It is
// registered as a template func so "nodes-list" can pass each row everything its
// edit form needs.
func nodeRowCtx(page nodesPageData, row nodeAdminRow) nodeRowContext {
	return nodeRowContext{
		Node:    row,
		Owners:  page.Owners,
		Kinds:   page.Kinds,
		Reaches: page.Reaches,
		CSRF:    page.CSRF,
	}
}

// The /nodes registry UI (slice-10): admin-gated node CRUD plus a per-node
// reachability smoke-test. Every handler in this file is mounted behind
// requireAuth + requireAdmin (and the mutations behind requireCSRF) in
// Server.Handler, so a non-admin never reaches the Store here.
//
// Secrets discipline (docs/auth.md): the reach_config form
// collects a secret_ref — a NAME/key into the host's secret store — never a
// cleartext password or key. No handler here reads, stores, displays, or logs an
// actual secret. reach_config holds only non-secret connection info + secret_ref.
//
// Reach boundary: smoke-test goes through the Actioner.SmokeTest engine method,
// NOT a direct internal/reach call, so this package imports NO internal/reach,
// pgx, or store-postgres. The unsupported-reach case is matched on the
// engine's own engine.ErrSmokeTestUnsupported sentinel (engine is core logic,
// not infrastructure — the same dependency the conflict handlers already have).

// --- view-models ---------------------------------------------------------

// nodesPageData drives GET /nodes (the full page) and the node-list fragment.
type nodesPageData struct {
	User  userView
	Nodes []nodeAdminRow
	// Owners is the set of user ids the owner_user_id <select> offers (plus a
	// "(shared / none)" empty option), so the admin picks an existing owner.
	Owners []string
	// Kinds / Reaches are the enum option sets for the create/edit form selects.
	Kinds   []string
	Reaches []string
	CSRF    string
}

// nodeAdminRow is one node in the admin list, including its reach_config so the
// edit form can be pre-filled. reach_config carries only non-secret fields +
// secret_ref (never a credential).
type nodeAdminRow struct {
	ID          string
	OwnerUserID string // "" for a shared node
	Display     string
	Kind        string
	Reach       string
	// reach_config fields for pre-filling the edit form (non-secret only).
	Path string
}

var nodeKindOptions = []string{
	string(store.KindDeck), string(store.KindMister),
	string(store.KindAnbernic), string(store.KindGeneric),
}

var nodeReachOptions = []string{
	string(store.ReachSyncthingShare),
}

// --- GET /nodes ----------------------------------------------------------

// handleNodesPage renders the admin node registry: a list of every node plus an
// add-node form and per-node edit/delete/test controls. There is no persistent
// reachability badge — the on-demand Test button renders a transient result.
// Admin-gating is enforced by the requireAdmin wrapper.
func (s *Server) handleNodesPage(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	data, err := s.buildNodesPage(r.Context(), u)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "nodes page build failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data.CSRF = s.csrfFor(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "nodes", data); err != nil {
		s.logger.ErrorContext(r.Context(), "nodes page render failed", "err", err.Error())
	}
}

func (s *Server) buildNodesPage(ctx context.Context, u store.User) (nodesPageData, error) {
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return nodesPageData{}, err
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nodesPageData{}, err
	}

	data := nodesPageData{
		User:    userView{ID: u.ID, Display: u.Display, Role: u.Role},
		Kinds:   nodeKindOptions,
		Reaches: nodeReachOptions,
	}
	for _, usr := range users {
		data.Owners = append(data.Owners, usr.ID)
	}
	sort.Strings(data.Owners)
	for _, n := range nodes {
		row := nodeAdminRow{
			ID:      n.ID,
			Display: n.Display,
			Kind:    string(n.Kind),
			Reach:   string(n.Reach),
			// reach_config: only non-secret fields. There is no secret to copy.
			Path: n.ReachConfig.Path,
		}
		if n.OwnerUserID != nil {
			row.OwnerUserID = *n.OwnerUserID
		}
		data.Nodes = append(data.Nodes, row)
	}
	sort.Slice(data.Nodes, func(i, j int) bool { return data.Nodes[i].ID < data.Nodes[j].ID })
	return data, nil
}

// --- POST /api/nodes (create) --------------------------------------------

// handleCreateNode handles POST /api/nodes. Form fields: id, owner_user_id
// (optional), display, kind, reach, plus the reach_config path. On success it
// returns the
// refreshed node-list fragment. Error mapping:
//   - duplicate id (ErrConflict)         -> 409
//   - bad kind/reach (caught locally in parseNodeForm) -> 400
//   - missing owner FK (ErrInvalidReference) -> 422
//   - bad reach_config shape (local)     -> 400
//
// Note: bad kind/reach is rejected by parseNodeForm before the Store is touched,
// so it surfaces as a 400 here. The ErrInvalidValue -> 422 branch in
// handleNodeWriteErr is a defense-in-depth backstop that form validation already
// prevents from firing.
func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	n, verr := s.parseNodeForm(r, "")
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}
	err := s.store.CreateNode(r.Context(), n)
	if s.handleNodeWriteErr(w, r, err) {
		return
	}
	s.refreshNodesList(w, r, u)
}

// --- POST /api/nodes/{id} (edit) -----------------------------------------

// handleEditNode handles POST /api/nodes/{id}. It rewrites the node's mutable
// fields. The path id is authoritative (the form's id field, if any, is
// ignored). Same error mapping as create, plus ErrNotFound -> 404.
func (s *Server) handleEditNode(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	n, verr := s.parseNodeForm(r, id)
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}
	err := s.store.UpdateNode(r.Context(), n)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such node", http.StatusNotFound)
		return
	}
	if s.handleNodeWriteErr(w, r, err) {
		return
	}
	s.refreshNodesList(w, r, u)
}

// --- POST /api/nodes/{id}/delete -----------------------------------------

// handleDeleteNode handles POST /api/nodes/{id}/delete. Under auto-mirror a
// node delete simply cascades: its sync_members and manifest rows go with it,
// and a removed member's file just stops mirroring (there is no active-binding
// FK to block it anymore). The ErrInvalidReference/ErrConflict branch is kept as
// a defensive friendly-409 in case a future FK ever blocks a node delete.
func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	err := s.store.DeleteNode(r.Context(), id)
	switch {
	case err == nil:
		s.refreshNodesList(w, r, u)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such node", http.StatusNotFound)
	case errors.Is(err, store.ErrInvalidReference) || errors.Is(err, store.ErrConflict):
		// Under auto-mirror a node delete cascades its sync memberships and manifest
		// rows, so nothing normally blocks it. This branch is a defensive,
		// friendly-409 in case a future FK ever refuses the delete (e.g. a node tied
		// to a conflicted sync) — non-500 so the admin sees a clear message.
		http.Error(w, "node can't be deleted right now — resolve any conflicted syncs that use it first", http.StatusConflict)
	default:
		s.logger.ErrorContext(r.Context(), "delete node failed", "node", id, "err", err.Error())
		http.Error(w, "could not delete node", http.StatusInternalServerError)
	}
}

// --- POST /api/nodes/{id}/smoke-test -------------------------------------

// handleSmokeTest handles POST /api/nodes/{id}/smoke-test: probe reachability via
// the engine (Actioner.SmokeTest), so the web layer never touches internal/reach
// or a driver. The result is purely transient — nothing is persisted (there is no
// last_seen / reachable storage anymore). Returns an HTML result fragment:
//   - nil           -> "reachable".
//   - ErrSmokeTestUnsupported -> a friendly, actionable message (a stored reach
//     value nothing can serve is a data fault the admin fixes by editing it).
//   - any other error -> the error, surfaced to the admin.
func (s *Server) handleSmokeTest(w http.ResponseWriter, r *http.Request) {
	if s.actioner == nil {
		s.logger.ErrorContext(r.Context(), "smoke-test: no actioner wired")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	id := r.PathValue("id")

	result := smokeResult{NodeID: id}
	err := s.actioner.SmokeTest(r.Context(), id)
	switch {
	case err == nil:
		result.OK = true
		result.Message = "reachable"
	case errors.Is(err, store.ErrNotFound):
		// engine.SmokeTest resolves the node and returns ErrNotFound for a typo'd
		// id, so we surface a clean 404 without a redundant pre-check GetNode.
		http.Error(w, "no such node", http.StatusNotFound)
		return
	case errors.Is(err, engine.ErrSmokeTestUnsupported):
		result.Message = "this device has a reach setting RetroSync cannot use — edit the device to change it"
	default:
		// Surface the engine error to the admin (the share path is missing, etc).
		// reach_config holds no secret, so nothing sensitive can leak here.
		result.Message = "not reachable: " + err.Error()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if rerr := s.templates.ExecuteTemplate(w, "smoke-result", result); rerr != nil {
		s.logger.ErrorContext(r.Context(), "smoke-test render failed", "err", rerr.Error())
	}
}

// smokeResult drives the smoke-test result fragment.
type smokeResult struct {
	NodeID  string
	OK      bool
	Message string
}

// --- GET /api/nodes/{id}/browse (save-file picker) -----------------------

// browsePicker drives the "node-browse" fragment, which serves BOTH pickers:
//   - the sync member save-FILE picker (slice-17): file rows are selectable, the
//     hx-get base is /api/nodes/{id}/browse.
//   - the node-registry FOLDER picker (slice-29): file rows are inert (shown so the
//     admin can confirm the folder holds the right saves, but not clickable), a
//     "Use this folder" button selects the current directory by its ABSOLUTE path,
//     and the hx-get base is /api/server/browse.
//
// The Mode-ish fields (FolderSelect, BrowseBase, CrumbLabel, AbsDir) parameterize
// the one fragment so neither picker duplicates it. It carries the directory being
// browsed (so folder/file links build correct relative paths), an optional up-link,
// the entries, and — when browsing is impossible — a friendly message.
type browsePicker struct {
	NodeID string
	// Dir is the relative path of the directory being shown ("" = the picker root:
	// the node save root, or the server share root). Folder and file links join
	// their Name onto Dir.
	Dir string
	// BrowseBase is the hx-get URL prefix the folder/up links target, WITHOUT the
	// ?path= query — /api/nodes/{id}/browse for the file picker, /api/server/browse
	// for the folder picker.
	BrowseBase string
	// CrumbLabel is the breadcrumb prefix shown before the current dir (the node id
	// for the file picker; the share root for the folder picker).
	CrumbLabel string
	// FolderSelect turns on the node-registry folder picker's behavior: a "Use this
	// folder" button (targeting AbsDir) and inert file rows. False renders the
	// slice-17 save-file picker (selectable file rows, no folder button).
	FolderSelect bool
	// AbsDir is the ABSOLUTE server-side path of the current dir (share root joined
	// with Dir), the value the "Use this folder" button fills into the node's path
	// input. Only meaningful when FolderSelect is true (the client can't know the
	// share root, so the server renders it).
	AbsDir string
	// Parent is the relative path of Dir's parent, shown as a ".." up-link.
	// HasParent is false at the root (no up-link there).
	Parent    string
	HasParent bool
	// EncodedParent is Parent url.QueryEscape'd for embedding in the up-link's
	// hx-get ?path= query. html/template only URL-escapes href/src attributes,
	// NOT hx-get, so a directory named "Mario & Luigi" would otherwise misparse
	// as a second query parameter.
	EncodedParent string
	// Entries are the directory's contents (dirs first, then files), metadata only.
	Entries []browseEntry
	// Message, when non-empty, replaces the listing with a friendly note (e.g.
	// "browsing not supported for this node", or a missing/not-a-directory path).
	Message string
}

// browseEntry is one row in the picker: a folder (click to re-browse) or a file
// (click to select its relative path). Rel is the full node-relative path of the
// entry (Dir joined with Name), which is exactly the member path a file click
// fills in. It carries metadata only — no contents.
type browseEntry struct {
	Name string
	Rel  string
	// EncodedRel is Rel url.QueryEscape'd for the folder row's hx-get ?path=
	// query (hx-get is not an href, so html/template does no URL escaping of
	// its own — see browsePicker.EncodedParent).
	EncodedRel string
	IsDir      bool
	Size       int64
	Mtime      time.Time
}

// handleBrowseNode implements GET /api/nodes/{id}/browse?path=<rel>. It returns
// an HTML fragment (htmx) listing the node's directory so the operator can click
// a real save file instead of typing its path. Admin-only (mounted behind
// requireAdmin) because it exposes a node's directory listing. Read-only.
//
// It is path-safe and contents-safe: the relative path goes straight to
// engine.BrowseNode, where the adapter's safepath check rejects any
// traversal/absolute path (mapped to 400 here via engine.ErrBrowseUnsafePath),
// and the listing carries metadata ONLY — names, sizes, mtimes — never file
// bytes. Error mapping:
//   - unsafe/traversal path (ErrBrowseUnsafePath)   -> 400
//   - missing node (store.ErrNotFound)              -> 404
//   - unsupported reach (ErrBrowseUnsupported) -> 200 + friendly message
//   - missing dir / not-a-directory (other err)     -> 200 + friendly message
func (s *Server) handleBrowseNode(w http.ResponseWriter, r *http.Request) {
	if s.actioner == nil {
		http.Error(w, "browsing unavailable", http.StatusInternalServerError)
		return
	}
	id := r.PathValue("id")
	// The requested directory is a node-relative path; "" means the node root.
	// We Clean it for display/link math but pass the RAW query value to the engine
	// so the adapter's safepath gets the unmodified hostile input to reject.
	raw := r.URL.Query().Get("path")

	entries, err := s.actioner.BrowseNode(r.Context(), id, raw)
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrBrowseUnsafePath):
			http.Error(w, "that path is not allowed", http.StatusBadRequest)
			return
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "no such node", http.StatusNotFound)
			return
		case errors.Is(err, engine.ErrBrowseUnsupported):
			pick := nodeBrowsePicker(id, cleanBrowseDir(raw))
			pick.Message = "this device has a reach setting RetroSync cannot browse"
			s.renderBrowse(w, r, pick)
			return
		default:
			// A missing directory or a not-a-directory path: surface a friendly note
			// in-place rather than a 500. reach_config holds no secret, so the engine
			// error text leaks nothing sensitive, but we keep the message generic.
			pick := nodeBrowsePicker(id, cleanBrowseDir(raw))
			pick.Message = "could not browse that folder"
			s.renderBrowse(w, r, pick)
			return
		}
	}

	pick := nodeBrowsePicker(id, cleanBrowseDir(raw))
	fillBrowseListing(&pick, entries)
	s.renderBrowse(w, r, pick)
}

// --- GET /api/server/browse (server folder picker) -----------------------

// handleBrowseServer implements GET /api/server/browse?path=<rel>. It returns the
// same "node-browse" HTML fragment as handleBrowseNode, but in FOLDER-select mode:
// it lists the SERVER's share-root directory (the syncthing NFS mount) so the admin
// can point-and-click the absolute mount path when registering a syncthing-share
// node, instead of hand-typing it. Admin-only (mounted behind requireAdmin) and
// read-only (a safe GET, no CSRF), exactly like handleBrowseNode.
//
// It mirrors handleBrowseNode's raw-query-to-engine / cleanBrowseDir discipline and
// error mapping: the RAW ?path= goes straight to engine.BrowseServer (so the
// adapter's safepath rejects hostile input), while cleanBrowseDir normalizes the
// value only for display/link math. Error mapping:
//   - unsafe/traversal path (ErrBrowseUnsafePath)        -> 400
//   - no share root configured (ErrBrowseUnsupported)    -> 200 + friendly message
//   - missing dir / not-a-directory (other err)          -> 200 + friendly message
func (s *Server) handleBrowseServer(w http.ResponseWriter, r *http.Request) {
	if s.actioner == nil {
		http.Error(w, "browsing unavailable", http.StatusInternalServerError)
		return
	}
	// "" means the share root; pass the RAW value to the engine so safepath sees the
	// unmodified hostile input.
	raw := r.URL.Query().Get("path")

	entries, err := s.actioner.BrowseServer(r.Context(), raw)
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrBrowseUnsafePath):
			http.Error(w, "that path is not allowed", http.StatusBadRequest)
			return
		case errors.Is(err, engine.ErrBrowseUnsupported):
			pick := s.serverBrowsePicker(cleanBrowseDir(raw))
			pick.Message = "server folder browsing is not configured"
			s.renderBrowse(w, r, pick)
			return
		default:
			// A missing/not-a-directory share path: surface a friendly note in-place
			// rather than a 500. The message is generic (no path echoed), so log the
			// real error server-side — it is how an operator distinguishes an
			// unmounted root from a misconfigured one or a permission problem.
			s.logger.WarnContext(r.Context(), "server browse failed", "err", err.Error())
			pick := s.serverBrowsePicker(cleanBrowseDir(raw))
			pick.Message = "could not browse that folder"
			s.renderBrowse(w, r, pick)
			return
		}
	}

	pick := s.serverBrowsePicker(cleanBrowseDir(raw))
	fillBrowseListing(&pick, entries)
	s.renderBrowse(w, r, pick)
}

// nodeBrowsePicker seeds a browsePicker for the sync member save-FILE picker: file
// rows selectable, hx-get base /api/nodes/{id}/browse, breadcrumb the node id.
func nodeBrowsePicker(id, dir string) browsePicker {
	return browsePicker{
		NodeID:     id,
		Dir:        dir,
		BrowseBase: "/api/nodes/" + id + "/browse",
		CrumbLabel: id,
	}
}

// serverBrowsePicker seeds a browsePicker for the node-registry FOLDER picker: a
// "Use this folder" button (with the absolute path), inert file rows, hx-get base
// /api/server/browse, breadcrumb the share root. It computes AbsDir by joining the
// server's known share root with dir — the client can't know the root, so the
// server renders the absolute path the admin copies into reach_config.path. dir is
// already cleanBrowseDir'd (no leading slash, no traversal), so a plain path.Join
// is safe and yields a forward-slash Unix path (the server runs in a Linux
// container). Without a configured root (only possible when Options.ShareRoot was
// left unset — the engine then answers ErrBrowseUnsupported anyway) the button is
// suppressed rather than fabricating an absolute-looking path.
func (s *Server) serverBrowsePicker(dir string) browsePicker {
	pick := browsePicker{
		Dir:        dir,
		BrowseBase: "/api/server/browse",
		CrumbLabel: s.shareRoot,
	}
	if s.shareRoot == "" {
		pick.CrumbLabel = "server share"
		return pick
	}
	pick.FolderSelect = true
	pick.AbsDir = path.Join(s.shareRoot, dir)
	return pick
}

// fillBrowseListing populates a seeded browsePicker's up-link and entry rows from
// the engine's directory listing, shared by both pickers. It joins each entry's
// name onto pick.Dir for the node-relative Rel path and URL-encodes the folder /
// up-link ?path= values (hx-get is not an href, so html/template applies no URL
// escaping of its own).
func fillBrowseListing(pick *browsePicker, entries []engine.DirEntry) {
	dir := pick.Dir
	if dir != "" {
		pick.HasParent = true
		// path.Dir of a single segment is "."; normalize back to "" (the root).
		parent := path.Dir(dir)
		if parent == "." {
			parent = ""
		}
		pick.Parent = parent
		pick.EncodedParent = url.QueryEscape(parent)
	}
	for _, e := range entries {
		rel := e.Name
		if dir != "" {
			rel = dir + "/" + e.Name
		}
		pick.Entries = append(pick.Entries, browseEntry{
			Name:       e.Name,
			Rel:        rel,
			EncodedRel: url.QueryEscape(rel),
			IsDir:      e.IsDir,
			Size:       e.Size,
			Mtime:      e.Mtime,
		})
	}
}

// cleanBrowseDir normalizes a raw query path into a node-relative directory for
// display/link math: "" and "." both become "" (the node root); otherwise it is
// path.Clean'd. This is purely cosmetic — the RAW value is what the engine's
// safepath validates — but it keeps the rendered up-link/child links tidy. A
// value that path.Clean would let escape (".." itself or a "../" prefix) is
// flattened to "" so a rejected request never renders an escaping link; in
// practice the engine has already 400'd such a path before render. The check
// matches what safepath actually rejects: a legitimate directory NAME that
// merely starts with dots ("..data" — k8s volume mounts create these) is kept,
// so the folder picker's "Use this folder" value stays correct for it.
func cleanBrowseDir(raw string) string {
	if raw == "" {
		return ""
	}
	c := path.Clean(raw)
	if c == "." || c == "/" {
		return ""
	}
	if c == ".." || strings.HasPrefix(c, "../") || strings.HasPrefix(c, "/") {
		return ""
	}
	return c
}

// renderBrowse writes the node-browse fragment.
func (s *Server) renderBrowse(w http.ResponseWriter, r *http.Request, pick browsePicker) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "node-browse", pick); err != nil {
		s.logger.ErrorContext(r.Context(), "browse render failed", "err", err.Error())
	}
}

// --- shared form parsing / error mapping ---------------------------------

// parseNodeForm reads and validates the node create/edit form. idOverride, when
// non-empty (edit), is the authoritative node id (the path id); for create it is
// "" and the id comes from the form. It returns the assembled Node and a non-empty
// human message string on a validation failure (the caller 400s with it).
//
// reach_config shape is validated here per reach: syncthing-share requires a
// non-empty path. No password or secret field is ever read — reach_config
// carries only non-secret reach info (docs/auth.md).
func (s *Server) parseNodeForm(r *http.Request, idOverride string) (store.Node, string) {
	if err := r.ParseForm(); err != nil {
		return store.Node{}, "bad request"
	}
	id := idOverride
	if id == "" {
		id = strings.TrimSpace(r.PostFormValue("id"))
	}
	if id == "" {
		return store.Node{}, "id is required"
	}
	// A node id is a registry slug, same shape as a game id (see slug.go). This
	// retrofit (slice-11 carry-forward) rejects an uppercase/spaced/empty-after-
	// trim id before it reaches the Store.
	if !validSlug(id) {
		return store.Node{}, "id must be a slug: lowercase letters, digits, and hyphens (e.g. bob-deck)"
	}
	display := strings.TrimSpace(r.PostFormValue("display"))
	if display == "" {
		return store.Node{}, "display is required"
	}
	kind := store.Kind(strings.TrimSpace(r.PostFormValue("kind")))
	if !store.ValidKind(kind) {
		return store.Node{}, "kind must be one of deck, mister, anbernic, generic"
	}
	rch := store.Reach(strings.TrimSpace(r.PostFormValue("reach")))
	if !store.ValidReach(rch) {
		return store.Node{}, "reach must be syncthing-share"
	}

	n := store.Node{ID: id, Display: display, Kind: kind, Reach: rch}

	owner := strings.TrimSpace(r.PostFormValue("owner_user_id"))
	if owner != "" {
		n.OwnerUserID = &owner
	}

	// reach_config shape per reach. ValidReach above has already rejected
	// anything but syncthing-share, so this is the only shape to assemble.
	path := strings.TrimSpace(r.PostFormValue("path"))
	if path == "" {
		return store.Node{}, "syncthing-share requires a non-empty path"
	}
	n.ReachConfig = store.ReachConfig{Path: path}
	return n, ""
}

// handleNodeWriteErr maps a CreateNode/UpdateNode error to an HTTP response and
// reports whether it handled one (true) so the caller can stop. It does NOT
// handle ErrNotFound (edit-specific) — the caller maps that first.
func (s *Server) handleNodeWriteErr(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "a node with that id already exists", http.StatusConflict)
	case errors.Is(err, store.ErrInvalidReference):
		http.Error(w, "owner_user_id does not name an existing user", http.StatusUnprocessableEntity)
	case errors.Is(err, store.ErrInvalidValue):
		http.Error(w, "invalid kind or reach value", http.StatusUnprocessableEntity)
	default:
		s.logger.ErrorContext(r.Context(), "node write failed", "err", err.Error())
		http.Error(w, "could not save node", http.StatusInternalServerError)
	}
	return true
}

// refreshNodesList re-renders the node-list fragment (the #nodes-list region)
// after a successful mutation, so HTMX swaps the updated table in place.
func (s *Server) refreshNodesList(w http.ResponseWriter, r *http.Request, u store.User) {
	data, err := s.buildNodesPage(r.Context(), u)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "refresh nodes build failed", "err", err.Error())
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}
	data.CSRF = s.csrfFor(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "nodes-list", data); err != nil {
		s.logger.ErrorContext(r.Context(), "refresh nodes render failed", "err", err.Error())
	}
}
