// Package web is RetroSync's HTTP surface: session auth and a read-only
// dashboard, served with the Go stdlib net/http and html/template. No web
// framework. Templates and static assets (CSS, a vendored htmx.min.js) are
// embedded via embed.FS so the binary is self-contained and runs offline — no
// runtime CDN (docs/ui.md self-hosted ethos).
//
// Slice-7 adds the play-sync ACTIONS: the "Play on <node>" / "Done playing"
// buttons, the activation "use my save" modal, force-takeover, and per-session
// CSRF protection on every state-changing POST. The web layer drives play-sync
// only through the narrow Actioner interface (no internal/reach import).
//
// Slice-9 adds conflict RESOLUTION: the dashboard conflict banner, the conflict
// modal (GET /games/{id}/conflict) that surfaces every node's live state, and
// the owner/admin-gated, CSRF-protected POST /api/games/{id}/resolve-conflict
// that drives the engine's ResolveConflict. The /games /nodes registry editing
// UI remains out of scope; its hook point is marked TODO(slice-registry).
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
	Activate(ctx context.Context, gameID, primaryNode, direction, peerScope string, force bool) error
	Deactivate(ctx context.Context, gameID string) error
	// ResolveConflict makes winnerNodeID's current save the authority for a
	// conflicted binding, fanning it out to every other in-scope peer (after
	// backing each loser up) and clearing the conflict flag. The single most
	// destructive action in the system: the POST handler gates it on
	// owner-or-admin AND CSRF before ever reaching here.
	ResolveConflict(ctx context.Context, gameID, winnerNodeID string) error
	// NodeStates returns the live per-node state (mtime, size, presence) of every
	// in-scope node, read-only, for the conflict modal to render so the human can
	// pick a winner with full disclosure.
	NodeStates(ctx context.Context, gameID string) ([]engine.NodeState, error)
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

	tmpl, err := template.ParseFS(templateFS, "templates/*.tmpl.html")
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

	// Action endpoints (slice-7). The activation modal fragment is a GET (no
	// state change, no CSRF needed). The state-changing POSTs are wrapped in
	// requireCSRF *inside* requireAuth so an unauthenticated request 303s to
	// /login (friendly) while an authenticated-but-tokenless request 403s.
	mux.Handle("GET /games/{id}/activate", s.requireAuth(http.HandlerFunc(s.handleActivateModal)))
	mux.Handle("POST /api/games/{id}/activate", s.requireAuth(s.requireCSRF(http.HandlerFunc(s.handleActivate))))
	mux.Handle("POST /api/games/{id}/deactivate", s.requireAuth(s.requireCSRF(http.HandlerFunc(s.handleDeactivate))))

	// Conflict resolution (slice-9). The modal fragment is a read-only GET
	// (viewable by any authenticated user, consistent with "can see others'
	// sessions" — docs/ui.md / brief D). The resolve POST is the single most
	// destructive action in the system: it is wrapped in requireCSRF AND re-checks
	// owner-or-admin inside the handler before touching the engine.
	mux.Handle("GET /games/{id}/conflict", s.requireAuth(http.HandlerFunc(s.handleConflictModal)))
	mux.Handle("POST /api/games/{id}/resolve-conflict", s.requireAuth(s.requireCSRF(http.HandlerFunc(s.handleResolveConflict))))

	// TODO(slice-registry): GET/POST /games, /nodes registry editing + node
	// smoke-test.

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
