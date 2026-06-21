package web

import (
	"crypto/subtle"
	"net/http"
)

// csrfFieldName is the form field carrying the CSRF token on a POST.
const csrfFieldName = "csrf_token"

// csrfHeaderName is the header carrying the CSRF token on an HTMX request. The
// dashboard sets it via hx-headers so action POSTs don't need a hidden input in
// every fragment; either source (form field or header) is accepted.
const csrfHeaderName = "X-CSRF-Token"

// csrfFor returns the per-session CSRF token for the request's session cookie,
// or "" if there is no valid session. Templates embed this so every form/HTMX
// POST can echo it back.
func (s *Server) csrfFor(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return ""
	}
	tok, ok := s.sessions.csrfToken(c.Value)
	if !ok {
		return ""
	}
	return tok
}

// requireCSRF wraps a state-changing handler with synchronizer-token CSRF
// protection. It is the CSRF control (SameSite=Lax on the session cookie is only
// defense-in-depth, per docs and the slice-6 audit):
//
//   - The submitted token comes from the form field csrf_token or the
//     X-CSRF-Token header (HTMX).
//   - It is compared to the session's stored token with
//     subtle.ConstantTimeCompare (no early-exit timing leak).
//   - On absence or mismatch the request is rejected with 403 and the wrapped
//     handler never runs (so no state changes and the Actioner is never called).
//
// Requests with no valid session are also rejected 403 here; in practice these
// handlers are also mounted behind requireAuth, but requireCSRF must not depend
// on ordering for its safety property.
func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := s.csrfFor(r)
		if want == "" {
			http.Error(w, "forbidden: no session", http.StatusForbidden)
			return
		}
		got := r.Header.Get(csrfHeaderName)
		if got == "" {
			// ParseForm is safe to call here; downstream handlers call it again
			// (it is idempotent and caches r.PostForm).
			if err := r.ParseForm(); err == nil {
				got = r.PostFormValue(csrfFieldName)
			}
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "forbidden: bad CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
