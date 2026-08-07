package web

import (
	"errors"
	"net/http"
	"sort"
	"strconv"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/store"
)

// historyListLimit caps how many versions are shown per member on the history
// page. Retention already keeps only the newest store.SaveVersionRetention, so
// this is a belt-and-suspenders cap that also bounds the query.
const historyListLimit = store.SaveVersionRetention

// --- view-models for the save-history page -------------------------------

// historyPageData drives the save-history page: every member of the sync with
// its captured save versions (newest-first), each offering a Restore button.
type historyPageData struct {
	// User drives the shared nav partial, which is the same on every page.
	User     userView
	SyncID   string
	SyncName string
	// Game is the sync's free-text game label (a display grouping; "" when unset).
	Game    string
	Members []historyMember
	CSRF    string
}

// historyMember is one member node and its captured versions on the history page.
type historyMember struct {
	NodeID   string
	Versions []historyVersion
}

// historyVersion is one captured save version row (metadata only — no bytes).
type historyVersion struct {
	Seq    int64
	When   string // captured_at as "N ago"
	Reason string
	Size   string // human-readable size
}

// handleHistoryPage serves GET /syncs/{id}/history: the read-only save-history
// page listing every member's captured versions with a Restore button per
// version. Like the conflict modal it is NOT owner-gated (any authenticated user
// may VIEW history, consistent with "can see others' syncs"); the destructive
// restore POST re-checks owner-of-a-member-or-admin AND CSRF.
func (s *Server) handleHistoryPage(w http.ResponseWriter, r *http.Request) {
	syncID := r.PathValue("id")

	// Needed only to render the shared nav; requireAuth already guarantees it.
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	sy, err := s.store.GetSync(r.Context(), syncID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such sync", http.StatusNotFound)
			return
		}
		s.logger.ErrorContext(r.Context(), "history: get sync failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	members, err := s.store.ListSyncMembers(r.Context(), syncID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "history: list members failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	now := s.now()
	data := historyPageData{
		User:     userView{ID: u.ID, Display: u.Display, Role: u.Role},
		SyncID:   sy.ID,
		SyncName: sy.Name,
		Game:     sy.Game,
		CSRF:     s.csrfFor(r),
	}
	for _, m := range members {
		vs, err := s.store.ListSaveVersions(r.Context(), syncID, m.NodeID, historyListLimit)
		if err != nil {
			s.logger.ErrorContext(r.Context(), "history: list versions failed", "node", m.NodeID, "err", err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		hm := historyMember{NodeID: m.NodeID}
		for _, v := range vs {
			captured := v.CapturedAt
			hm.Versions = append(hm.Versions, historyVersion{
				Seq:    v.Seq,
				When:   fmtTimeAgo(&captured, now),
				Reason: v.Reason,
				Size:   fmtSize(v.Size),
			})
		}
		data.Members = append(data.Members, hm)
	}
	sort.Slice(data.Members, func(i, j int) bool { return data.Members[i].NodeID < data.Members[j].NodeID })

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "history", data); err != nil {
		s.logger.ErrorContext(r.Context(), "history render failed", "err", err.Error())
	}
}

// handleRestoreVersion handles POST /api/syncs/{id}/versions/{seq}/restore. It
// makes the chosen captured version authoritative again and propagates it to the
// sync's members (engine.RestoreVersion).
//
// Authorization (same rule as resolve-conflict): the user must own at least one
// of the sync's member nodes, or be an admin. The check happens BEFORE the engine
// is called, so a non-authorized user never reaches it. CSRF is enforced by the
// requireCSRF wrapper.
//
// Engine error mapping:
//   - store.ErrNotFound -> 410 Gone (the version was pruned/gone — retention
//     evicted it, OR the seq does not belong to THIS sync; either way "this
//     snapshot is no longer available"). The seq is bound to the path syncID in
//     the store, so a cross-sync seq surfaces here as not-found, not someone
//     else's data.
//   - engine.ErrNoPath -> 409 Conflict (the captured member is no longer a
//     member of this sync, so there is nowhere to restore it).
//   - bad seq path value -> 400.
//   - anything else      -> 500.
func (s *Server) handleRestoreVersion(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.actioner == nil {
		s.logger.ErrorContext(r.Context(), "restore: no actioner wired")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	syncID := r.PathValue("id")

	// Authorization FIRST, before any work that could reach the engine. A
	// non-existent sync is treated as already-gone (404) rather than leaking
	// whether it exists.
	if _, err := s.store.GetSync(r.Context(), syncID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such sync", http.StatusNotFound)
			return
		}
		s.logger.ErrorContext(r.Context(), "restore: get sync failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	owns, err := s.userOwnsAnyMember(r.Context(), u, syncID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "restore: ownership check failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !owns {
		http.Error(w, "forbidden: you do not own a node in this sync", http.StatusForbidden)
		return
	}

	// Validate the seq only AFTER authorization, so a non-owner gets 403 regardless.
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq <= 0 {
		http.Error(w, "bad version id", http.StatusBadRequest)
		return
	}

	// Pass the PATH syncID (already authorized above) alongside seq: the engine
	// loads the version BOUND to this syncID, so a seq belonging to a different
	// sync is store-ErrNotFound (mapped to 410), not a cross-sync overwrite.
	err = s.actioner.RestoreVersion(r.Context(), syncID, seq)
	switch {
	case err == nil:
		s.refreshDashboard(w, r, u)
	case errors.Is(err, store.ErrNotFound):
		// The version was pruned (retention evicted it) or otherwise gone.
		http.Error(w, "that save version is no longer available", http.StatusGone)
	case errors.Is(err, engine.ErrNoPath):
		// The captured member is no longer in the sync.
		http.Error(w, "that save's node is no longer a member of this sync", http.StatusConflict)
	default:
		s.logger.ErrorContext(r.Context(), "restore failed", "sync", syncID, "seq", seq, "err", err.Error())
		http.Error(w, "could not restore that version", http.StatusInternalServerError)
	}
}
