package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/a-mcf/retrosync/internal/auth"
	"github.com/a-mcf/retrosync/internal/store"
)

// The /users people registry (slice-36) plus the /settings page.
//
// Until this slice a user could only be created with `retrosync user set` on the
// CLI — unusable in a cluster, where that means a one-off pod just to add a
// person. /users is the admin surface (list, add, edit display+role, reset
// password, delete); /settings carries the ONE thing a non-admin can do — change
// their own password, which requires their current one — and links an admin
// through to /users, which used to sit in the top navigation.
//
// Everything under /users is mounted behind requireAuth + requireAdmin (and the
// mutations behind requireCSRF) in Server.Handler, exactly like /nodes, so a
// non-admin never reaches the Store here. /settings is behind requireAuth only.
//
// Secrets discipline: pw_hash NEVER leaves this package's Store calls. No view
// model here carries it, no template renders it, no log line mentions it. The
// only thing that ever touches a cleartext password is auth.Hash / auth.Verify.
//
// Hashing path: passwords are hashed with auth.Hash — the exact function the
// `user set` CLI uses — and validated with auth.ValidatePassword, the exact rule
// the CLI applies. There is deliberately no second hashing path or cost
// parameter in the web layer.

// --- view models ---------------------------------------------------------

// usersPageData drives GET /users (the full page) and the user-list fragment.
type usersPageData struct {
	User  userView
	Users []userAdminRow
	// Roles is the enum option set for the create/edit role <select>.
	Roles []string
	CSRF  string
	// AdminCount is how many admins exist right now. The list uses it to explain
	// the delete/demote refusal on the last admin BEFORE the server refuses it —
	// the refusal itself is enforced in the store, inside the write's
	// transaction; this is only signposting.
	AdminCount int
	// MinPasswordLen is auth.MinPasswordLen, rendered as the password fields'
	// minlength + hint so the form states the same rule the server enforces.
	MinPasswordLen int
}

// userRowContext is the per-row template context: one person plus the shared
// page-level role options, CSRF token, and password-length hint, so the
// "user-row" template can render its edit/reset forms without re-deriving them.
type userRowContext struct {
	User           userAdminRow
	Roles          []string
	CSRF           string
	MinPasswordLen int
}

// userRowCtx builds a userRowContext from the page data and a single row. It is
// registered as a template func so "users-list" can pass each row what its forms
// need (mirrors nodeRowCtx).
func userRowCtx(page usersPageData, row userAdminRow) userRowContext {
	return userRowContext{
		User:           row,
		Roles:          page.Roles,
		CSRF:           page.CSRF,
		MinPasswordLen: page.MinPasswordLen,
	}
}

// userAdminRow is one person in the admin list. It carries NO pw_hash — the
// hash never reaches a template.
type userAdminRow struct {
	ID      string
	Display string
	Role    string
	// Devices is how many nodes this user owns. It is both a useful fact and the
	// reason a delete may be refused (nodes.owner_user_id has no ON DELETE), so
	// the list links it to /nodes where the admin can reassign them.
	Devices int
	// IsSelf marks the signed-in admin's own row: self-delete is refused, and
	// their own password change belongs on /settings (it asks for the current
	// password), so the row points there instead of offering a reset.
	IsSelf bool
	// IsLastAdmin marks the sole remaining admin: delete and demote are refused
	// for them (store.ErrLastAdmin).
	IsLastAdmin bool
}

// settingsPageData drives GET /settings, the page every signed-in user gets:
// their own account, plus links to the admin-only configuration that used to
// crowd the top navigation.
type settingsPageData struct {
	User userView
	CSRF string
	// Changed is set after a successful password change (the post-change redirect
	// carries ?changed=1) so the page can confirm it.
	Changed bool
	// MinPasswordLen is rendered as the form's hint + minlength attribute; it is
	// auth.MinPasswordLen, the same number the server enforces.
	MinPasswordLen int
	// PeopleCount/AdminCount describe the People section for admins, so its link
	// says something about what is behind it instead of being a bare word. Zero
	// for a non-admin, who never sees that section.
	PeopleCount int
	AdminCount  int
}

var userRoleOptions = []string{string(store.RoleUser), string(store.RoleAdmin)}

// countAdmins reports how many of these users are admins. Both the People page
// and the settings summary need it, and the last-admin guard's behaviour is
// explained in terms of it, so it is one function rather than two loops.
func countAdmins(users []store.User) int {
	n := 0
	for _, u := range users {
		if u.Role == store.RoleAdmin {
			n++
		}
	}
	return n
}

// --- GET /users ----------------------------------------------------------

// handleUsersPage renders the people registry: every user with their role and
// device count, plus add/edit/reset/delete controls. Admin-gating is enforced by
// the requireAdmin wrapper.
func (s *Server) handleUsersPage(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	data, err := s.buildUsersPage(r.Context(), u)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "users page build failed", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data.CSRF = s.csrfFor(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "users", data); err != nil {
		s.logger.ErrorContext(r.Context(), "users page render failed", "err", err.Error())
	}
}

// buildUsersPage assembles the page data: every user (WITHOUT their pw_hash)
// joined with a count of the nodes they own.
func (s *Server) buildUsersPage(ctx context.Context, me store.User) (usersPageData, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return usersPageData{}, fmt.Errorf("list users: %w", err)
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return usersPageData{}, fmt.Errorf("list nodes: %w", err)
	}
	owned := make(map[string]int, len(users))
	for _, n := range nodes {
		if n.OwnerUserID != nil {
			owned[*n.OwnerUserID]++
		}
	}
	admins := countAdmins(users)

	data := usersPageData{
		User:           userView{ID: me.ID, Display: me.Display, Role: me.Role},
		Roles:          userRoleOptions,
		AdminCount:     admins,
		MinPasswordLen: auth.MinPasswordLen,
	}
	for _, u := range users {
		data.Users = append(data.Users, userAdminRow{
			ID:          u.ID,
			Display:     u.Display,
			Role:        string(u.Role),
			Devices:     owned[u.ID],
			IsSelf:      u.ID == me.ID,
			IsLastAdmin: u.Role == store.RoleAdmin && admins == 1,
		})
	}
	sort.Slice(data.Users, func(i, j int) bool { return data.Users[i].ID < data.Users[j].ID })
	return data, nil
}

// --- POST /api/users (create) --------------------------------------------

// handleCreateUser handles POST /api/users. Form fields: id, display (optional,
// defaults to the id like `user set` does), role, password. On success it
// returns the refreshed user-list fragment. Error mapping:
//   - bad id/role/password (local validation) -> 400
//   - duplicate id (ErrConflict)              -> 409
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	me, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("id"))
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if !validSlug(id) {
		http.Error(w, userIDError, http.StatusBadRequest)
		return
	}
	role, verr := parseRole(r.PostFormValue("role"))
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}
	display := strings.TrimSpace(r.PostFormValue("display"))
	if display == "" {
		display = id // same default as `retrosync user set`
	}

	hash, herr := s.hashPassword(r.Context(), r.PostFormValue("password"))
	if herr != nil {
		s.writePasswordErr(w, r, herr)
		return
	}

	err := s.store.CreateUser(r.Context(), store.User{
		ID: id, Display: display, PwHash: hash, Role: role,
	})
	switch {
	case err == nil:
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "someone with that username already exists", http.StatusConflict)
		return
	default:
		s.logger.ErrorContext(r.Context(), "create user failed", "user", id, "err", err.Error())
		http.Error(w, "could not add that person", http.StatusInternalServerError)
		return
	}
	s.refreshUsersList(w, r, me)
}

// --- POST /api/users/{id} (edit display + role) --------------------------

// handleEditUser handles POST /api/users/{id}: rewrite display and role. The
// path id is authoritative. The password is NOT touched here (there is a
// dedicated reset route), so the existing hash is read and written back
// unchanged — the hash is never rendered or accepted from the form.
//
// The write is field-scoped (UpdateUserProfile touches display and role only), so
// it cannot revert a password change that commits between this handler's read and
// its write. The row is still READ first, but only to learn the previous role for
// the session-revocation decision below — a stale read there is harmless, because
// currentUser re-reads the role from the Store on every request anyway.
//
// Demoting the last admin is refused by the STORE, inside the transaction that
// would perform the write (store.ErrLastAdmin -> 409). A role change revokes the
// affected user's sessions.
func (s *Server) handleEditUser(w http.ResponseWriter, r *http.Request) {
	me, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	role, verr := parseRole(r.PostFormValue("role"))
	if verr != "" {
		http.Error(w, verr, http.StatusBadRequest)
		return
	}
	display := strings.TrimSpace(r.PostFormValue("display"))
	if display == "" {
		display = id
	}

	existing, err := s.store.GetUser(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no such person", http.StatusNotFound)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "edit user: load failed", "user", id, "err", err.Error())
		http.Error(w, "could not save that person", http.StatusInternalServerError)
		return
	}

	previousRole := existing.Role
	if err := s.store.UpdateUserProfile(r.Context(), id, display, role); err != nil {
		s.writeUserWriteErr(w, r, id, err)
		return
	}

	// Session invalidation on a ROLE change: a demoted admin must stop being an
	// admin now, not whenever their week-long cookie expires. (currentUser re-reads
	// the user from the Store on every request, so the role is already re-checked
	// per request; revoking is the belt to that braces, and it covers promotion
	// too so nobody carries a half-stale session.) An unchanged role disturbs
	// nobody's session — including this admin's own when they only fixed a display
	// name.
	if previousRole != role {
		revoked := s.sessions.destroyUser(id)
		s.logger.InfoContext(r.Context(), "user role changed",
			"user", id, "from", string(previousRole), "to", string(role),
			"by", me.ID, "sessions_revoked", revoked)
	}
	s.refreshUsersList(w, r, me)
}

// --- POST /api/users/{id}/password (admin reset) -------------------------

// handleResetPassword handles POST /api/users/{id}/password: an admin sets a new
// password for SOMEONE ELSE. Form field: password.
//
// It deliberately does NOT ask for the target's current password — that is what
// makes it a reset. An admin resetting THEIR OWN password is refused here and
// pointed at /settings, which does verify the current password (guardrail 4): a
// reset is a recovery tool for other people, not a way to skip proving you are
// still at the keyboard.
//
// A successful reset revokes every session the target user has, so a password
// changed because it leaked actually locks the holder out.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	me, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	if id == me.ID {
		http.Error(w, "to change your own password, use Settings — it asks for your current password", http.StatusConflict)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Existence is checked before hashing so an unknown id costs a 404 rather than
	// an argon2id run. The write re-reports ErrNotFound if the user vanishes in
	// between; nothing here depends on the row's contents.
	if _, err := s.store.GetUser(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such person", http.StatusNotFound)
			return
		}
		s.logger.ErrorContext(r.Context(), "reset password: load failed", "user", id, "err", err.Error())
		http.Error(w, "could not set that password", http.StatusInternalServerError)
		return
	}

	hash, herr := s.hashPassword(r.Context(), r.PostFormValue("password"))
	if herr != nil {
		s.writePasswordErr(w, r, herr)
		return
	}
	if err := s.store.UpdateUserPassword(r.Context(), id, hash); err != nil {
		s.writeUserWriteErr(w, r, id, err)
		return
	}

	revoked := s.sessions.destroyUser(id)
	s.logger.InfoContext(r.Context(), "password reset by admin",
		"user", id, "by", me.ID, "sessions_revoked", revoked)
	s.refreshUsersList(w, r, me)
}

// --- POST /api/users/{id}/delete -----------------------------------------

// handleDeleteUser handles POST /api/users/{id}/delete. Three refusals, all
// humane rather than a driver-level 500:
//
//   - SELF-delete -> 409. Even for an admin who is not the last one: there is no
//     reason to allow it and every reason not to (you'd be deleting the session
//     you are using).
//   - LAST ADMIN -> 409, refused by the store inside the delete's transaction.
//   - OWNS DEVICES -> 409 naming the count and pointing at /nodes. nodes.owner_user_id
//     REFERENCES users (id) with NO on-delete action (0001_registry.up.sql), so the
//     driver would raise a foreign-key violation; a cascade is NOT the answer
//     (silently deleting or orphaning someone's devices is worse than a message
//     telling you to reassign them).
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	me, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	if id == me.ID {
		http.Error(w, "you can't delete your own account — ask another admin to do it", http.StatusConflict)
		return
	}

	err := s.store.DeleteUser(r.Context(), id)
	switch {
	case err == nil:
		// The user is gone, so currentUser would already fail for their cookie;
		// revoke anyway so nothing lingers in the session map.
		revoked := s.sessions.destroyUser(id)
		s.logger.InfoContext(r.Context(), "user deleted",
			"user", id, "by", me.ID, "sessions_revoked", revoked)
		s.refreshUsersList(w, r, me)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such person", http.StatusNotFound)
	case errors.Is(err, store.ErrLastAdmin):
		http.Error(w, lastAdminError, http.StatusConflict)
	case errors.Is(err, store.ErrInvalidReference):
		http.Error(w, s.ownedDevicesError(r.Context(), id), http.StatusConflict)
	default:
		s.logger.ErrorContext(r.Context(), "delete user failed", "user", id, "err", err.Error())
		http.Error(w, "could not remove that person", http.StatusInternalServerError)
	}
}

// ownedDevicesError builds the humane refusal for deleting a user who still owns
// devices, naming the count so the admin knows what they are looking for. It
// counts at message time (a fresh ListNodes); a counting failure degrades to the
// generic sentence rather than a 500 — the delete was already correctly refused.
func (s *Server) ownedDevicesError(ctx context.Context, id string) string {
	const suffix = "Reassign or remove them on the Devices page (/nodes) first."
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "count owned devices failed", "user", id, "err", err.Error())
		return "that person still owns devices. " + suffix
	}
	n := 0
	for _, node := range nodes {
		if node.OwnerUserID != nil && *node.OwnerUserID == id {
			n++
		}
	}
	noun := "devices"
	if n == 1 {
		noun = "device"
	}
	return fmt.Sprintf("%s still owns %d %s. %s", id, n, noun, suffix)
}

// --- GET /settings --------------------------------------------------------

// handleSettingsPage renders the settings page: who you are signed in as, the
// change-my-password form, and — for an admin — the way through to People.
// Available to EVERY signed-in user; changing your own password is the one
// user-management thing a non-admin can do.
//
// People is LINKED from here rather than inlined. Inlining the registry (list +
// add form + per-person edit/reset/delete) would rebuild the long
// mixed-purpose page that /syncs and /nodes were pulled apart to avoid. The
// point of this page is to get those destinations out of the top navigation,
// not to merge them into one screen.
func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	data := settingsPageData{
		User:           userView{ID: u.ID, Display: u.Display, Role: u.Role},
		CSRF:           s.csrfFor(r),
		Changed:        r.URL.Query().Get("changed") == "1",
		MinPasswordLen: auth.MinPasswordLen,
	}

	// The People summary is admin-only, and a counting failure must not cost the
	// user their password form: log it and leave the counts at zero, which the
	// template reads as "unknown" and omits — zero is otherwise unreachable,
	// since the admin looking at the page is themselves one of the people.
	if u.Role == store.RoleAdmin {
		users, err := s.store.ListUsers(r.Context())
		if err != nil {
			s.logger.ErrorContext(r.Context(), "settings: count people failed", "err", err.Error())
		} else {
			data.PeopleCount = len(users)
			data.AdminCount = countAdmins(users)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "settings", data); err != nil {
		s.logger.ErrorContext(r.Context(), "settings page render failed", "err", err.Error())
	}
}

// handleAccountRedirect keeps /account working. It was the self-service page
// until settings absorbed it; bookmarks, and any message that still says
// "Account", should land somewhere useful rather than 404.
func (s *Server) handleAccountRedirect(w http.ResponseWriter, r *http.Request) {
	dest := "/settings"
	if r.URL.RawQuery != "" {
		dest += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, dest, http.StatusMovedPermanently)
}

// --- POST /api/account/password (self-service change) --------------------

// handleChangeOwnPassword handles POST /api/account/password: any signed-in user
// changes THEIR OWN password. Form fields: current_password, new_password,
// confirm_password.
//
// The current password is verified first (guardrail 4) — a borrowed, unlocked
// session must not be enough to take an account over. A wrong current password is
// a 403 with a generic message; the new password goes through the same
// auth.ValidatePassword + auth.Hash as every other path.
//
// On success every session of this user is revoked (including the one making the
// request) and a FRESH session is minted for the caller, so: other devices are
// logged out, and the caller keeps working with a rotated token + rotated CSRF.
// Because the CSRF token rotates, the response is a redirect to /settings rather
// than an in-place fragment — the page reloads with the new token instead of
// leaving the stale one embedded in the DOM.
func (s *Server) handleChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	me, ok := userFromContext(r.Context())
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	current := r.PostFormValue("current_password")
	next := r.PostFormValue("new_password")
	confirm := r.PostFormValue("confirm_password")

	if next != confirm {
		http.Error(w, "the two new passwords don't match", http.StatusBadRequest)
		return
	}

	// Re-read the user rather than trusting the context copy: the hash must be the
	// one currently stored (an admin may have reset it since this page loaded).
	existing, err := s.store.GetUser(r.Context(), me.ID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "change password: load failed", "user", me.ID, "err", err.Error())
		http.Error(w, "could not change your password", http.StatusInternalServerError)
		return
	}

	okPw, verr := s.verifyPassword(r.Context(), existing.PwHash, current)
	if verr != nil {
		s.writePasswordErr(w, r, verr)
		return
	}
	if !okPw {
		http.Error(w, "that's not your current password", http.StatusForbidden)
		return
	}

	hash, herr := s.hashPassword(r.Context(), next)
	if herr != nil {
		s.writePasswordErr(w, r, herr)
		return
	}
	// Field-scoped on purpose: an admin may be demoting this very user right now,
	// and a whole-row write would carry the role this handler read a moment ago
	// back into the table, silently restoring a privilege that was just revoked.
	if err := s.store.UpdateUserPassword(r.Context(), me.ID, hash); err != nil {
		s.writeUserWriteErr(w, r, me.ID, err)
		return
	}

	// Revoke every session (this one included), then re-establish just this one.
	revoked := s.sessions.destroyUser(me.ID)
	s.logger.InfoContext(r.Context(), "password changed by owner",
		"user", me.ID, "sessions_revoked", revoked)
	token, terr := s.sessions.create(me.ID)
	if terr != nil {
		// The password IS changed; we just failed to mint the replacement session.
		// Clear the cookie and send them to /login rather than pretending.
		s.logger.ErrorContext(r.Context(), "change password: re-session failed", "err", terr.Error())
		clearCookie(w)
		s.hxRedirect(w, r, "/login")
		return
	}
	s.sessions.setCookie(w, token)
	s.hxRedirect(w, r, "/settings?changed=1")
}

// --- shared helpers ------------------------------------------------------

// userIDError is the human 400 for a badly-shaped username. A user id is a
// registry slug, the same shape as a node id (slug.go): it appears in URLs
// (/api/users/{id}) and is typed at the login prompt.
const userIDError = "username must be lowercase letters, digits, and hyphens (e.g. alex or bob-smith)"

// lastAdminError is the human refusal for removing the final admin, shared by
// the delete and edit paths.
const lastAdminError = "that's the only admin — make someone else an admin first, or nobody can manage RetroSync"

// parseRole validates a role form value, returning a human message on failure.
func parseRole(raw string) (store.Role, string) {
	role := store.Role(strings.TrimSpace(raw))
	if !store.ValidRole(role) {
		return "", "role must be user or admin"
	}
	return role, ""
}

// errPasswordBusy is returned by the password helpers when the argon2
// concurrency cap could not be acquired before the client went away.
var errPasswordBusy = errors.New("password work: server busy")

// hashPassword validates a proposed password (auth.ValidatePassword — the same
// rule `user set` applies) and hashes it with auth.Hash — the same function, the
// same cost parameters. There is no second hashing path in this codebase.
//
// The hash runs under s.loginSem, the same semaphore that caps concurrent login
// verifies: each argon2id call pins ~64 MiB, so an unbounded burst of password
// writes could OOM a small pod just as logins could.
func (s *Server) hashPassword(ctx context.Context, password string) (string, error) {
	if err := auth.ValidatePassword(password); err != nil {
		return "", err
	}
	select {
	case s.loginSem <- struct{}{}:
	case <-ctx.Done():
		return "", errPasswordBusy
	}
	defer func() { <-s.loginSem }()
	h, err := auth.Hash(password)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return h, nil
}

// verifyPassword checks a cleartext password against a stored hash under the
// same argon2 concurrency cap as hashPassword and login.
func (s *Server) verifyPassword(ctx context.Context, encodedHash, password string) (bool, error) {
	select {
	case s.loginSem <- struct{}{}:
	case <-ctx.Done():
		return false, errPasswordBusy
	}
	defer func() { <-s.loginSem }()
	ok, err := auth.Verify(encodedHash, password)
	if err != nil {
		return false, fmt.Errorf("verify password: %w", err)
	}
	return ok, nil
}

// writePasswordErr maps a hashPassword/verifyPassword failure to a response: the
// validation rule is shown to the human (it is advice, not a leak), a busy
// semaphore is a 503, and anything else is an opaque 500 with the detail logged.
func (s *Server) writePasswordErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrPasswordTooShort):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errPasswordBusy):
		http.Error(w, "server busy — try again", http.StatusServiceUnavailable)
	default:
		// Never echo the error: it comes from the crypto path.
		s.logger.ErrorContext(r.Context(), "password work failed", "err", err.Error())
		http.Error(w, "could not set that password", http.StatusInternalServerError)
	}
}

// writeUserWriteErr maps a user-update error to an HTTP response. ErrLastAdmin is
// the lockout guard the store enforces inside the write's transaction.
func (s *Server) writeUserWriteErr(w http.ResponseWriter, r *http.Request, id string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such person", http.StatusNotFound)
	case errors.Is(err, store.ErrLastAdmin):
		http.Error(w, lastAdminError, http.StatusConflict)
	case errors.Is(err, store.ErrInvalidValue):
		http.Error(w, "role must be user or admin", http.StatusUnprocessableEntity)
	default:
		s.logger.ErrorContext(r.Context(), "user write failed", "user", id, "err", err.Error())
		http.Error(w, "could not save that person", http.StatusInternalServerError)
	}
}

// redirect sends the caller to dest, honoring HTMX: an HX-Request gets an
// HX-Redirect header (htmx swaps fragments, so a 303 would be swallowed),
// everything else gets a plain 303. Mirrors the discovery create flow.
func (s *Server) hxRedirect(w http.ResponseWriter, r *http.Request, dest string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// refreshUsersList re-renders the user-list fragment (the #users-list region)
// after a successful mutation, so HTMX swaps the updated list in place.
func (s *Server) refreshUsersList(w http.ResponseWriter, r *http.Request, me store.User) {
	data, err := s.buildUsersPage(r.Context(), me)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "refresh users build failed", "err", err.Error())
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	data.CSRF = s.csrfFor(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "users-list", data); err != nil {
		s.logger.ErrorContext(r.Context(), "refresh users render failed", "err", err.Error())
	}
}
