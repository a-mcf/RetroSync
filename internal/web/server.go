// Package web is RetroSync's HTTP surface: session auth and a read-only
// dashboard, served with the Go stdlib net/http and html/template. No web
// framework. Templates and static assets (CSS, a vendored htmx.min.js) are
// embedded via embed.FS so the binary is self-contained and runs offline — no
// runtime CDN (docs/ui.md self-hosted ethos).
//
// The play-sync ACTIONS — the "Play on <node>" / "Done playing" buttons, the
// activation "use my save" modal, force-takeover, conflict resolution — are keyed
// by SYNC id (a sync is the unit of mirroring; its members are its scope). Every
// state-changing POST is CSRF-protected. The web layer drives play-sync only
// through the narrow Actioner interface (no internal/reach import).
//
// Conflict RESOLUTION: the dashboard conflict banner, the conflict modal
// (GET /syncs/{id}/conflict) that surfaces every member's live state, and the
// owner/admin-gated, CSRF-protected POST /api/syncs/{id}/resolve-conflict that
// drives the engine's ResolveConflict. The /games registry editing UI manages a
// game's syncs and each sync's members (node + path) directly (game_paths is
// retired).
package web

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/a-mcf/retrosync/internal/auth"
	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/store"
)

//go:embed templates/*.tmpl.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// ctxKey is the unexported type for request-context keys, so no other package
// can collide with ours.
type ctxKey int

const userCtxKey ctxKey = iota

// Actioner is the narrow play-sync surface the web layer drives for the action
// endpoints (activate / deactivate / resolve-conflict). It is deliberately small
// and reach-free: the web package must NOT import internal/reach or any
// persistence driver — it depends only on this interface, which *engine.Engine
// satisfies. The web package MAY depend on internal/engine for the NodeState
// value type (engine is core play-sync logic, not infrastructure). main.go wires
// the real engine; tests pass a recording stub.
type Actioner interface {
	Activate(ctx context.Context, syncID, primaryNode, direction string, force bool) error
	Deactivate(ctx context.Context, syncID string) error
	// ResolveConflict makes winnerNodeID's current save the authority for a
	// conflicted binding, fanning it out to every other in-scope peer (after
	// backing each loser up) and clearing the conflict flag. The single most
	// destructive action in the system: the POST handler gates it on
	// owner-or-admin AND CSRF before ever reaching here.
	ResolveConflict(ctx context.Context, syncID, winnerNodeID string) error
	// NodeStates returns the live per-node state (mtime, size, presence) of every
	// member of the sync, read-only, for the conflict modal to render so the human
	// can pick a winner with full disclosure.
	NodeStates(ctx context.Context, syncID string) ([]engine.NodeState, error)
	// SmokeTest verifies that a node is reachable for the /nodes registry "Test"
	// button and the pairing smoke-test flow (docs/auth.md). It is the engine
	// method that lets the web layer probe reachability WITHOUT importing
	// internal/reach or any persistence driver: the engine owns the resolver and
	// the Stat. A nil return means reachable; a non-nil error is surfaced to the
	// admin (for an ssh node it is reach.ErrUnsupportedReach — "not supported
	// yet"). Read-only: it never mutates the node (the handler updates
	// last_seen_at via the Store on success).
	SmokeTest(ctx context.Context, nodeID string) error
}

// Server holds the web service's dependencies. Construct with New; build the
// router with Handler.
type Server struct {
	store     store.Store
	actioner  Actioner
	sessions  *sessionManager
	templates *template.Template
	static    fs.FS
	logger    *slog.Logger
	now       func() time.Time
	// dummyHash is a valid argon2id hash minted at construction. Verifying a
	// failed login against it (for an unknown username) keeps login timing
	// roughly constant, mitigating username enumeration via response time.
	dummyHash string
}

// Options configures New. A nil Now or Logger gets a sane default.
type Options struct {
	// Now overrides the clock (sessions, "time ago"); defaults to time.Now.
	Now func() time.Time
	// Logger receives request/error logs; defaults to a discarding logger.
	Logger *slog.Logger
	// Actioner drives the play-sync action endpoints (activate/deactivate). May
	// be nil when only the read-only surface is exercised; the action handlers
	// guard against a nil Actioner with a 500 rather than panicking.
	Actioner Actioner
}

// New builds a Server backed by st. It parses the embedded templates eagerly so
// a template syntax error fails fast at construction (and in tests) rather than
// on first request.
func New(st store.Store, opts Options) (*Server, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(discardHandler{})
	}

	tmpl, err := template.New("retrosync").Funcs(templateFuncs).ParseFS(templateFS, "templates/*.tmpl.html")
	if err != nil {
		return nil, err
	}
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}

	dummy, err := auth.Hash("retrosync-dummy-timing-hash-not-a-real-password")
	if err != nil {
		return nil, err
	}

	return &Server{
		store:     st,
		actioner:  opts.Actioner,
		sessions:  newSessionManager(now),
		templates: tmpl,
		static:    staticSub,
		logger:    logger,
		now:       now,
		dummyHash: dummy,
	}, nil
}

// Handler builds the http.Handler with all routes for this slice, using Go
// 1.22 method+path patterns. Routes:
//
//	GET  /healthz        — liveness, no auth
//	GET  /login          — login form, no auth
//	POST /login          — authenticate, mint session
//	POST /logout         — destroy session
//	GET  /static/...     — embedded assets (css, htmx), no auth
//	GET  /               — dashboard (auth required)
//	GET  /api/status     — status JSON (auth required)
//	GET  /api/games      — games JSON (auth required)
//	GET  /api/nodes      — nodes JSON (auth required)
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// Embedded static assets. http.FileServerFS sets content types and handles
	// conditional requests; no auth so the login page can style itself.
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(s.static)))

	// Authenticated routes.
	mux.Handle("GET /{$}", s.requireAuth(http.HandlerFunc(s.handleDashboard)))
	mux.Handle("GET /api/status", s.requireAuth(http.HandlerFunc(s.handleAPIStatus)))
	mux.Handle("GET /api/games", s.requireAuth(http.HandlerFunc(s.handleAPIGames)))
	mux.Handle("GET /api/nodes", s.requireAuth(http.HandlerFunc(s.handleAPINodes)))

	// Play-sync action endpoints, keyed by SYNC id. The activation modal fragment
	// is a GET (no state change, no CSRF needed). The state-changing POSTs are
	// wrapped in requireCSRF *inside* requireAuth so an unauthenticated request
	// 303s to /login (friendly) while an authenticated-but-tokenless request 403s.
	mux.Handle("GET /syncs/{id}/activate", s.requireAuth(http.HandlerFunc(s.handleActivateModal)))
	mux.Handle("POST /api/syncs/{id}/activate", s.requireAuth(s.requireCSRF(http.HandlerFunc(s.handleActivate))))
	mux.Handle("POST /api/syncs/{id}/deactivate", s.requireAuth(s.requireCSRF(http.HandlerFunc(s.handleDeactivate))))

	// Conflict resolution, keyed by SYNC id. The modal fragment is a read-only GET
	// (viewable by any authenticated user, consistent with "can see others'
	// sessions" — docs/ui.md / brief D). The resolve POST is the single most
	// destructive action in the system: it is wrapped in requireCSRF AND re-checks
	// owner-or-admin inside the handler before touching the engine.
	mux.Handle("GET /syncs/{id}/conflict", s.requireAuth(http.HandlerFunc(s.handleConflictModal)))
	mux.Handle("POST /api/syncs/{id}/resolve-conflict", s.requireAuth(s.requireCSRF(http.HandlerFunc(s.handleResolveConflict))))

	// Nodes registry (slice-10). Admin-only per docs/auth.md: every page and every
	// mutation is wrapped in requireAdmin, so a non-admin never reaches the Store.
	// requireAdmin sits inside requireAuth (so an unauthenticated request 303s to
	// /login / 401s on /api) and, for mutations, alongside requireCSRF. Ordering of
	// the admin and CSRF wrappers does not affect safety — each independently
	// blocks the request — but admin is checked first so a non-admin gets a clean
	// 403 "admins only" rather than a CSRF error.
	mux.Handle("GET /nodes", s.requireAuth(s.requireAdmin(http.HandlerFunc(s.handleNodesPage))))
	mux.Handle("POST /api/nodes", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleCreateNode)))))
	mux.Handle("POST /api/nodes/{id}", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleEditNode)))))
	mux.Handle("POST /api/nodes/{id}/delete", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleDeleteNode)))))
	mux.Handle("POST /api/nodes/{id}/smoke-test", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleSmokeTest)))))

	// Games registry (slice-11 + slice-16). Admin-only per docs/auth.md, same
	// wrapping as /nodes: every page and mutation behind requireAuth+requireAdmin
	// (mutations also behind requireCSRF), so a non-admin never reaches the Store.
	// A game's playable scope is managed through its SYNCS and each sync's MEMBERS
	// (node + path), all via POST (not PUT/DELETE) to stay consistent with the
	// existing form/HTMX style.
	mux.Handle("GET /games", s.requireAuth(s.requireAdmin(http.HandlerFunc(s.handleGamesPage))))
	mux.Handle("POST /api/games", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleCreateGame)))))
	mux.Handle("POST /api/games/{id}", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleEditGame)))))
	mux.Handle("POST /api/games/{id}/delete", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleDeleteGame)))))

	// Sync registry mutations (slice-16). Same admin + CSRF wrapping. Note these
	// are distinct from the PLAY-side /api/syncs/{id}/activate|deactivate|
	// resolve-conflict routes above: those drive the engine and are owner/admin-
	// gated; these edit the registry and are admin-only.
	mux.Handle("POST /api/syncs", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleCreateSync)))))
	mux.Handle("POST /api/syncs/{id}", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleRenameSync)))))
	mux.Handle("POST /api/syncs/{id}/delete", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleDeleteSync)))))
	mux.Handle("POST /api/syncs/{id}/members/{node_id}", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleSetSyncMember)))))
	mux.Handle("POST /api/syncs/{id}/members/{node_id}/delete", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleDeleteSyncMember)))))

	return mux
}

// userFromContext returns the authenticated user injected by requireAuth. The
// second result is false on unauthenticated routes (should not happen for any
// handler mounted behind requireAuth).
func userFromContext(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userCtxKey).(store.User)
	return u, ok
}

// discardHandler drops every slog record (used when New is given no logger).
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }
