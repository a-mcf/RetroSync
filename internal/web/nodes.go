package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"sort"
	"strings"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/store"
)

// templateFuncs are the html/template helpers the templates reference. Kept
// minimal: nodeRowCtx bundles one node row with the page-level enum/owner option
// sets + CSRF so the "node-row" template can render an edit form with correct
// pre-selected <option>s without each row re-deriving them.
var templateFuncs = template.FuncMap{
	"nodeRowCtx": nodeRowCtx,
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
// Secrets discipline (docs/auth.md): the reach_config form for an ssh node
// collects a secret_ref — a NAME/key into the host's secret store — never a
// cleartext password or key. No handler here reads, stores, displays, or logs an
// actual secret. reach_config holds only non-secret connection info + secret_ref.
//
// Reach boundary: smoke-test goes through the Actioner.SmokeTest engine method,
// NOT a direct internal/reach call, so this package imports NO internal/reach,
// pgx, or store-postgres. The ssh "not supported yet" case is matched on the
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
	Reachable   bool
	LastSeen    string
	// reach_config fields for pre-filling the edit form (non-secret only).
	Path      string
	Host      string
	SSHUser   string
	SecretRef string
}

var nodeKindOptions = []string{
	string(store.KindDeck), string(store.KindMister),
	string(store.KindAnbernic), string(store.KindGeneric),
}

var nodeReachOptions = []string{
	string(store.ReachSyncthingShare), string(store.ReachSSH),
}

// --- GET /nodes ----------------------------------------------------------

// handleNodesPage renders the admin node registry: a list of every node (with
// reachability from last_seen_at) plus an add-node form and per-node edit/delete
// /test controls. Admin-gating is enforced by the requireAdmin wrapper.
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
	now := s.now()
	for _, n := range nodes {
		row := nodeAdminRow{
			ID:        n.ID,
			Display:   n.Display,
			Kind:      string(n.Kind),
			Reach:     string(n.Reach),
			Reachable: n.LastSeenAt != nil,
			LastSeen:  fmtTimeAgo(n.LastSeenAt, now),
			// reach_config: only non-secret fields. There is no secret to copy.
			Path:      n.ReachConfig.Path,
			Host:      n.ReachConfig.Host,
			SSHUser:   n.ReachConfig.User,
			SecretRef: n.ReachConfig.SecretRef,
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
// (optional), display, kind, reach, plus reach_config fields (path for
// syncthing-share; host/user/secret_ref for ssh). On success it returns the
// refreshed node-list fragment. Error mapping:
//   - duplicate id (ErrConflict)         -> 409
//   - bad kind/reach (ErrInvalidValue)   -> 422
//   - missing owner FK (ErrInvalidReference) -> 422
//   - bad reach_config shape (local)     -> 400
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

// handleDeleteNode handles POST /api/nodes/{id}/delete. A delete blocked by a
// foreign key (the node is an active binding's primary, or has game_paths) comes
// back from the Store as ErrInvalidReference; we surface a friendly "node is in
// use" message with 409 rather than a 500.
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
		// The node is referenced by an active binding (primary) or a game_path.
		// Friendly, non-500 message so the admin knows to stop the session first.
		http.Error(w, "node is in use by an active session or game mapping — stop it first", http.StatusConflict)
	default:
		s.logger.ErrorContext(r.Context(), "delete node failed", "node", id, "err", err.Error())
		http.Error(w, "could not delete node", http.StatusInternalServerError)
	}
}

// --- POST /api/nodes/{id}/smoke-test -------------------------------------

// handleSmokeTest handles POST /api/nodes/{id}/smoke-test: probe reachability via
// the engine (Actioner.SmokeTest), so the web layer never touches internal/reach
// or a driver. Returns an HTML result fragment:
//   - nil           -> "reachable"; also bumps last_seen_at via the Store.
//   - ErrSmokeTestUnsupported (ssh) -> "smoke-test not supported yet (ssh adapter pending)".
//   - any other error -> the error, surfaced to the admin.
func (s *Server) handleSmokeTest(w http.ResponseWriter, r *http.Request) {
	if s.actioner == nil {
		s.logger.ErrorContext(r.Context(), "smoke-test: no actioner wired")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	id := r.PathValue("id")

	// Confirm the node exists first so a typo'd id is a clean 404 rather than a
	// generic engine error.
	if _, err := s.store.GetNode(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such node", http.StatusNotFound)
			return
		}
		s.logger.ErrorContext(r.Context(), "smoke-test: get node failed", "node", id, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	result := smokeResult{NodeID: id}
	err := s.actioner.SmokeTest(r.Context(), id)
	switch {
	case err == nil:
		result.OK = true
		result.Message = "reachable"
		// Optionally record the successful probe (docs/auth.md pairing flow: a
		// passing smoke test makes the node "available"). A failure to bump
		// last_seen_at is non-fatal to the report.
		if uerr := s.touchLastSeen(r.Context(), id); uerr != nil {
			s.logger.ErrorContext(r.Context(), "smoke-test: last_seen update failed", "node", id, "err", uerr.Error())
		}
	case errors.Is(err, engine.ErrSmokeTestUnsupported):
		result.Message = "smoke-test not supported yet (ssh adapter pending)"
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

// touchLastSeen updates a node's last_seen_at to now via UpdateNode. It re-reads
// the node so it rewrites the existing row faithfully (UpdateNode rewrites all
// mutable fields).
func (s *Server) touchLastSeen(ctx context.Context, id string) error {
	n, err := s.store.GetNode(ctx, id)
	if err != nil {
		return err
	}
	now := s.now()
	n.LastSeenAt = &now
	return s.store.UpdateNode(ctx, n)
}

// --- shared form parsing / error mapping ---------------------------------

// parseNodeForm reads and validates the node create/edit form. idOverride, when
// non-empty (edit), is the authoritative node id (the path id); for create it is
// "" and the id comes from the form. It returns the assembled Node and a non-empty
// human message string on a validation failure (the caller 400s with it).
//
// reach_config shape is validated here per reach:
//   - syncthing-share: requires a non-empty path.
//   - ssh: requires host, user, and secret_ref (secret_ref is a NAME into the
//     host secret store, NEVER a cleartext credential — see docs/auth.md).
//
// No password/secret field is ever read. reach_config carries only non-secret
// reach info + secret_ref.
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
		return store.Node{}, "reach must be one of syncthing-share, ssh"
	}

	n := store.Node{ID: id, Display: display, Kind: kind, Reach: rch}

	owner := strings.TrimSpace(r.PostFormValue("owner_user_id"))
	if owner != "" {
		n.OwnerUserID = &owner
	}

	// reach_config shape per reach. Only the fields valid for the chosen reach are
	// kept; the others are left zero so a stale path/host can't linger.
	switch rch {
	case store.ReachSyncthingShare:
		path := strings.TrimSpace(r.PostFormValue("path"))
		if path == "" {
			return store.Node{}, "syncthing-share requires a non-empty path"
		}
		n.ReachConfig = store.ReachConfig{Path: path}
	case store.ReachSSH:
		host := strings.TrimSpace(r.PostFormValue("host"))
		user := strings.TrimSpace(r.PostFormValue("user"))
		secretRef := strings.TrimSpace(r.PostFormValue("secret_ref"))
		if host == "" || user == "" || secretRef == "" {
			return store.Node{}, "ssh requires host, user, and secret_ref"
		}
		n.ReachConfig = store.ReachConfig{Host: host, User: user, SecretRef: secretRef}
	}
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

// compile-time assertion: *engine.Engine satisfies the smoke-test surface used
// here. (The full Actioner assertion lives wherever the engine is wired; this
// documents the SmokeTest dependency locally.)
var _ interface {
	SmokeTest(context.Context, string) error
} = (*engine.Engine)(nil)
