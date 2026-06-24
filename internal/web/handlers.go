package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/a-mcf/retrosync/internal/auth"
	"github.com/a-mcf/retrosync/internal/store"
)

// handleHealthz is an unauthenticated liveness probe.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// requireAuth wraps a handler so only an authenticated request reaches it. On
// success the authed user is injected into the request context. On failure:
// HTML routes redirect to /login (303); /api/* routes get a 401 JSON error.
//
// Note: requireAuth itself does not check CSRF. GET routes behind it are safe
// (read-only). The mutating action POSTs (e.g. /api/syncs/{id}/resolve-conflict)
// are additionally wrapped in requireCSRF (synchronizer-token check). The
// /login and /logout POSTs are mounted OUTSIDE this middleware and rely on
// SameSite=Lax for baseline CSRF protection.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.currentUser(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSONError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireAdmin wraps a handler so only an admin user reaches it (docs/auth.md:
// "edit the registry ... add nodes" is admin-only). It MUST be mounted inside
// requireAuth — it reads the authenticated user from the request context. A
// non-admin is rejected BEFORE the wrapped handler runs, so a regular user never
// reaches the Store via any /nodes page or /api/nodes* mutation:
//
//   - /api/* routes get a 403 JSON error.
//   - HTML routes get a 403 "admins only" page.
//
// If no user is in context (requireAdmin somehow mounted without requireAuth) it
// fails closed with a 403 rather than panicking.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := userFromContext(r.Context())
		if !ok || u.Role != store.RoleAdmin {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSONError(w, http.StatusForbidden, "admins only")
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			if err := s.templates.ExecuteTemplate(w, "forbidden", nil); err != nil {
				s.logger.ErrorContext(r.Context(), "forbidden render failed", "err", err.Error())
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// currentUser resolves the session cookie to a User, or (zero, false) when
// there is no valid session or the user has since been deleted.
func (s *Server) currentUser(r *http.Request) (store.User, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return store.User{}, false
	}
	userID, ok := s.sessions.lookup(c.Value)
	if !ok {
		return store.User{}, false
	}
	u, err := s.store.GetUser(r.Context(), userID)
	if err != nil {
		// Session points at a user that no longer exists; treat as logged out.
		return store.User{}, false
	}
	return u, true
}

// handleLoginForm renders the login page. If already authenticated, send them
// to the dashboard.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUser(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderLogin(w, http.StatusOK, "")
}

// handleLogin authenticates username+password. On success it regenerates a
// fresh session token (no session fixation), sets the secure cookie, and
// redirects to the dashboard. On failure it re-renders the form with a generic
// error and NO cookie — username and password are not distinguished in the
// message, to avoid user enumeration.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, http.StatusBadRequest, "Bad request.")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")

	if !s.authenticate(r.Context(), username, password) {
		// Same message + status for unknown user and wrong password.
		s.renderLogin(w, http.StatusUnauthorized, "Invalid username or password.")
		return
	}

	token, err := s.sessions.create(username)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "login: mint session failed", "err", err.Error())
		s.renderLogin(w, http.StatusInternalServerError, "Could not start a session.")
		return
	}
	s.sessions.setCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// authenticate reports whether username+password are valid. It always runs a
// password verification (against a dummy hash for an unknown user) so the
// response time does not reveal whether the username exists.
func (s *Server) authenticate(ctx context.Context, username, password string) bool {
	u, err := s.store.GetUser(ctx, username)
	if err != nil {
		// Unknown user: still do a verify against a throwaway hash to keep timing
		// roughly constant, then fail. (s.dummyHash is a real argon2id hash minted
		// at construction, so this burns comparable CPU.)
		_, _ = auth.Verify(s.dummyHash, password)
		return false
	}
	ok, err := auth.Verify(u.PwHash, password)
	if err != nil {
		s.logger.ErrorContext(ctx, "login: stored hash invalid", "user", username, "err", err.Error())
		return false
	}
	return ok
}

// handleLogout destroys the current session and clears the cookie. Idempotent.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		s.sessions.destroy(c.Value)
	}
	clearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleDashboard renders the read-only dashboard for the authed user.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	data, err := s.buildDashboard(r.Context(), u)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "dashboard build failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data.CSRF = s.csrfFor(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "dashboard", data); err != nil {
		// Header may already be written; just log.
		s.logger.ErrorContext(r.Context(), "dashboard render failed", "err", err.Error())
	}
}

// renderLogin writes the login page with an optional error message.
func (s *Server) renderLogin(w http.ResponseWriter, status int, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.templates.ExecuteTemplate(w, "login", struct{ Error string }{Error: errMsg}); err != nil {
		s.logger.Error("login render failed", "err", err.Error())
	}
}

// --- JSON API ------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// handleAPIStatus implements GET /api/status (docs/api.md shape).
func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.buildStatus(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "api/status failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleAPISyncs implements GET /api/syncs (docs/api.md shape). Supports the
// documented q filter (a case-insensitive substring over the game label, sync
// name, and id).
func (s *Server) handleAPISyncs(w http.ResponseWriter, r *http.Request) {
	syncs, err := s.buildSyncs(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")))
	if err != nil {
		s.logger.ErrorContext(r.Context(), "api/syncs failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, syncs)
}

// handleAPINodes implements GET /api/nodes.
func (s *Server) handleAPINodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.buildNodes(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "api/nodes failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}
