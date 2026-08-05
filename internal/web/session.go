package web

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

// sessionCookieName is the cookie that carries the opaque session token.
const sessionCookieName = "retrosync_session"

// sessionTTL bounds how long a session is valid. A household tool doesn't need
// aggressive expiry; a week keeps a Deck logged in across a play week.
const sessionTTL = 7 * 24 * time.Hour

// session is one logged-in session: which user it authenticates, its
// per-session CSRF token, and when it expires.
type session struct {
	userID  string
	csrf    string
	expires time.Time
}

// sessionManager is an in-memory, token-keyed session store.
//
// NOTE: sessions are kept only in process memory; they do NOT survive a
// restart of the retrosync process. Everyone is logged out on deploy. For a
// single-household self-hosted tool that is an acceptable tradeoff (and avoids
// a server-side session table this slice). A persistent store can replace this
// behind the same interface later.
type sessionManager struct {
	mu       sync.Mutex
	sessions map[string]session
	now      func() time.Time
}

func newSessionManager(now func() time.Time) *sessionManager {
	if now == nil {
		now = time.Now
	}
	return &sessionManager{
		sessions: make(map[string]session),
		now:      now,
	}
}

// newToken returns a 256-bit cryptographically random, URL-safe token. 32
// bytes from crypto/rand is well beyond brute-force reach.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// create mints a new session for userID and returns its token. A fresh session
// token AND a fresh per-session CSRF token are generated on every call, so
// logging in always rotates both identifiers (defeating session fixation, and
// scoping the CSRF token to the live session).
func (m *sessionManager) create(userID string) (string, error) {
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	csrf, err := newToken()
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[tok] = session{userID: userID, csrf: csrf, expires: m.now().Add(sessionTTL)}
	return tok, nil
}

// lookup returns the userID for a valid, unexpired token. ok is false for an
// unknown or expired token; an expired token is also evicted.
func (m *sessionManager) lookup(tok string) (userID string, ok bool) {
	s, found, valid := m.get(tok)
	if !valid {
		return "", false
	}
	return s.userID, found
}

// csrfToken returns the per-session CSRF token for a valid, unexpired session
// token. ok is false for an unknown or expired token.
func (m *sessionManager) csrfToken(tok string) (token string, ok bool) {
	s, found, _ := m.get(tok)
	if !found {
		return "", false
	}
	return s.csrf, true
}

// get returns the session for tok. found is false for an unknown/expired token
// (an expired token is evicted). valid mirrors found and is kept for readability
// at call sites that only care about validity.
func (m *sessionManager) get(tok string) (s session, found, valid bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[tok]
	if !ok {
		return session{}, false, false
	}
	if !m.now().Before(s.expires) {
		delete(m.sessions, tok)
		return session{}, false, false
	}
	return s, true, true
}

// destroy removes a session token if present (idempotent).
func (m *sessionManager) destroy(tok string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, tok)
}

// destroyUser revokes EVERY session belonging to userID and reports how many it
// removed. It is the revocation primitive behind the user-management guardrail
// "invalidate sessions on password reset, role change, and delete": because
// sessions are server-side (a token-keyed map), a reset/demote/delete can take
// effect immediately instead of waiting out the 7-day cookie.
//
// Caveat, deliberately not solved this slice: the map is per-PROCESS. A restart
// clears it (everyone is logged out — fine), but with more than one replica a
// revocation only reaches sessions on the replica that served the request.
// RetroSync runs single-replica today; a shared/persistent session store is the
// follow-up if that ever changes.
func (m *sessionManager) destroyUser(userID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for tok, s := range m.sessions {
		if s.userID == userID {
			delete(m.sessions, tok)
			n++
		}
	}
	return n
}

// setCookie writes the session cookie with the security flags required by
// docs/auth.md: HttpOnly (no JS access), Secure (HTTPS only), SameSite=Lax
// (sent on top-level navigations — needed so the post-login redirect carries
// the cookie — but not on cross-site sub-requests, a baseline CSRF mitigation).
func (m *sessionManager) setCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  m.now().Add(sessionTTL),
		MaxAge:   int(sessionTTL / time.Second),
	})
}

// clearCookie expires the session cookie in the client.
func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
