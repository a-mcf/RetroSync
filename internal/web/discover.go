package web

// The /discover view (slice-22): the on-ramp that replaces typing a game label
// and a save-file path by hand. It scans every reachable node's save directory
// (via the engine's read-only DiscoverGames), groups the found save files by an
// inferred game name, shows the candidate nodes + paths, and lets the admin
// one-click create a sync from the selected candidates.
//
// Admin-only + CSRF, the same discipline as the /syncs registry: the page and the
// create POST are mounted behind requireAuth+requireAdmin (the POST also behind
// requireCSRF) in Server.Handler, so a non-admin never reaches the engine or the
// store here. The scan is READ-ONLY; the only write is the explicit create.
//
// The web layer drives the scan through the Actioner.DiscoverGames engine method,
// so it imports NO internal/reach or persistence driver — the engine owns the
// resolver, the bounded walk, and the containment (mirrors the smoke-test/browse
// boundary).

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/a-mcf/retrosync/internal/store"
)

// --- view-models ---------------------------------------------------------

// discoverPageData drives GET /discover.
type discoverPageData struct {
	User userView
	// Games are the discovered game groups, each with its candidate nodes/files.
	Games []discoverGameView
	CSRF  string
}

// discoverGameView is one inferred game and its candidate save files across
// nodes, plus a default sync id (a slug of the inferred name) the create form
// pre-fills.
type discoverGameView struct {
	// Name is the inferred game label (prefilled into the create form's game field).
	Name string
	// DefaultSyncID is a slug derived from Name, offered as the new sync's id.
	DefaultSyncID string
	Candidates    []discoverCandidateView
}

// discoverCandidateView is one (node, path) candidate with display metadata.
type discoverCandidateView struct {
	NodeID string
	Path   string
	Size   int64
	Mtime  string
	// Value is the encoded "<node_id>\x1f<path>" the create form submits per checked
	// candidate, so node id and path travel together unambiguously (a node id is a
	// slug and a path may contain anything but the unit-separator, which a real save
	// path never does).
	Value string
}

// candidateSep is the unit-separator byte joining node id + path in a candidate's
// form value. A node id is a slug ([a-z0-9-]) and a save path never contains a
// 0x1f control byte, so this split is unambiguous.
const candidateSep = "\x1f"

// --- GET /discover -------------------------------------------------------

// handleDiscoverPage renders the discovery view: it runs the read-only scan and
// groups the found save files by inferred game name, each with its candidate
// nodes/files and a one-click "Create sync" form. Admin-gating is enforced by the
// requireAdmin wrapper.
func (s *Server) handleDiscoverPage(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if s.actioner == nil {
		s.logger.ErrorContext(r.Context(), "discover: no actioner wired")
		http.Error(w, "discovery unavailable", http.StatusInternalServerError)
		return
	}

	games, err := s.actioner.DiscoverGames(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "discover scan failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := discoverPageData{
		User: userView{ID: u.ID, Display: u.Display, Role: u.Role},
		CSRF: s.csrfFor(r),
	}
	now := s.now()
	for _, g := range games {
		gv := discoverGameView{
			Name:          g.Name,
			DefaultSyncID: slugify(g.Name),
		}
		for _, c := range g.Candidates {
			gv.Candidates = append(gv.Candidates, discoverCandidateView{
				NodeID: c.NodeID,
				Path:   c.Path,
				Size:   c.Size,
				Mtime:  fmtTimeAgo(&c.Mtime, now),
				Value:  c.NodeID + candidateSep + c.Path,
			})
		}
		data.Games = append(data.Games, gv)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "discover", data); err != nil {
		s.logger.ErrorContext(r.Context(), "discover render failed", "err", err.Error())
	}
}

// --- POST /api/discover/create-sync --------------------------------------

// handleDiscoverCreateSync handles the one-click create. Form fields:
//   - game:       the (free-text) game label for the new sync (required).
//   - name:       the new sync's name (defaults to "Main" if blank).
//   - id:         optional sync id (slug); auto-generated from game+name when blank.
//   - candidate:  repeated; each is "<node_id>\x1f<path>" — the selected candidates.
//
// It creates the sync (CreateSync with the label) then adds each selected
// candidate as a member (SetSyncMember). Error mapping:
//   - no candidates selected / bad fields / bad derived slug -> 400
//   - duplicate sync id (CreateSync ErrConflict)             -> 409
//   - a candidate (node,path) already in ANOTHER sync        -> 409 (UNIQUE(node,path))
//   - a candidate naming a missing node/sync (FK)            -> 422
//
// On success it redirects to /syncs (HX-Redirect for an HTMX submit, a 303
// otherwise) so the new sync shows in the registry.
func (s *Server) handleDiscoverCreateSync(w http.ResponseWriter, r *http.Request) {
	// No auth check here: this route is mounted behind requireAuth+requireAdmin
	// (and requireCSRF) in Server.Handler, so an unauthenticated/non-admin request
	// never reaches it — a guard here would be dead code.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	game := strings.TrimSpace(r.PostFormValue("game"))
	if game == "" {
		http.Error(w, "a game label is required", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		// Sensible default so the admin can one-click without naming the sync.
		name = "Main"
	}

	candidates, verr := parseCandidates(r.PostForm["candidate"])
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}

	id := strings.TrimSpace(r.PostFormValue("id"))
	if id == "" {
		id = slugify(game + "-" + name)
	}
	if !validSlug(id) {
		http.Error(w, "id must be a slug: lowercase letters, digits, and hyphens", http.StatusBadRequest)
		return
	}

	// TODO(store-tx): make create+members transactional once the Store grows a tx
	// boundary (atomic create+SetSyncMember, or best-effort cleanup-on-failure).
	// Today a mid-create member failure leaves an admin-visible empty/partial sync
	// (acceptable — the admin can see/fix it in the registry — just flagged).
	//
	// 1) Create the sync with the free-text label.
	err := s.store.CreateSync(r.Context(), store.Sync{ID: id, Game: game, Name: name})
	switch {
	case err == nil:
		// proceed to add members
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "a sync with that id already exists", http.StatusConflict)
		return
	default:
		s.logger.ErrorContext(r.Context(), "discover create sync failed", "sync", id, "err", err.Error())
		http.Error(w, "could not create sync", http.StatusInternalServerError)
		return
	}

	// 2) Add each selected candidate as a member. A (node,path) already claimed by
	// another sync (UNIQUE(node,path)) -> 409; a missing node/sync -> 422. On any
	// member failure we surface the error; the half-built sync is left in place so
	// the admin can see/fix it in the registry (a member that errored was simply
	// not added).
	for _, c := range candidates {
		merr := s.store.SetSyncMember(r.Context(), store.SyncMember{SyncID: id, NodeID: c.nodeID, Path: c.path})
		switch {
		case merr == nil:
			continue
		case errors.Is(merr, store.ErrConflict):
			http.Error(w, "one of those save files is already in another sync", http.StatusConflict)
			return
		case errors.Is(merr, store.ErrInvalidReference):
			http.Error(w, "a selected candidate names a node that no longer exists", http.StatusUnprocessableEntity)
			return
		default:
			s.logger.ErrorContext(r.Context(), "discover add member failed", "sync", id, "node", c.nodeID, "err", merr.Error())
			http.Error(w, "could not add a candidate to the sync", http.StatusInternalServerError)
			return
		}
	}

	// Success: send the admin to /syncs so the new sync shows. HTMX honors
	// HX-Redirect; a plain form submit follows the 303.
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/syncs")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/syncs", http.StatusSeeOther)
}

// candidate is a parsed (node, path) selection from the create form.
type candidate struct {
	nodeID string
	path   string
}

// parseCandidates decodes the repeated "candidate" form values (each
// "<node_id>\x1f<path>") into (node, path) pairs. It returns a non-empty human
// message on a validation failure: an empty selection (the admin checked nothing)
// or a malformed value. Duplicate (node,path) selections are de-duplicated so a
// double-checked candidate doesn't cause a self-collision. The result is sorted
// (node, path) for a deterministic add order.
func parseCandidates(raw []string) ([]candidate, string) {
	seen := make(map[candidate]bool)
	var out []candidate
	for _, v := range raw {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		nodeID, path, ok := strings.Cut(v, candidateSep)
		if !ok {
			return nil, "a selected candidate is malformed"
		}
		nodeID = strings.TrimSpace(nodeID)
		path = strings.TrimSpace(path)
		if nodeID == "" || path == "" {
			return nil, "a selected candidate is malformed"
		}
		// Candidate paths arrive from the form, so they get the same lexical
		// member-path gate as the /syncs member routes (see validMemberPath): a
		// persisted absolute/".." path would silently halt polling for the sync.
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
	if len(out) == 0 {
		return nil, "select at least one save file to create a sync"
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].nodeID != out[j].nodeID {
			return out[i].nodeID < out[j].nodeID
		}
		return out[i].path < out[j].path
	})
	return out, ""
}

// TODO(slice-fresh-node): adding a node that does NOT yet hold the save (a fresh
// target device that should RECEIVE the game on first sync) stays the existing
// /syncs member-add via the picker — discovery groups EXISTING save files only.
// A future slice can offer "also add this game to <fresh node>" here.
