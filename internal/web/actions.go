package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/store"
)

// --- view-models for the activation modal --------------------------------

// activateModalData drives the "use my save" activation modal fragment.
type activateModalData struct {
	SyncID      string
	SyncName    string
	GameDisplay string
	System      string
	// PrimaryNode is the node the user is binding FROM (the node they clicked
	// "Play on"). It becomes active_bindings.primary_node.
	PrimaryNode string
	// Sources is every member of this sync, with its last-known mtime — surfaced
	// BEFORE binding so the user sees what each "start from" choice would
	// overwrite (docs/state-machine.md "surface, don't hide"). Default-selected is
	// PrimaryNode ("use my save").
	Sources []modalSource
	CSRF    string
}

// modalSource is one selectable "start from" node in the modal.
type modalSource struct {
	NodeID string
	// Direction is the form value this radio submits: "from-primary" when this
	// node IS the primary, else "from-peer-<nodeID>".
	Direction string
	Mtime     string // last-known mtime, or "no save yet"
	HasSave   bool
	Selected  bool // default radio = the node the user is binding from
	IsPrimary bool
}

// handleActivateModal serves GET /syncs/{id}/activate?node=<nodeID> as an HTML
// modal fragment. It lists every member of this sync with that node's last-known
// mtime (from the manifest), defaulting the radio to the binding node (the
// `node` query param — "use my save").
//
// This is a read-only GET (no CSRF needed); the POST it submits to is
// CSRF-protected and re-checks node ownership.
func (s *Server) handleActivateModal(w http.ResponseWriter, r *http.Request) {
	syncID := r.PathValue("id")
	bindingNode := r.URL.Query().Get("node")

	data, err := s.buildActivateModal(r.Context(), syncID, bindingNode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such sync", http.StatusNotFound)
			return
		}
		s.logger.ErrorContext(r.Context(), "activate modal build failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data.CSRF = s.csrfFor(r)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "activate-modal", data); err != nil {
		s.logger.ErrorContext(r.Context(), "activate modal render failed", "err", err.Error())
	}
}

// buildActivateModal assembles the modal view-model. It surfaces EVERY member's
// last-known mtime (per the slice-3 audit contract), defaulting selection to
// bindingNode. When only one node has a save, that node is pre-selected instead
// (still showing all mtimes), per docs/ui.md.
func (s *Server) buildActivateModal(ctx context.Context, syncID, bindingNode string) (activateModalData, error) {
	sy, err := s.store.GetSync(ctx, syncID)
	if err != nil {
		return activateModalData{}, err
	}
	g, err := s.store.GetGame(ctx, sy.GameID)
	if err != nil {
		return activateModalData{}, err
	}

	members, err := s.store.ListSyncMembers(ctx, syncID)
	if err != nil {
		return activateModalData{}, err
	}

	data := activateModalData{
		SyncID:      sy.ID,
		SyncName:    sy.Name,
		GameDisplay: g.Display,
		System:      g.System,
		PrimaryNode: bindingNode,
	}

	savesCount := 0
	for _, m := range members {
		mtime := "no save yet"
		hasSave := false
		if me, err := s.store.GetManifest(ctx, syncID, m.NodeID); err == nil {
			if me.Mtime != nil {
				mtime = fmtMtime(me.Mtime)
				hasSave = true
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return activateModalData{}, err
		}
		if hasSave {
			savesCount++
		}
		dir := "from-peer-" + m.NodeID
		if m.NodeID == bindingNode {
			dir = "from-primary"
		}
		data.Sources = append(data.Sources, modalSource{
			NodeID:    m.NodeID,
			Direction: dir,
			Mtime:     mtime,
			HasSave:   hasSave,
			IsPrimary: m.NodeID == bindingNode,
		})
	}
	sort.Slice(data.Sources, func(i, j int) bool { return data.Sources[i].NodeID < data.Sources[j].NodeID })

	// Default selection. Primary "use my save" if the binding node has a save;
	// otherwise if exactly one node has a save, pre-select that one; otherwise
	// fall back to the binding node so a default always exists.
	selectDefault(data.Sources, bindingNode, savesCount)
	return data, nil
}

// selectDefault marks exactly one source Selected per docs/state-machine.md /
// docs/ui.md: prefer the binding node ("use my save"); else the lone save; else
// the binding node regardless.
func selectDefault(sources []modalSource, bindingNode string, savesCount int) {
	// Prefer the binding node when it itself holds a save.
	for i := range sources {
		if sources[i].NodeID == bindingNode && sources[i].HasSave {
			sources[i].Selected = true
			return
		}
	}
	// Else, if exactly one node has a save, pre-select it.
	if savesCount == 1 {
		for i := range sources {
			if sources[i].HasSave {
				sources[i].Selected = true
				return
			}
		}
	}
	// Else default to the binding node (no save anywhere yet, or ambiguous).
	for i := range sources {
		if sources[i].NodeID == bindingNode {
			sources[i].Selected = true
			return
		}
	}
}

// --- POST /api/syncs/{id}/activate ---------------------------------------

// handleActivate handles POST /api/syncs/{id}/activate. Form fields:
//
//	primary_node  the node to bind as primary (must be owned by the user, or
//	              the user must be admin — docs/auth.md)
//	direction     "from-primary" or "from-peer-<nodeID>"
//	force         optional; "true"/"1"/"on" => force-takeover
//
// On store.ErrConflict without force it returns 409 plus a "take over" fragment
// that re-POSTs with force=true. On success it returns the refreshed dashboard
// fragment.
func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.actioner == nil {
		s.logger.ErrorContext(r.Context(), "activate: no actioner wired")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	syncID := r.PathValue("id")
	primaryNode := strings.TrimSpace(r.PostFormValue("primary_node"))
	direction := strings.TrimSpace(r.PostFormValue("direction"))
	force := isTrue(r.PostFormValue("force"))

	if primaryNode == "" {
		http.Error(w, "primary_node required", http.StatusBadRequest)
		return
	}
	if direction == "" {
		// UI default per api.md: bind from the primary's own current file.
		direction = "from-primary"
	}

	// Authorization (docs/auth.md): a user may only bind a sync onto a node they
	// OWN; admin may bind any. Enforced BEFORE calling the Actioner so an
	// unauthorized request never reaches the engine.
	owns, err := s.userOwnsNode(r.Context(), u, primaryNode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such node", http.StatusNotFound)
			return
		}
		s.logger.ErrorContext(r.Context(), "activate: ownership check failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !owns {
		http.Error(w, "forbidden: you do not own that node", http.StatusForbidden)
		return
	}

	err = s.actioner.Activate(r.Context(), syncID, primaryNode, direction, force)
	switch {
	case err == nil:
		s.refreshDashboard(w, r, u)
	case errors.Is(err, store.ErrConflict):
		// Already bound to a different primary and force was not set: 409 + a
		// fragment offering "Take over from <other>" which re-POSTs force=true.
		s.renderTakeover(w, r, syncID, primaryNode, direction)
	case errors.Is(err, engine.ErrPrimaryNotMember):
		// The caller owns primaryNode but it is NOT a member of this sync, so it
		// has no claim to the sync's play authority. Ownership passed the check
		// above; the engine's member gate is what rejects it. 422: the request is
		// well-formed but names a primary that is not a valid member.
		http.Error(w, "that node is not a member of this sync", http.StatusUnprocessableEntity)
	default:
		s.logger.ErrorContext(r.Context(), "activate failed", "sync", syncID, "err", err.Error())
		http.Error(w, "could not start session", http.StatusInternalServerError)
	}
}

// renderTakeover writes a 409 with the take-over fragment. It looks up the
// current primary (the "<other>" node) for the button label; if that lookup
// fails it still renders with a generic label rather than erroring.
func (s *Server) renderTakeover(w http.ResponseWriter, r *http.Request, syncID, primaryNode, direction string) {
	other := ""
	if b, err := s.store.GetBinding(r.Context(), syncID); err == nil {
		other = b.PrimaryNode
	}
	data := struct {
		SyncID      string
		PrimaryNode string
		Direction   string
		Other       string
		CSRF        string
	}{
		SyncID:      syncID,
		PrimaryNode: primaryNode,
		Direction:   direction,
		Other:       other,
		CSRF:        s.csrfFor(r),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Land the takeover fragment in the modal mount regardless of which element
	// initiated the request (the direct Play button targets #dashboard).
	w.Header().Set("HX-Retarget", "#modal")
	w.Header().Set("HX-Reswap", "innerHTML")
	w.WriteHeader(http.StatusConflict)
	if err := s.templates.ExecuteTemplate(w, "takeover", data); err != nil {
		s.logger.ErrorContext(r.Context(), "takeover render failed", "err", err.Error())
	}
}

// --- POST /api/syncs/{id}/deactivate -------------------------------------

// handleDeactivate handles POST /api/syncs/{id}/deactivate. Idempotent
// (deactivating an idle sync is a no-op). Refreshes the dashboard on success.
//
// Authorization (docs/auth.md): a user "can see other users' active sessions
// but not modify them." Deactivate MODIFIES a session, so it requires that the
// user own the binding's primary node (or be admin). The check happens BEFORE
// the Actioner is called, so an unauthorized request never reaches the engine.
// If the sync is already idle (no binding), there is nothing to protect:
// deactivating an idle sync is a harmless idempotent no-op (per api.md), so we
// allow it through rather than 403.
func (s *Server) handleDeactivate(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.actioner == nil {
		s.logger.ErrorContext(r.Context(), "deactivate: no actioner wired")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	syncID := r.PathValue("id")

	// Ownership: look up the current binding. If one exists, only the owner of
	// its primary node (or an admin) may stop it. If the sync is idle, fall
	// through to the idempotent no-op.
	if b, err := s.store.GetBinding(r.Context(), syncID); err == nil {
		owns, err := s.userOwnsNode(r.Context(), u, b.PrimaryNode)
		if err != nil {
			s.logger.ErrorContext(r.Context(), "deactivate: ownership check failed", "err", err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !owns {
			http.Error(w, "forbidden: you do not own that session's node", http.StatusForbidden)
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.logger.ErrorContext(r.Context(), "deactivate: get binding failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := s.actioner.Deactivate(r.Context(), syncID); err != nil {
		s.logger.ErrorContext(r.Context(), "deactivate failed", "sync", syncID, "err", err.Error())
		http.Error(w, "could not stop session", http.StatusInternalServerError)
		return
	}
	s.refreshDashboard(w, r, u)
}

// --- helpers -------------------------------------------------------------

// userOwnsNode reports whether u may act on nodeID: true for an admin (any
// node) or for the node's owner. ErrNotFound is propagated so the caller can
// 404 a non-existent node.
func (s *Server) userOwnsNode(ctx context.Context, u store.User, nodeID string) (bool, error) {
	if u.Role == store.RoleAdmin {
		return true, nil
	}
	n, err := s.store.GetNode(ctx, nodeID)
	if err != nil {
		return false, err
	}
	return n.OwnerUserID != nil && *n.OwnerUserID == u.ID, nil
}

// refreshDashboard re-renders the full dashboard with an implicit 200. HTMX
// swaps it into the page; a non-HTMX form post sees the same HTML (progressive
// enhancement). On a build error it falls back to a redirect to "/".
func (s *Server) refreshDashboard(w http.ResponseWriter, r *http.Request, u store.User) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.writeDashboardFragment(w, r, u)
}

// writeDashboardFragment renders the dashboard body without writing a status
// line, so callers that have already set a non-200 status (e.g. the
// resolve-conflict "already resolved" 409 refresh) can reuse the same HTML. On a
// build error it redirects to "/" — but only if no status/body has been written
// yet (the caller must not have called WriteHeader before a possible failure).
func (s *Server) writeDashboardFragment(w http.ResponseWriter, r *http.Request, u store.User) {
	data, err := s.buildDashboard(r.Context(), u)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "refresh dashboard build failed", "err", err.Error())
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	data.CSRF = s.csrfFor(r)
	if err := s.templates.ExecuteTemplate(w, "dashboard", data); err != nil {
		s.logger.ErrorContext(r.Context(), "refresh dashboard render failed", "err", err.Error())
	}
}

// isTrue parses a checkbox/flag form value.
func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "on", "yes":
		return true
	default:
		return false
	}
}
