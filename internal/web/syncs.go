package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/a-mcf/retrosync/internal/store"
)

// The /syncs registry UI (slice-21): admin-gated sync CRUD plus per-sync member
// (node + path) management. The *sync* is the atomic entity now — there is no
// games table. "Game" is just a free-text LABEL on the sync (a display grouping;
// a later slice infers it from save filenames). Every handler in this file is
// mounted behind requireAuth + requireAdmin (and the mutations behind
// requireCSRF) in Server.Handler, so a non-admin never reaches the Store here
// (mirrors the slice-10 /nodes registry).
//
// A sync is the unit of mirroring; its members (node + save-file path) are its
// scope. The registry edits syncs + sync_members directly.
//
// No secrets are involved in syncs or members (a path is not a credential), but
// we keep the same discipline: nothing sensitive is logged.
//
// Auto-mirror simplified the delete/remove guards. There is no "active session"
// to protect — every sync just mirrors. The one remaining guard (friendly 409):
// deleting a SYNC while it is CONFLICTED is refused, since that would silently
// discard an unresolved fork. Removing a MEMBER is always allowed — a removed
// member's file simply stops mirroring.
//
// Member paths can be filled by the save-file PICKER (slice-17): the "Add member"
// form's "Browse…" button hits GET /api/nodes/{id}/browse (handleBrowseNode) to
// list the selected node's mounted save dir; clicking a file fills the path
// input. The manual text field stays as a fallback.
// TODO(slice-discovery): infer the game label (and peer members) from save
// filenames so the operator does not type it by hand.
// TODO(slice-ssh): ssh nodes return "browsing not supported yet" until the
// ssh/sftp adapter lands; today only syncthing-share nodes are browsable.

// --- view-models ---------------------------------------------------------

// syncsPageData drives GET /syncs (the full page) and the syncs-list fragment.
type syncsPageData struct {
	User  userView
	Games []gameGroup
	// Nodes is the set of node ids the "add a member" <select> offers.
	Nodes []string
	// Q echoes the active search filter (over game label + sync name + id) back
	// into the search box.
	Q    string
	CSRF string
}

// gameGroup is a display grouping of syncs that share the same free-text game
// label. It is purely presentational — there is no game entity behind it.
type gameGroup struct {
	// Label is the shared game label ("" renders as "(no label)").
	Label string
	Syncs []syncAdminRow
}

// syncAdminRow is one sync, with its members and runtime state.
type syncAdminRow struct {
	ID      string
	Game    string
	Name    string
	Members []syncMemberRow
	// Conflict is true when the sync is paused on a fork (conflict_at set).
	Conflict bool
}

// syncMemberRow is one (node, path) member of a sync.
type syncMemberRow struct {
	NodeID string
	Path   string
}

// groupRowContext is the per-group template context: one game group plus the
// shared page-level node option set and CSRF token, so the "sync-group" template
// can render its forms without re-deriving them.
type groupRowContext struct {
	Group gameGroup
	Nodes []string
	CSRF  string
}

// groupRowCtx builds a groupRowContext from the page data and a single group. It
// is registered as a template func so "syncs-list" can pass each group what its
// forms need.
func groupRowCtx(page syncsPageData, group gameGroup) groupRowContext {
	return groupRowContext{Group: group, Nodes: page.Nodes, CSRF: page.CSRF}
}

// --- GET /syncs ----------------------------------------------------------

// handleSyncsPage renders the admin sync registry: a search box, the filtered
// sync list (grouped by game label, each sync with its members + runtime state),
// a "+ New sync" form, and per-sync controls. Admin-gating is enforced by the
// requireAdmin wrapper.
func (s *Server) handleSyncsPage(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	data, err := s.buildSyncsPage(r.Context(), u, q)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "syncs page build failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data.CSRF = s.csrfFor(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// An HTMX search request targets just the list region; a full navigation
	// renders the whole page.
	tmpl := "syncs"
	if r.Header.Get("HX-Request") == "true" {
		tmpl = "syncs-list"
	}
	if err := s.templates.ExecuteTemplate(w, tmpl, data); err != nil {
		s.logger.ErrorContext(r.Context(), "syncs page render failed", "err", err.Error())
	}
}

// buildSyncsPage assembles the registry view-model: every sync (optionally
// filtered by a case-insensitive substring over game label + sync name + id),
// grouped by game label for display.
func (s *Server) buildSyncsPage(ctx context.Context, u store.User, q string) (syncsPageData, error) {
	syncs, err := s.store.ListSyncs(ctx)
	if err != nil {
		return syncsPageData{}, err
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return syncsPageData{}, err
	}

	data := syncsPageData{
		User: userView{ID: u.ID, Display: u.Display, Role: u.Role},
		Q:    q,
	}
	for _, n := range nodes {
		data.Nodes = append(data.Nodes, n.ID)
	}
	sort.Strings(data.Nodes)

	needle := strings.ToLower(q)
	byLabel := map[string]*gameGroup{}
	var order []string
	for _, sy := range syncs {
		if needle != "" &&
			!strings.Contains(strings.ToLower(sy.Game), needle) &&
			!strings.Contains(strings.ToLower(sy.Name), needle) &&
			!strings.Contains(strings.ToLower(sy.ID), needle) {
			continue
		}
		row := syncAdminRow{ID: sy.ID, Game: sy.Game, Name: sy.Name, Conflict: sy.ConflictAt != nil}
		members, err := s.store.ListSyncMembers(ctx, sy.ID)
		if err != nil {
			return syncsPageData{}, err
		}
		for _, m := range members {
			row.Members = append(row.Members, syncMemberRow{NodeID: m.NodeID, Path: m.Path})
		}
		g, ok := byLabel[sy.Game]
		if !ok {
			g = &gameGroup{Label: sy.Game}
			byLabel[sy.Game] = g
			order = append(order, sy.Game)
		}
		g.Syncs = append(g.Syncs, row)
	}
	sort.Strings(order)
	for _, label := range order {
		g := byLabel[label]
		sort.Slice(g.Syncs, func(i, j int) bool { return g.Syncs[i].ID < g.Syncs[j].ID })
		data.Games = append(data.Games, *g)
	}
	return data, nil
}

// --- POST /api/syncs (create) --------------------------------------------

// handleCreateSync handles POST /api/syncs. Form fields: game (the free-text
// label, required), name (required), id (optional — auto-generated as a slug
// from game+name when omitted), plus optional REPEATED member rows: parallel
// node_id + path fields, one pair per save file to add up front (the slice-30
// one-shot form). A blank row (empty path) adds no member, so creating a sync
// with zero members still works (the current behavior). Error mapping:
//   - bad/empty fields, malformed member row, or invalid derived slug (local) -> 400
//   - duplicate id (ErrConflict)                                              -> 409
//   - a member naming a missing node (ErrInvalidReference)                    -> 422
//   - a member (node,path) already in ANOTHER sync (ErrConflict)             -> 409
//
// Create is one-shot but NOT transactional: on a member failure after the sync
// row exists, the partial sync is deliberately LEFT IN PLACE (matching the
// discover create-sync path) and the error message says so. A compensating
// DeleteSync here would be a data-loss hazard: this handler does not hold the
// engine's per-sync lock, so the background poll loop can mirror the partial
// sync and capture an overwritten save into save_versions during the create
// window — and DeleteSync cascades save_versions, permanently destroying that
// capture. A visible partial sync is recoverable in the registry; a destroyed
// capture is not. TODO(store-tx): a transactional create (see the twin TODO in
// discover.go) would close the window properly.
//
// It reuses the same Store machinery (CreateSync + SetSyncMember) the discover
// create-sync path uses; there is no games FK, so any non-empty label is
// accepted as-is.
func (s *Server) handleCreateSync(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	game := strings.TrimSpace(r.PostFormValue("game"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	if game == "" {
		http.Error(w, "a game label is required", http.StatusBadRequest)
		return
	}
	if name == "" {
		http.Error(w, "a sync name is required", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("id"))
	if id == "" {
		// Auto-id from game + name (e.g. "Super Metroid" + "Bob's stream" ->
		// super-metroid-bob-s-stream).
		id = slugify(game + "-" + name)
	}
	if !validSlug(id) {
		http.Error(w, "id must be a slug: lowercase letters, digits, and hyphens", http.StatusBadRequest)
		return
	}
	// Optional member rows: parallel node_id[]/path[] fields. Validate them
	// (lexical path gate + a path-without-device is malformed) BEFORE touching the
	// store, so a bad row 400s without creating anything.
	members, verr := parseMemberRows(r.PostForm["node_id"], r.PostForm["path"])
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}

	err := s.store.CreateSync(r.Context(), store.Sync{ID: id, Game: game, Name: name})
	switch {
	case err == nil:
		// proceed to add members
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "a sync with that id already exists", http.StatusConflict)
		return
	default:
		s.logger.ErrorContext(r.Context(), "create sync failed", "sync", id, "err", err.Error())
		http.Error(w, "could not create sync", http.StatusInternalServerError)
		return
	}

	// Add each member. On a failure the sync (and any members already added)
	// stays in place — see the handler comment for why deleting it here would
	// risk destroying an engine capture. The message tells the admin the sync
	// exists so they can finish or fix it in the registry list.
	for _, m := range members {
		merr := s.store.SetSyncMember(r.Context(), store.SyncMember{SyncID: id, NodeID: m.nodeID, Path: m.path})
		if merr == nil {
			continue
		}
		switch {
		case errors.Is(merr, store.ErrConflict):
			http.Error(w, "that save file is already in another sync — the sync was created without it; fix it in the list above", http.StatusConflict)
		case errors.Is(merr, store.ErrInvalidReference):
			http.Error(w, "no such device — the sync was created without it; fix it in the list above", http.StatusUnprocessableEntity)
		default:
			s.logger.ErrorContext(r.Context(), "create sync: add member failed", "sync", id, "node", m.nodeID, "err", merr.Error())
			http.Error(w, "could not add a device — the sync was created without it; fix it in the list above", http.StatusInternalServerError)
		}
		return
	}
	s.refreshSyncsList(w, r, u)
}

// parseMemberRows decodes the parallel node_id[]/path[] form fields the one-shot
// create form submits (one pair per member row, positionally matched). A row
// with a blank path adds no member — so the single default row left empty yields
// a memberless sync (creating with zero members stays valid). It returns a human
// message on a malformed row: a path with no chosen device, or a bad/traversal
// path (the same lexical gate as the member routes, since a persisted bad path
// would silently halt polling for the whole sync). Identical (node,path) pairs
// are de-duplicated so a double-filled row can't self-collide.
func parseMemberRows(nodeIDs, paths []string) ([]candidate, string) {
	n := len(paths)
	if len(nodeIDs) > n {
		n = len(nodeIDs)
	}
	at := func(s []string, i int) string {
		if i < len(s) {
			return strings.TrimSpace(s[i])
		}
		return ""
	}
	seen := make(map[candidate]bool)
	var out []candidate
	for i := 0; i < n; i++ {
		nodeID := at(nodeIDs, i)
		path := at(paths, i)
		if path == "" {
			continue // an unfilled row adds no member
		}
		if nodeID == "" {
			return nil, "choose a device for each save file"
		}
		if !validMemberPath(path) {
			return nil, memberPathError
		}
		c := candidate{nodeID: nodeID, path: path}
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, ""
}

// --- POST /api/syncs/{id} (edit: rename + relabel) -----------------------

// handleRenameSync handles POST /api/syncs/{id}. Body fields: name (required),
// game (the free-text label, required). The sync id is immutable (the path id is
// authoritative); only name + game label change. Empty name/game -> 400; missing
// sync -> 404.
func (s *Server) handleRenameSync(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		http.Error(w, "a sync name is required", http.StatusBadRequest)
		return
	}
	game := strings.TrimSpace(r.PostFormValue("game"))
	if game == "" {
		http.Error(w, "a game label is required", http.StatusBadRequest)
		return
	}
	// Read the existing sync to preserve its runtime state across the edit.
	sy, err := s.store.GetSync(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such sync", http.StatusNotFound)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "rename sync: get failed", "sync", id, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sy.Name = name
	sy.Game = game
	err = s.store.UpdateSync(r.Context(), sy)
	switch {
	case err == nil:
		s.refreshSyncsList(w, r, u)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such sync", http.StatusNotFound)
	default:
		s.logger.ErrorContext(r.Context(), "rename sync failed", "sync", id, "err", err.Error())
		http.Error(w, "could not rename sync", http.StatusInternalServerError)
	}
}

// --- POST /api/syncs/{id}/delete -----------------------------------------

// handleDeleteSync handles POST /api/syncs/{id}/delete. A CONFLICTED sync is
// refused with a friendly 409 (deleting it would silently discard an unresolved
// fork — resolve it first). Otherwise we delete; its members + runtime rows
// cascade. Missing sync -> 404.
func (s *Server) handleDeleteSync(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")

	// Guard: refuse deleting a conflicted sync (don't discard an unresolved fork).
	// NOTE: GetSync-then-DeleteSync is a benign TOCTOU — admin-only, not driven
	// concurrently, so the window cannot be raced in practice.
	if sy, err := s.store.GetSync(r.Context(), id); err == nil {
		if sy.ConflictAt != nil {
			http.Error(w, "this sync is in conflict — resolve it first", http.StatusConflict)
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.logger.ErrorContext(r.Context(), "delete sync: get sync failed", "sync", id, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	err := s.store.DeleteSync(r.Context(), id)
	switch {
	case err == nil:
		s.refreshSyncsList(w, r, u)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such sync", http.StatusNotFound)
	default:
		s.logger.ErrorContext(r.Context(), "delete sync failed", "sync", id, "err", err.Error())
		http.Error(w, "could not delete sync", http.StatusInternalServerError)
	}
}

// --- POST /api/syncs/{id}/members/{node_id} (set member) -----------------

// handleSetSyncMember handles POST /api/syncs/{id}/members/{node_id}. Body field:
// path. SetSyncMember upserts on (sync_id, node_id). Error mapping:
//   - empty or non-relative/traversal path (local)        -> 400
//   - missing sync or node (ErrInvalidReference)          -> 422
//   - (node, path) already a member of ANOTHER sync       -> 409
//     (the global UNIQUE (node_id, path) invariant)
//
// The path may be typed OR filled by the slice-17 save-file picker (the picker
// just sets this same field); either way it arrives as the `path` form value.
func (s *Server) handleSetSyncMember(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	syncID := r.PathValue("id")
	nodeID := r.PathValue("node_id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	path := strings.TrimSpace(r.PostFormValue("path"))
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	// Lexical fail-fast (mirrors safepath's rule; see validMemberPath): a bad
	// path wouldn't traverse — safepath rejects it at use — but persisting it
	// would silently halt polling for the whole sync.
	if !validMemberPath(path) {
		http.Error(w, memberPathError, http.StatusBadRequest)
		return
	}
	err := s.store.SetSyncMember(r.Context(), store.SyncMember{SyncID: syncID, NodeID: nodeID, Path: path})
	switch {
	case err == nil:
		s.refreshSyncsList(w, r, u)
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "that save file is already in another sync", http.StatusConflict)
	case errors.Is(err, store.ErrInvalidReference):
		http.Error(w, "no such sync or node", http.StatusUnprocessableEntity)
	default:
		s.logger.ErrorContext(r.Context(), "set sync member failed", "sync", syncID, "node", nodeID, "err", err.Error())
		http.Error(w, "could not save member", http.StatusInternalServerError)
	}
}

// --- POST /api/syncs/{id}/members/{node_id}/delete (remove member) -------

// handleDeleteSyncMember handles POST /api/syncs/{id}/members/{node_id}/delete.
// Under auto-mirror any member is removable — a removed member's file simply
// stops mirroring (there is no "active primary" to protect). A missing member is
// a 404.
func (s *Server) handleDeleteSyncMember(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	syncID := r.PathValue("id")
	nodeID := r.PathValue("node_id")

	err := s.store.DeleteSyncMember(r.Context(), syncID, nodeID)
	switch {
	case err == nil:
		s.refreshSyncsList(w, r, u)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such member", http.StatusNotFound)
	default:
		s.logger.ErrorContext(r.Context(), "delete sync member failed", "sync", syncID, "node", nodeID, "err", err.Error())
		http.Error(w, "could not remove member", http.StatusInternalServerError)
	}
}

// --- shared refresh ------------------------------------------------------

// refreshSyncsList re-renders the syncs-list fragment (the #syncs-list region)
// after a successful mutation so HTMX swaps the updated list in place. It
// preserves the active search filter: the mutation POST's own URL never
// carries ?q= (the forms post to /api/syncs/...), but HTMX sends the page URL
// in the HX-Current-URL header on every request, so q is parsed from there —
// falling back to the request URL's q for a non-HTMX submit.
func (s *Server) refreshSyncsList(w http.ResponseWriter, r *http.Request, u store.User) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if cur := r.Header.Get("HX-Current-URL"); cur != "" {
		if curURL, err := url.Parse(cur); err == nil {
			q = strings.TrimSpace(curURL.Query().Get("q"))
		}
	}
	data, err := s.buildSyncsPage(r.Context(), u, q)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "refresh syncs build failed", "err", err.Error())
		http.Redirect(w, r, "/syncs", http.StatusSeeOther)
		return
	}
	data.CSRF = s.csrfFor(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "syncs-list", data); err != nil {
		s.logger.ErrorContext(r.Context(), "refresh syncs render failed", "err", err.Error())
	}
}

// --- GET /syncs/{id}/member-editor (the member <dialog>) -----------------

// memberEditorData drives the member editor dialog.
type memberEditorData struct {
	SyncID   string
	Game     string
	SyncName string
	// NodeID is the member being edited. EMPTY means "add a device", and the
	// difference between the two modes is only whether the device is fixed or
	// chosen — the fields, the picker and the submit target are otherwise the
	// same, which is why one template serves both.
	NodeID string
	Path   string
	// Nodes offers the device choice in add mode: every registered node that is
	// not already a member of this sync (a repeat would silently upsert the
	// existing member's path rather than add anything).
	Nodes []string
	CSRF  string
}

// handleMemberEditor serves the member editor as a <dialog> fragment, swapped
// into the syncs page's #modal mount — the same shape as the conflict modal.
//
// Why a dialog rather than editing in place: setting a member's path offers the
// Browse picker, which is a scrolling file tree. That does not fit in a table
// row, and putting it there is what made the old member rows sprawl. Keeping the
// row read-only until an explicit Edit also means the page can show a sync's
// configuration without simultaneously offering to change it — the path is no
// longer a live <input> that can be nudged by accident.
//
// Read-only GET, admin-gated by the route wrapper like the rest of the registry.
// It writes nothing, so there is no CSRF token to check on the way in; the forms
// it renders carry one for their own POSTs.
func (s *Server) handleMemberEditor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sy, err := s.store.GetSync(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such sync", http.StatusNotFound)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "member editor: load sync failed", "sync", id, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	members, err := s.store.ListSyncMembers(r.Context(), id)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "member editor: list members failed", "sync", id, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := memberEditorData{
		SyncID:   id,
		Game:     sy.Game,
		SyncName: sy.Name,
		NodeID:   strings.TrimSpace(r.URL.Query().Get("node")),
		CSRF:     s.csrfFor(r),
	}

	if data.NodeID != "" {
		// Edit mode: the member must exist, so a stale row's Edit link 404s
		// rather than silently turning into an add.
		found := false
		for _, m := range members {
			if m.NodeID == data.NodeID {
				data.Path, found = m.Path, true
				break
			}
		}
		if !found {
			http.Error(w, "no such member", http.StatusNotFound)
			return
		}
	} else {
		// Add mode: offer only nodes that are not already members.
		taken := make(map[string]bool, len(members))
		for _, m := range members {
			taken[m.NodeID] = true
		}
		nodes, err := s.store.ListNodes(r.Context())
		if err != nil {
			s.logger.ErrorContext(r.Context(), "member editor: list nodes failed", "err", err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, n := range nodes {
			if !taken[n.ID] {
				data.Nodes = append(data.Nodes, n.ID)
			}
		}
		sort.Strings(data.Nodes)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "member-editor", data); err != nil {
		s.logger.ErrorContext(r.Context(), "member editor render failed", "err", err.Error())
	}
}
