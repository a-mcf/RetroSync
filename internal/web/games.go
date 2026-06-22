package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/a-mcf/retrosync/internal/store"
)

// The /games registry UI (slice-11): admin-gated game CRUD plus per-game,
// per-node path-mapping management. Every handler in this file is mounted behind
// requireAuth + requireAdmin (and the mutations behind requireCSRF) in
// Server.Handler, so a non-admin never reaches the Store here (mirrors the
// slice-10 /nodes registry).
//
// No secrets are involved in games or game_paths (a path is not a credential),
// but we keep the same discipline: nothing sensitive is logged.
//
// TODO(slice-registry-sync): the /games registry still manages game_paths, which
// are now orphaned from the engine (the engine syncs via sync_members). The full
// registry→syncs UI move + dropping game_paths + the file picker is slice 16.
// This slice only re-points the binding-touching guards onto the sync-keyed
// binding API so they compile and stay friendly.
//
// Two delete/remove guards, both surfaced as a friendly 409 rather than a 500:
//   - Deleting a game that has any ACTIVE sync is refused — the Store's
//     DeleteGame returns ErrConflict (the FK graph would otherwise cascade a live
//     binding away).
//   - Removing the path of a node that is the active primary of ANY sync of this
//     game is refused — checked here against the game's syncs' bindings before
//     the Store delete.

// --- view-models ---------------------------------------------------------

// gamesPageData drives GET /games (the full page) and the games-list fragment.
type gamesPageData struct {
	User  userView
	Games []gameAdminRow
	// Nodes is the set of node ids the "add a path mapping" <select> offers.
	Nodes []string
	// Q / System echo the active search filter back into the search box.
	Q      string
	System string
	CSRF   string
}

// gameAdminRow is one game in the admin list, with its path mappings and (if
// active) the primary node currently playing it.
type gameAdminRow struct {
	ID      string
	Display string
	System  string
	Notes   string
	// Active is non-empty (the primary node id) when a binding makes this game
	// active; "" when idle.
	Active string
	Paths  []gamePathRow
}

// gamePathRow is one (node, path) mapping for a game, plus whether that node is
// the active primary (so the row's Remove control can warn / be guarded).
type gamePathRow struct {
	NodeID    string
	Path      string
	IsPrimary bool
}

// gameRowContext is the per-row template context: one game plus the shared
// page-level node option set and CSRF token, so the "game-row" template can
// render its add-path form without re-deriving them.
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
// game list (each with its path mappings + active state), an add-game form, and
// per-game edit/delete + add/remove-path controls. Admin-gating is enforced by
// the requireAdmin wrapper.
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

		// A game is "active" when any of its syncs has an active binding. We
		// collect the set of active-primary node ids across the game's syncs so a
		// game_path row on any of those nodes is flagged IsPrimary (guarded against
		// removal). TODO(slice-registry-sync): the registry view moves onto syncs.
		activePrimaries := make(map[string]bool)
		syncs, err := s.store.ListSyncsByGame(ctx, g.ID)
		if err != nil {
			return gamesPageData{}, err
		}
		for _, sy := range syncs {
			if b, err := s.store.GetBinding(ctx, sy.ID); err == nil {
				row.Active = b.PrimaryNode
				activePrimaries[b.PrimaryNode] = true
			} else if !errors.Is(err, store.ErrNotFound) {
				return gamesPageData{}, err
			}
		}

		paths, err := s.store.ListGamePathsByGame(ctx, g.ID)
		if err != nil {
			return gamesPageData{}, err
		}
		for _, p := range paths {
			row.Paths = append(row.Paths, gamePathRow{
				NodeID:    p.NodeID,
				Path:      p.Path,
				IsPrimary: activePrimaries[p.NodeID],
			})
		}
		data.Games = append(data.Games, row)
	}
	sort.Slice(data.Games, func(i, j int) bool { return data.Games[i].ID < data.Games[j].ID })
	return data, nil
}

// --- POST /api/games (create) --------------------------------------------

// handleCreateGame handles POST /api/games. Form fields: id (optional), display,
// system, notes (optional). Per docs/open-questions.md, when id is omitted it is
// auto-generated as a slug from display; a manual id overrides. The final id is
// validated against the shared slug shape. Error mapping:
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

// handleDeleteGame handles POST /api/games/{id}/delete. game_paths cascade on
// delete, and the game's syncs (and their runtime rows) cascade too — but only
// if NONE of those syncs is active. The Store refuses to delete a game with any
// active sync, returning ErrConflict (or ErrInvalidReference) — surfaced as a
// friendly 409 "being played right now" rather than a 500.
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

// --- POST /api/games/{id}/paths/{node_id} (add/update) -------------------

// handleSetGamePath handles POST /api/games/{id}/paths/{node_id}. Body field:
// path. SetGamePath upserts on (game_id, node_id). A missing game or node comes
// back as ErrInvalidReference -> 422. An empty path -> 400.
func (s *Server) handleSetGamePath(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	gameID := r.PathValue("id")
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
	err := s.store.SetGamePath(r.Context(), store.GamePath{GameID: gameID, NodeID: nodeID, Path: path})
	switch {
	case err == nil:
		s.refreshGamesList(w, r, u)
	case errors.Is(err, store.ErrInvalidReference):
		http.Error(w, "no such game or node", http.StatusUnprocessableEntity)
	default:
		s.logger.ErrorContext(r.Context(), "set game path failed", "game", gameID, "node", nodeID, "err", err.Error())
		http.Error(w, "could not save path mapping", http.StatusInternalServerError)
	}
}

// --- POST /api/games/{id}/paths/{node_id}/delete (remove) ----------------

// handleDeleteGamePath handles POST /api/games/{id}/paths/{node_id}/delete.
// Per docs/api.md it is forbidden if this node is the active primary for the
// game: we check the game's syncs' bindings first and return a friendly 409 in
// that case (the Store FK would otherwise let the delete through, since
// game_paths are not what a binding references). Otherwise we delete; a missing
// mapping is a 404.
//
// TODO(slice-registry-sync): re-pointed onto the sync-keyed binding API — the
// node is "the active primary" if it is the primary of ANY active sync of this
// game. The whole game_paths registry moves onto sync_members in slice 16.
func (s *Server) handleDeleteGamePath(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	gameID := r.PathValue("id")
	nodeID := r.PathValue("node_id")

	// Guard: refuse removing the path of a node that is the active primary of any
	// sync of this game.
	// NOTE: ListSyncsByGame/GetBinding-then-DeleteGamePath is a benign TOCTOU — a
	// binding could in principle appear between the reads and the delete. Harmless
	// here: this route is admin-only and not driven concurrently, so the window
	// cannot be raced in practice.
	syncs, err := s.store.ListSyncsByGame(r.Context(), gameID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "delete game path: list syncs failed", "game", gameID, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, sy := range syncs {
		if b, err := s.store.GetBinding(r.Context(), sy.ID); err == nil {
			if b.PrimaryNode == nodeID {
				http.Error(w, "this node is the active primary — stop the session first", http.StatusConflict)
				return
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			s.logger.ErrorContext(r.Context(), "delete game path: get binding failed", "sync", sy.ID, "err", err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	err = s.store.DeleteGamePath(r.Context(), gameID, nodeID)
	switch {
	case err == nil:
		s.refreshGamesList(w, r, u)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such path mapping", http.StatusNotFound)
	default:
		s.logger.ErrorContext(r.Context(), "delete game path failed", "game", gameID, "node", nodeID, "err", err.Error())
		http.Error(w, "could not remove path mapping", http.StatusInternalServerError)
	}
}

// --- shared form parsing / error mapping ---------------------------------

// parseGameForm reads and validates the game create/edit form. idOverride, when
// non-empty (edit), is the authoritative game id (the path id); for create it is
// "" and the id comes from the form — or, if the form's id is blank, is
// auto-generated as a slug from display (docs/open-questions.md). It returns the
// assembled Game and a non-empty human message on a validation failure.
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
