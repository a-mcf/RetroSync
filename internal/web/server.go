// Package web is RetroSync's HTTP surface: session auth and a read-only
// dashboard, served with the Go stdlib net/http and html/template. No web
// framework. Templates and static assets (CSS, a vendored htmx.min.js) are
// embedded via embed.FS so the binary is self-contained and runs offline — no
// runtime CDN (docs/ui.md self-hosted ethos).
//
// This slice (slice-6) is READ-ONLY: the dashboard displays state but wires no
// mutations. Action endpoints (activate/deactivate/resolve-conflict) and the
// /games and /nodes registry editing UI land in later slices; their hook
// points are marked with TODO(slice-actions) / TODO(slice-registry).
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

// Server holds the web service's dependencies. Construct with New; build the
// router with Handler.
type Server struct {
	store     store.Store
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

	// TODO(slice-actions): POST /api/games/{id}/activate, .../deactivate,
	// .../resolve-conflict and their HTML modals.
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
