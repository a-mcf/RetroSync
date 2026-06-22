package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/store"
)

// --- view-models for the conflict modal ----------------------------------

// conflictModalData drives the conflict-resolution modal fragment. It lists
// every in-scope node's CURRENT live state (mtime, size) with a "Use this node"
// winner button per node that holds a file, so the human can pick the winner
// with full disclosure (docs/state-machine.md "UI shows every node's current
// state ... and a 'use this one' button per node").
type conflictModalData struct {
	SyncID      string
	SyncName    string
	GameDisplay string
	System      string
	Nodes       []conflictNode
	CSRF        string
}

// conflictNode is one in-scope node's live state in the conflict modal.
type conflictNode struct {
	NodeID string
	// Present is false when the node currently has no file; the row shows
	// "no save" and offers NO winner button (you cannot pick an empty file).
	Present bool
	Mtime   string // formatted live mtime, or "" when !Present
	Size    string // human-readable size, or "" when !Present
}

// handleConflictModal serves GET /syncs/{id}/conflict as an HTML modal
// fragment. It is intentionally read-only and NOT owner-gated: any authenticated
// user may VIEW a conflict (consistent with "can see others' sessions",
// docs/ui.md / brief D). The destructive resolve POST it submits to re-checks
// owner-or-admin AND CSRF.
func (s *Server) handleConflictModal(w http.ResponseWriter, r *http.Request) {
	if s.actioner == nil {
		s.logger.ErrorContext(r.Context(), "conflict modal: no actioner wired")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	syncID := r.PathValue("id")

	sy, err := s.store.GetSync(r.Context(), syncID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such sync", http.StatusNotFound)
			return
		}
		s.logger.ErrorContext(r.Context(), "conflict modal: get sync failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	g, err := s.store.GetGame(r.Context(), sy.GameID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such game", http.StatusNotFound)
			return
		}
		s.logger.ErrorContext(r.Context(), "conflict modal: get game failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	states, err := s.actioner.NodeStates(r.Context(), syncID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "conflict modal: node states failed", "sync", syncID, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := conflictModalData{
		SyncID:      sy.ID,
		SyncName:    sy.Name,
		GameDisplay: g.Display,
		System:      g.System,
		CSRF:        s.csrfFor(r),
	}
	for _, st := range states {
		cn := conflictNode{NodeID: st.NodeID, Present: st.Present}
		if st.Present {
			cn.Mtime = fmtMtime(&st.Mtime)
			cn.Size = fmtSize(st.Size)
		}
		data.Nodes = append(data.Nodes, cn)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "conflict-modal", data); err != nil {
		s.logger.ErrorContext(r.Context(), "conflict modal render failed", "err", err.Error())
	}
}

// handleResolveConflict handles POST /api/syncs/{id}/resolve-conflict. Form
// field: winner_node_id.
//
// This is the single most destructive action in the system (it overwrites every
// peer's save with the winner's). Authorization (auditor-mandated, same rule as
// deactivate): only the OWNER of the binding's primary node — or an admin — may
// pick a winner. The check happens BEFORE the Actioner is called, so a
// non-owning user never reaches the engine. CSRF is enforced by the requireCSRF
// wrapper around this handler.
//
// Engine error mapping:
//   - ErrNotConflicted     -> 409 + dashboard refresh (someone else already
//     resolved it; the refreshed dashboard drops the banner).
//   - ErrNoPath            -> 400 (winner is not a member of this sync).
//   - ErrSourceMissing     -> 422 (winner currently holds no file).
//   - anything else        -> 500.
func (s *Server) handleResolveConflict(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.actioner == nil {
		s.logger.ErrorContext(r.Context(), "resolve-conflict: no actioner wired")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	syncID := r.PathValue("id")
	winner := strings.TrimSpace(r.PostFormValue("winner_node_id"))

	// Authorization FIRST, before any validation that could reach the engine. The
	// resolve action requires that the user own the binding's primary node (or be
	// admin) — the same rule as deactivate, because this is the most destructive
	// action and a wrong user must not pick a winner. A non-conflicted/idle sync
	// is still owner-gated here: there is nothing to resolve, but a stranger
	// shouldn't be probing the action either.
	b, err := s.store.GetBinding(r.Context(), syncID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// No binding: nothing to resolve. Treat as already-resolved (409) and
			// refresh, rather than leaking whether the sync exists.
			http.Error(w, "no active session to resolve", http.StatusConflict)
			return
		}
		s.logger.ErrorContext(r.Context(), "resolve-conflict: get binding failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	owns, err := s.userOwnsNode(r.Context(), u, b.PrimaryNode)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "resolve-conflict: ownership check failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !owns {
		http.Error(w, "forbidden: you do not own that session's node", http.StatusForbidden)
		return
	}

	// Validate input only AFTER authorization: a non-owner gets 403 regardless of
	// what they submit, and never reaches the engine.
	if winner == "" {
		http.Error(w, "winner_node_id required", http.StatusBadRequest)
		return
	}

	err = s.actioner.ResolveConflict(r.Context(), syncID, winner)
	switch {
	case err == nil:
		s.refreshDashboard(w, r, u)
	case errors.Is(err, engine.ErrNotConflicted):
		// Already resolved (e.g. a double-submit, or a peer resolved it first).
		// Refresh the dashboard so the now-stale banner disappears; 409 signals the
		// no-op without an error page.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("HX-Retarget", "#dashboard")
		w.Header().Set("HX-Reswap", "outerHTML")
		w.WriteHeader(http.StatusConflict)
		s.writeDashboardFragment(w, r, u)
	case errors.Is(err, engine.ErrNoPath):
		http.Error(w, "that node is not a member of this sync", http.StatusBadRequest)
	case errors.Is(err, engine.ErrSourceMissing):
		http.Error(w, "that node currently has no save to use", http.StatusUnprocessableEntity)
	default:
		s.logger.ErrorContext(r.Context(), "resolve-conflict failed", "sync", syncID, "err", err.Error())
		http.Error(w, "could not resolve conflict", http.StatusInternalServerError)
	}
}
