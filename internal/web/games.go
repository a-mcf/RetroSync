package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/a-mcf/retrosync/internal/store"
)

// The /games registry UI (slice-16): admin-gated game CRUD, plus per-game sync
// management and per-sync member (node + path) management. Every handler in this
// file is mounted behind requireAuth + requireAdmin (and the mutations behind
// requireCSRF) in Server.Handler, so a non-admin never reaches the Store here
// (mirrors the slice-10 /nodes registry).
//
// A sync is the unit of mirroring; its members (node + save-file path) are its
// scope. The registry edits syncs + sync_members directly; game_paths is gone.
//
// No secrets are involved in syncs or members (a path is not a credential), but
// we keep the same discipline: nothing sensitive is logged.
//
// Two delete/remove guards, both surfaced as a friendly 409 rather than a 500:
//   - Deleting a SYNC that has an active binding is refused — tearing it down
//     would cascade a live session away. Checked here against GetBinding(syncID).
//   - Removing a MEMBER whose node is the active primary of the sync is refused
//     — checked here against the sync's binding before the Store delete.
//
// TODO(slice-picker): member paths are typed by hand here (a text field), the
// same as game_paths was. The save-file discovery / file picker is slice 17.

// --- view-models ---------------------------------------------------------

// gamesPageData drives GET /games (the full page) and the games-list fragment.
type gamesPageData struct {
	User  userView
	Games []gameAdminRow
	// Nodes is the set of node ids the "add a member" <select> offers.
	Nodes []string
	// Q / System echo the active search filter back into the search box.
	Q      string
	System string
	CSRF   string
}

// gameAdminRow is one game in the admin list, with its syncs (each with their
// members) and, if any sync is active, the primary node currently playing it.
type gameAdminRow struct {
	ID      string
	Display string
	System  string
	Notes   string
	// Active is non-empty (the primary node id of the first active sync) when a
	// binding makes this game active; "" when idle.
	Active string
	Syncs  []syncAdminRow
}

// syncAdminRow is one sync of a game, with its members and active state.
type syncAdminRow struct {
	ID      string
	Name    string
	Members []syncMemberRow
	// ActivePrimary is the node currently bound as primary for this sync, or ""
	// if the sync is idle. Used to badge the primary member and guard removal.
	ActivePrimary string
}

// syncMemberRow is one (node, path) member of a sync, plus whether that node is
// the sync's active primary (so the Remove control can warn / be guarded).
type syncMemberRow struct {
	NodeID    string
	Path      string
	IsPrimary bool
}

// gameRowContext is the per-row template context: one game plus the shared
// page-level node option set and CSRF token, so the "game-row" template can
// render its forms without re-deriving them.
type gameRowContext struct {
	Game  gameAdminRow
	Nodes []string
	CSRF  string
}

// gameRowCtx builds a gameRowContext from the page data and a single row. It is
// registered as a template func so "games-list" can pass each row what its forms
// need.
func gameRowCtx(page gamesPageData, row gameAdminRow) gameRowContext {
	return gameRowContext{Game: row, Nodes: page.Nodes, CSRF: page.CSRF}
}

// --- GET /games ----------------------------------------------------------

// handleGamesPage renders the admin game registry: a search box, the filtered
// game list (each with its syncs + members + active state), an add-game form,
// and per-game/per-sync controls. Admin-gating is enforced by the requireAdmin
// wrapper.
func (s *Server) handleGamesPage(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	f := store.GameFilter{
		Q:      strings.TrimSpace(r.URL.Query().Get("q")),
		System: strings.TrimSpace(r.URL.Query().Get("system")),
	}
	data, err := s.buildGamesPage(r.Context(), u, f)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "games page build failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data.CSRF = s.csrfFor(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// An HTMX search request targets just the list region; a full navigation
	// renders the whole page.
	tmpl := "games"
	if r.Header.Get("HX-Request") == "true" {
		tmpl = "games-list"
	}
	if err := s.templates.ExecuteTemplate(w, tmpl, data); err != nil {
		s.logger.ErrorContext(r.Context(), "games page render failed", "err", err.Error())
	}
}

func (s *Server) buildGamesPage(ctx context.Context, u store.User, f store.GameFilter) (gamesPageData, error) {
	games, err := s.store.ListGames(ctx, f)
	if err != nil {
		return gamesPageData{}, err
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return gamesPageData{}, err
	}

	data := gamesPageData{
		User:   userView{ID: u.ID, Display: u.Display, Role: u.Role},
		Q:      f.Q,
		System: f.System,
	}
	for _, n := range nodes {
		data.Nodes = append(data.Nodes, n.ID)
	}
	sort.Strings(data.Nodes)

	for _, g := range games {
		row := gameAdminRow{ID: g.ID, Display: g.Display, System: g.System, Notes: g.Notes}

		syncs, err := s.store.ListSyncsByGame(ctx, g.ID)
		if err != nil {
			return gamesPageData{}, err
		}
		for _, sy := range syncs {
			sr := syncAdminRow{ID: sy.ID, Name: sy.Name}
			if b, err := s.store.GetBinding(ctx, sy.ID); err == nil {
				sr.ActivePrimary = b.PrimaryNode
				row.Active = b.PrimaryNode
			} else if !errors.Is(err, store.ErrNotFound) {
				return gamesPageData{}, err
			}
			members, err := s.store.ListSyncMembers(ctx, sy.ID)
			if err != nil {
				return gamesPageData{}, err
			}
			for _, m := range members {
				sr.Members = append(sr.Members, syncMemberRow{
					NodeID:    m.NodeID,
					Path:      m.Path,
					IsPrimary: sr.ActivePrimary == m.NodeID,
				})
			}
			row.Syncs = append(row.Syncs, sr)
		}
		data.Games = append(data.Games, row)
	}
	sort.Slice(data.Games, func(i, j int) bool { return data.Games[i].ID < data.Games[j].ID })
	return data, nil
}

// --- POST /api/games (create) --------------------------------------------

// handleCreateGame handles POST /api/games. Form fields: id (optional), display,
// system, notes (optional). When id is omitted it is auto-generated as a slug
// from display; a manual id overrides. Error mapping:
//   - bad/empty fields or invalid slug (local)  -> 400
//   - duplicate id (ErrConflict)                -> 409
func (s *Server) handleCreateGame(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	g, verr := parseGameForm(r, "")
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}
	err := s.store.CreateGame(r.Context(), g)
	if s.handleGameWriteErr(w, r, err) {
		return
	}
	s.refreshGamesList(w, r, u)
}

// --- POST /api/games/{id} (edit) -----------------------------------------

// handleEditGame handles POST /api/games/{id}. It rewrites the game's mutable
// fields. The path id is authoritative (any id field in the form is ignored).
// Same error mapping as create plus ErrNotFound -> 404.
func (s *Server) handleEditGame(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	g, verr := parseGameForm(r, id)
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}
	err := s.store.UpdateGame(r.Context(), g)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such game", http.StatusNotFound)
		return
	}
	if s.handleGameWriteErr(w, r, err) {
		return
	}
	s.refreshGamesList(w, r, u)
}

// --- POST /api/games/{id}/delete -----------------------------------------

// handleDeleteGame handles POST /api/games/{id}/delete. The game's syncs (and
// their members + runtime rows) cascade — but only if NONE of those syncs is
// active. The Store refuses to delete a game with any active sync, returning
// ErrConflict — surfaced as a friendly 409 "being played right now".
func (s *Server) handleDeleteGame(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	err := s.store.DeleteGame(r.Context(), id)
	switch {
	case err == nil:
		s.refreshGamesList(w, r, u)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such game", http.StatusNotFound)
	case errors.Is(err, store.ErrInvalidReference) || errors.Is(err, store.ErrConflict):
		http.Error(w, "game is being played right now — stop the session first", http.StatusConflict)
	default:
		s.logger.ErrorContext(r.Context(), "delete game failed", "game", id, "err", err.Error())
		http.Error(w, "could not delete game", http.StatusInternalServerError)
	}
}

// --- POST /api/syncs (create) --------------------------------------------

// handleCreateSync handles POST /api/syncs. Form fields: game_id (required),
// name (required), id (optional — auto-generated as a slug from game+name when
// omitted). Error mapping:
//   - bad/empty fields or invalid derived slug (local) -> 400
//   - missing game (ErrInvalidReference)               -> 422
//   - duplicate id (ErrConflict)                       -> 409
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
	gameID := strings.TrimSpace(r.PostFormValue("game_id"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	if gameID == "" {
		http.Error(w, "game_id is required", http.StatusBadRequest)
		return
	}
	if name == "" {
		http.Error(w, "a sync name is required", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("id"))
	if id == "" {
		// Auto-id from game + name (e.g. super-metroid + "Bob's stream" ->
		// super-metroid-bob-s-stream).
		id = slugify(gameID + "-" + name)
	}
	if !validSlug(id) {
		http.Error(w, "id must be a slug: lowercase letters, digits, and hyphens", http.StatusBadRequest)
		return
	}
	err := s.store.CreateSync(r.Context(), store.Sync{ID: id, GameID: gameID, Name: name})
	switch {
	case err == nil:
		s.refreshGamesList(w, r, u)
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "a sync with that id already exists", http.StatusConflict)
	case errors.Is(err, store.ErrInvalidReference):
		http.Error(w, "no such game", http.StatusUnprocessableEntity)
	default:
		s.logger.ErrorContext(r.Context(), "create sync failed", "sync", id, "err", err.Error())
		http.Error(w, "could not create sync", http.StatusInternalServerError)
	}
}

// --- POST /api/syncs/{id} (rename) ---------------------------------------

// handleRenameSync handles POST /api/syncs/{id}. Body field: name. The sync id
// and game_id are immutable here (the path id is authoritative); only the name
// changes. Empty name -> 400; missing sync -> 404.
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
	// Read the existing sync to preserve its game_id (immutable via this route).
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
	err = s.store.UpdateSync(r.Context(), sy)
	switch {
	case err == nil:
		s.refreshGamesList(w, r, u)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such sync", http.StatusNotFound)
	default:
		s.logger.ErrorContext(r.Context(), "rename sync failed", "sync", id, "err", err.Error())
		http.Error(w, "could not rename sync", http.StatusInternalServerError)
	}
}

// --- POST /api/syncs/{id}/delete -----------------------------------------

// handleDeleteSync handles POST /api/syncs/{id}/delete. A sync with an active
// binding is refused with a friendly 409 (deleting it would cascade away a live
// session). Otherwise we delete; its members cascade. Missing sync -> 404.
func (s *Server) handleDeleteSync(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")

	// Guard: refuse deleting an active sync (don't tear down a live session).
	// NOTE: GetBinding-then-DeleteSync is a benign TOCTOU — admin-only, not driven
	// concurrently, so the window cannot be raced in practice.
	if _, err := s.store.GetBinding(r.Context(), id); err == nil {
		http.Error(w, "this sync is being played right now — stop the session first", http.StatusConflict)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.logger.ErrorContext(r.Context(), "delete sync: get binding failed", "sync", id, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	err := s.store.DeleteSync(r.Context(), id)
	switch {
	case err == nil:
		s.refreshGamesList(w, r, u)
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
//   - empty path (local)                                  -> 400
//   - missing sync or node (ErrInvalidReference)          -> 422
//   - (node, path) already a member of ANOTHER sync       -> 409
//     (the global UNIQUE (node_id, path) invariant)
//
// TODO(slice-picker): the path is typed by hand; the file picker lands later.
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
	err := s.store.SetSyncMember(r.Context(), store.SyncMember{SyncID: syncID, NodeID: nodeID, Path: path})
	switch {
	case err == nil:
		s.refreshGamesList(w, r, u)
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
// Refused with a friendly 409 if that node is the active primary of this sync
// (checked against the binding before delete). Otherwise we delete; a missing
// member is a 404.
func (s *Server) handleDeleteSyncMember(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	syncID := r.PathValue("id")
	nodeID := r.PathValue("node_id")

	// Guard: refuse removing the member whose node is the active primary of this
	// sync (benign TOCTOU — admin-only, not raced in practice).
	if b, err := s.store.GetBinding(r.Context(), syncID); err == nil {
		if b.PrimaryNode == nodeID {
			http.Error(w, "this node is the active primary — stop the session first", http.StatusConflict)
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.logger.ErrorContext(r.Context(), "delete member: get binding failed", "sync", syncID, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	err := s.store.DeleteSyncMember(r.Context(), syncID, nodeID)
	switch {
	case err == nil:
		s.refreshGamesList(w, r, u)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such member", http.StatusNotFound)
	default:
		s.logger.ErrorContext(r.Context(), "delete sync member failed", "sync", syncID, "node", nodeID, "err", err.Error())
		http.Error(w, "could not remove member", http.StatusInternalServerError)
	}
}

// --- shared form parsing / error mapping ---------------------------------

// parseGameForm reads and validates the game create/edit form. idOverride, when
// non-empty (edit), is the authoritative game id (the path id); for create it is
// "" and the id comes from the form — or, if the form's id is blank, is
// auto-generated as a slug from display. It returns the assembled Game and a
// non-empty human message on a validation failure.
func parseGameForm(r *http.Request, idOverride string) (store.Game, string) {
	if err := r.ParseForm(); err != nil {
		return store.Game{}, "bad request"
	}
	display := strings.TrimSpace(r.PostFormValue("display"))
	if display == "" {
		return store.Game{}, "display is required"
	}

	id := idOverride
	if id == "" {
		id = strings.TrimSpace(r.PostFormValue("id"))
		if id == "" {
			// No manual id: auto-generate a slug from the display.
			id = slugify(display)
		}
	}
	if id == "" {
		return store.Game{}, "could not derive an id from the display — provide an explicit id"
	}
	if !validSlug(id) {
		return store.Game{}, "id must be a slug: lowercase letters, digits, and hyphens (e.g. super-metroid)"
	}

	g := store.Game{
		ID:      id,
		Display: display,
		System:  strings.TrimSpace(r.PostFormValue("system")),
		Notes:   strings.TrimSpace(r.PostFormValue("notes")),
	}
	if g.System == "" {
		return store.Game{}, "system is required"
	}
	return g, ""
}

// handleGameWriteErr maps a CreateGame/UpdateGame error to an HTTP response and
// reports whether it handled one (true). It does NOT handle ErrNotFound
// (edit-specific) — the caller maps that first.
func (s *Server) handleGameWriteErr(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "a game with that id already exists", http.StatusConflict)
	default:
		s.logger.ErrorContext(r.Context(), "game write failed", "err", err.Error())
		http.Error(w, "could not save game", http.StatusInternalServerError)
	}
	return true
}

// refreshGamesList re-renders the games-list fragment (the #games-list region)
// after a successful mutation so HTMX swaps the updated list in place. It
// preserves the active search filter so a mutation doesn't reset the view.
func (s *Server) refreshGamesList(w http.ResponseWriter, r *http.Request, u store.User) {
	f := store.GameFilter{
		Q:      strings.TrimSpace(r.URL.Query().Get("q")),
		System: strings.TrimSpace(r.URL.Query().Get("system")),
	}
	data, err := s.buildGamesPage(r.Context(), u, f)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "refresh games build failed", "err", err.Error())
		http.Redirect(w, r, "/games", http.StatusSeeOther)
		return
	}
	data.CSRF = s.csrfFor(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "games-list", data); err != nil {
		s.logger.ErrorContext(r.Context(), "refresh games render failed", "err", err.Error())
	}
}
