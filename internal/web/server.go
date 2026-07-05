// Package web is RetroSync's HTTP surface: session auth and a dashboard, served
// with the Go stdlib net/http and html/template. No web framework. Templates and
// static assets (CSS, a vendored htmx.min.js) are embedded via embed.FS so the
// binary is self-contained and runs offline — no runtime CDN (docs/ui.md
// self-hosted ethos).
//
// Under the auto-mirror model there are NO play actions: no "Play on <node>",
// no "Done playing", no activation modal, no take-over. Every sync mirrors
// automatically; the dashboard "My syncs" section is a STATUS view (each sync's
// members, their last-known mtime, and whether the sync is in sync or in
// conflict). The ONLY human action is resolving a conflict.
//
// Conflict RESOLUTION: the dashboard conflict banner, the conflict modal
// (GET /syncs/{id}/conflict) that surfaces every member's live state, and the
// owner/admin-gated, CSRF-protected POST /api/syncs/{id}/resolve-conflict that
// drives the engine's ResolveConflict. Authority is owning one of the sync's
// member nodes (or admin). The /syncs registry editing UI manages syncs (each
// carrying a free-text game label) and each sync's members (node + path)
// directly — the sync is the atomic entity; there is no games table.
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

// Actioner is the narrow surface the web layer drives for the action endpoints
// (conflict resolution + the registry's smoke-test / browse). It is deliberately
// small and reach-free: the web package must NOT import internal/reach or any
// persistence driver — it depends only on this interface, which *engine.Engine
// satisfies. The web package MAY depend on internal/engine for the NodeState
// value type (engine is core play-sync logic, not infrastructure). main.go wires
// the real engine; tests pass a recording stub.
//
// There are no Activate/Deactivate methods: under auto-mirror there is no play
// session to start or stop. The only state-changing engine action the web layer
// drives is ResolveConflict.
type Actioner interface {
	// ResolveConflict makes winnerNodeID's current save the authority for a
	// conflicted sync, fanning it out to every other member (after backing each
	// loser up) and clearing the conflict flag. The single most destructive
	// action in the system: the POST handler gates it on owner-of-a-member-node-
	// or-admin AND CSRF before ever reaching here.
	ResolveConflict(ctx context.Context, syncID, winnerNodeID string) error
	// RestoreVersion makes a previously-captured save version (identified by its
	// monotonic seq, BOUND to syncID) the current authoritative content of its
	// (sync, node) member and propagates it to the rest of the sync (the recovery
	// net's undo). The POST handler gates it on owner-of-a-member-node-or-admin AND
	// CSRF before ever reaching here, and passes the PATH syncID it authorized so a
	// seq belonging to a DIFFERENT sync can never be restored (cross-sync restore
	// is store-enforced -> store.ErrNotFound). A pruned/gone seq also returns
	// store.ErrNotFound.
	RestoreVersion(ctx context.Context, syncID string, seq int64) error
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
	// yet"). Read-only: it never mutates the node, and the handler persists
	// nothing — the result is a transient reachable/error pill.
	SmokeTest(ctx context.Context, nodeID string) error
	// BrowseNode lists the directory entries directly under relPath on a node, for
	// the registry's save-file picker. relPath is node-relative ("" / "." = the
	// node's save root). It is the engine method that lets the web layer browse a
	// node's mounted save directory WITHOUT importing internal/reach (the engine
	// owns the resolver, the safepath containment, and the List). It returns
	// metadata only — names, types, sizes, mtimes — NEVER file contents. A
	// traversal/unsafe relPath surfaces the adapter's containment rejection
	// (safepath.ErrUnsafePath) in the error chain; an unsupported reach (ssh)
	// returns engine.ErrBrowseUnsupported; a missing node returns store.ErrNotFound.
	// Read-only.
	BrowseNode(ctx context.Context, nodeID, relPath string) ([]engine.DirEntry, error)
	// DiscoverGames scans every directory-listing-reachable node's save directory,
	// infers a game name per save-like file, drops files already claimed by a sync
	// member, and aggregates the survivors by inferred game name — the discovery
	// on-ramp (slice-22). It is READ-ONLY (no store/node writes) and resilient:
	// unsupported-reach (ssh) and per-node scan errors are skipped, not fatal, and
	// the per-node walk is bounded. The web layer drives it WITHOUT importing
	// internal/reach (the engine owns the resolver, the bounded walk, and the
	// containment).
	DiscoverGames(ctx context.Context) ([]engine.DiscoveredGame, error)
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
	// loginSem caps concurrent argon2id verifications (see handleLogin). Each
	// verify pins ~64 MiB, so an uncapped burst of POST /login requests —
	// including failures, which still burn a dummy verify — could pin N×64 MiB
	// and OOM a memory-limited pod. Buffered-channel semaphore, capacity
	// loginVerifyLimit.
	loginSem chan struct{}
}

// loginVerifyLimit is the maximum number of argon2id verifications running at
// once. 4 bounds the worst-case verify memory at ~256 MiB (4 × the 64 MiB
// argon2id memory cost) — safe in a small pod, and far more parallelism than a
// household's real login traffic ever needs.
const loginVerifyLimit = 4

// Options configures New. A nil Now or Logger gets a sane default.
type Options struct {
	// Now overrides the clock (sessions, "time ago"); defaults to time.Now.
	Now func() time.Time
	// Logger receives request/error logs; defaults to a discarding logger.
	Logger *slog.Logger
	// Actioner drives the engine-backed action endpoints (resolve-conflict and
	// smoke-test/status). May be nil when only the read-only surface is exercised;
	// the action handlers guard against a nil Actioner with a 500 rather than
	// panicking.
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
		loginSem:  make(chan struct{}, loginVerifyLimit),
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
//	GET  /api/syncs      — syncs JSON (auth required)
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
	mux.Handle("GET /api/syncs", s.requireAuth(http.HandlerFunc(s.handleAPISyncs)))
	mux.Handle("GET /api/nodes", s.requireAuth(http.HandlerFunc(s.handleAPINodes)))

	// Conflict resolution, keyed by SYNC id — the only play-side action under
	// auto-mirror. The modal fragment is a read-only GET (viewable by any
	// authenticated user, consistent with "can see others' syncs" — docs/ui.md).
	// The resolve POST is the single most destructive action in the system: it is
	// wrapped in requireCSRF AND re-checks owner-of-a-member-node-or-admin inside
	// the handler before touching the engine.
	mux.Handle("GET /syncs/{id}/conflict", s.requireAuth(http.HandlerFunc(s.handleConflictModal)))
	mux.Handle("POST /api/syncs/{id}/resolve-conflict", s.requireAuth(s.requireCSRF(http.HandlerFunc(s.handleResolveConflict))))

	// Save history + restore (slice-20: the recovery net). The history page lists
	// the server-side captured save versions for a sync's members (read-only,
	// viewable by any authenticated user, consistent with the conflict modal). The
	// restore POST makes a chosen version authoritative again and propagates it —
	// destructive, so it re-checks owner-of-a-member-node-or-admin AND CSRF, the
	// same authority rule as resolve-conflict.
	mux.Handle("GET /syncs/{id}/history", s.requireAuth(http.HandlerFunc(s.handleHistoryPage)))
	mux.Handle("POST /api/syncs/{id}/versions/{seq}/restore", s.requireAuth(s.requireCSRF(http.HandlerFunc(s.handleRestoreVersion))))

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
	// Save-file picker (slice-17): an admin-only, read-only GET that lists a
	// node's mounted save directory so the operator can click the real file in
	// the "Add member" form instead of typing the path. It exposes a node's
	// directory listing (metadata only — names/sizes/mtimes, NEVER contents), so
	// it is gated behind requireAdmin like the rest of the registry. No CSRF: it
	// is a safe GET (read-only) and admin-gated.
	mux.Handle("GET /api/nodes/{id}/browse", s.requireAuth(s.requireAdmin(http.HandlerFunc(s.handleBrowseNode))))

	// Syncs registry (slice-21). Admin-only per docs/auth.md, same wrapping as
	// /nodes: the page and every mutation behind requireAuth+requireAdmin
	// (mutations also behind requireCSRF), so a non-admin never reaches the Store.
	// The *sync* is the atomic entity (there is no games table); "game" is just a
	// free-text label on the sync, grouped for display. A sync's playable scope is
	// its MEMBERS (node + path), all managed via POST (not PUT/DELETE) to stay
	// consistent with the existing form/HTMX style.
	mux.Handle("GET /syncs", s.requireAuth(s.requireAdmin(http.HandlerFunc(s.handleSyncsPage))))

	// Sync registry mutations. Same admin + CSRF wrapping. Note these are distinct
	// from the engine-side /api/syncs/{id}/resolve-conflict route above: that
	// drives the auto-mirror engine and is owner/admin-gated; these edit the
	// registry and are admin-only.
	mux.Handle("POST /api/syncs", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleCreateSync)))))
	mux.Handle("POST /api/syncs/{id}", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleRenameSync)))))
	mux.Handle("POST /api/syncs/{id}/delete", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleDeleteSync)))))
	mux.Handle("POST /api/syncs/{id}/members/{node_id}", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleSetSyncMember)))))
	mux.Handle("POST /api/syncs/{id}/members/{node_id}/delete", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleDeleteSyncMember)))))

	// Discovery (slice-22): the on-ramp that replaces manual sync-building. The GET
	// page scans every reachable node's save dir, infers a game name per save file,
	// and offers candidates; the POST one-click-creates a sync from the selected
	// candidates. Admin-only (same wrapping as the rest of the registry); the POST
	// is also CSRF-protected. The scan itself is READ-ONLY — nothing is written
	// until the explicit create.
	mux.Handle("GET /discover", s.requireAuth(s.requireAdmin(http.HandlerFunc(s.handleDiscoverPage))))
	mux.Handle("POST /api/discover/create-sync", s.requireAuth(s.requireAdmin(s.requireCSRF(http.HandlerFunc(s.handleDiscoverCreateSync)))))

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
