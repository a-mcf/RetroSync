package web

import (
	"context"
	"errors"
	"net/http"

	"github.com/a-mcf/retrosync/internal/store"
)

// This file holds the shared authorization + dashboard-refresh helpers used by
// the play-side handlers. Under auto-mirror there are no activate/deactivate/
// take-over actions; the only state-changing play action is conflict resolution
// (see conflict.go), which reuses these helpers.

// --- helpers -------------------------------------------------------------

// userOwnsAnyMember reports whether u has authority over a sync: true for an
// admin, or when u owns at least one of the sync's member nodes. This is the
// auto-mirror authority rule for conflict resolution — there is no "primary"
// anymore, so authority comes from owning a node that is part of the sync. A
// member node that no longer exists is skipped (a stale member must not grant or
// deny authority on its own). The sync's member list itself is the source of
// truth; an empty member set yields false for a non-admin.
func (s *Server) userOwnsAnyMember(ctx context.Context, u store.User, syncID string) (bool, error) {
	if u.Role == store.RoleAdmin {
		return true, nil
	}
	members, err := s.store.ListSyncMembers(ctx, syncID)
	if err != nil {
		return false, err
	}
	for _, m := range members {
		n, err := s.store.GetNode(ctx, m.NodeID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // a stale member node: ignore it
			}
			return false, err
		}
		if n.OwnerUserID != nil && *n.OwnerUserID == u.ID {
			return true, nil
		}
	}
	return false, nil
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
