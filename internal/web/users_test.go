package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/a-mcf/retrosync/internal/auth"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

// The /users + /account handler tests (slice-36). The fixture (newActionFixture)
// seeds exactly one admin (bob) and one regular user (carol), with carol owning
// carol-deck — which is precisely the shape the guardrails care about: a last
// admin, and a user who owns a device.

const newTestPassword = "another-long-password"

// getPage issues an authenticated GET and returns the recorder.
func getPage(t *testing.T, f *actionFixture, c *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// userRole reads a user's current role straight from the store.
func userRole(t *testing.T, f *actionFixture, id string) store.Role {
	t.Helper()
	u, err := f.store.GetUser(context.Background(), id)
	if err != nil {
		t.Fatalf("get user %s: %v", id, err)
	}
	return u.Role
}

// passwordWorks reports whether id can log in with pw.
func passwordWorks(t *testing.T, f *actionFixture, id, pw string) bool {
	t.Helper()
	form := url.Values{"username": {id}, "password": {pw}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec.Code == http.StatusSeeOther
}

// --- GET /users ----------------------------------------------------------

func TestUsersPage_AdminSeesEveryone(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "bob")

	rec := getPage(t, f, c, "/users")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"bob", "carol", "Add a person", "csrf_token", "only admin"} {
		if !strings.Contains(body, want) {
			t.Errorf("users page missing %q", want)
		}
	}
	// carol owns carol-deck: the row links the device count at /nodes.
	if !strings.Contains(body, `href="/nodes"`) {
		t.Error("device count is not linked to /nodes")
	}
}

func TestUsersPage_NonAdminForbidden(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "carol")

	rec := getPage(t, f, c, "/users")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET /users = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Admins only") {
		t.Errorf("expected the same 'Admins only' page /nodes gives, got: %s", rec.Body.String())
	}
}

func TestUsersAPI_NonAdminForbidden(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "carol")

	cases := []struct {
		name, path string
		fields     url.Values
	}{
		{"create", "/api/users", url.Values{"id": {"eve"}, "role": {"admin"}, "password": {newTestPassword}}},
		{"edit", "/api/users/bob", url.Values{"display": {"Bob"}, "role": {"user"}}},
		{"reset", "/api/users/bob/password", url.Values{"password": {newTestPassword}}},
		{"delete", "/api/users/bob/delete", url.Values{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postForm(t, f, c, csrf, tc.path, tc.fields)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("non-admin POST %s = %d, want 403", tc.path, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "admins only") {
				t.Errorf("body = %q, want the 'admins only' JSON error", rec.Body.String())
			}
		})
	}
	// Nothing was written.
	if _, err := f.store.GetUser(context.Background(), "eve"); err == nil {
		t.Error("non-admin managed to create a user")
	}
	if userRole(t, f, "bob") != store.RoleAdmin {
		t.Error("non-admin managed to demote the admin")
	}
}

// --- POST /api/users (create) --------------------------------------------

func TestCreateUser_HappyPath(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/users", url.Values{
		"id": {"dave"}, "display": {"Dave"}, "role": {"user"},
		"password": {newTestPassword},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	u, err := f.store.GetUser(context.Background(), "dave")
	if err != nil {
		t.Fatalf("user not created: %v", err)
	}
	if u.Display != "Dave" || u.Role != store.RoleUser {
		t.Errorf("created user wrong: %+v", u)
	}
	// The password went through auth.Hash — the same path `user set` uses — and
	// the cleartext was never stored.
	if u.PwHash == newTestPassword || u.PwHash == "" {
		t.Fatal("stored hash is empty or cleartext")
	}
	if ok, err := auth.Verify(u.PwHash, newTestPassword); err != nil || !ok {
		t.Fatalf("stored hash does not verify: ok=%v err=%v", ok, err)
	}
	if !passwordWorks(t, f, "dave", newTestPassword) {
		t.Error("the new user cannot log in with the password just set")
	}
	// The refreshed list fragment came back.
	if !strings.Contains(rec.Body.String(), "dave") {
		t.Error("response did not include the refreshed user list")
	}
}

func TestCreateUser_DisplayDefaultsToID(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/users", url.Values{
		"id": {"dave"}, "role": {"user"}, "password": {newTestPassword},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	u, err := f.store.GetUser(context.Background(), "dave")
	if err != nil {
		t.Fatalf("user not created: %v", err)
	}
	if u.Display != "dave" {
		t.Errorf("display = %q, want it to default to the id (like `user set`)", u.Display)
	}
}

func TestCreateUser_Rejections(t *testing.T) {
	tests := []struct {
		name       string
		fields     url.Values
		wantStatus int
		wantBody   string
	}{
		{
			name:       "duplicate id",
			fields:     url.Values{"id": {"carol"}, "role": {"user"}, "password": {newTestPassword}},
			wantStatus: http.StatusConflict,
			wantBody:   "already exists",
		},
		{
			name:       "bad slug id",
			fields:     url.Values{"id": {"Dave Smith"}, "role": {"user"}, "password": {newTestPassword}},
			wantStatus: http.StatusBadRequest,
			wantBody:   "lowercase",
		},
		{
			name:       "empty id",
			fields:     url.Values{"id": {"  "}, "role": {"user"}, "password": {newTestPassword}},
			wantStatus: http.StatusBadRequest,
			wantBody:   "id is required",
		},
		{
			name:       "bad role",
			fields:     url.Values{"id": {"dave"}, "role": {"wizard"}, "password": {newTestPassword}},
			wantStatus: http.StatusBadRequest,
			wantBody:   "role must be",
		},
		{
			name:       "short password",
			fields:     url.Values{"id": {"dave"}, "role": {"user"}, "password": {"short"}},
			wantStatus: http.StatusBadRequest,
			wantBody:   "at least",
		},
		{
			name:       "empty password",
			fields:     url.Values{"id": {"dave"}, "role": {"user"}},
			wantStatus: http.StatusBadRequest,
			wantBody:   "at least",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newActionFixture(t)
			c, csrf := loginAs(t, f, "bob")

			rec := postForm(t, f, c, csrf, "/api/users", tc.fields)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to mention %q", rec.Body.String(), tc.wantBody)
			}
			if _, err := f.store.GetUser(context.Background(), "dave"); err == nil {
				t.Error("a rejected create still wrote a user")
			}
		})
	}
}

// --- POST /api/users/{id} (edit) -----------------------------------------

func TestEditUser_DisplayAndRole(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/users/carol", url.Values{
		"display": {"Carol Danvers"}, "role": {"admin"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	u, err := f.store.GetUser(context.Background(), "carol")
	if err != nil {
		t.Fatalf("get carol: %v", err)
	}
	if u.Display != "Carol Danvers" || u.Role != store.RoleAdmin {
		t.Fatalf("edit not applied: %+v", u)
	}
	// The password survives an edit that does not touch it.
	if !passwordWorks(t, f, "carol", testPassword) {
		t.Error("editing display/role invalidated the password")
	}
}

func TestEditUser_RoleChangeRevokesSessions(t *testing.T) {
	f := newActionFixture(t)
	admin, csrf := loginAs(t, f, "bob")
	carol, _ := loginAs(t, f, "carol")

	// Carol has a live session.
	if rec := getPage(t, f, carol, "/"); rec.Code != http.StatusOK {
		t.Fatalf("carol's session should work before the role change: %d", rec.Code)
	}
	rec := postForm(t, f, admin, csrf, "/api/users/carol", url.Values{
		"display": {"carol"}, "role": {"admin"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("promote status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	// A role change revokes her sessions: the old cookie no longer authenticates.
	if got := getPage(t, f, carol, "/"); got.Code != http.StatusSeeOther {
		t.Fatalf("after a role change the old session still works (status %d)", got.Code)
	}
	// The admin's own session is untouched.
	if got := getPage(t, f, admin, "/users"); got.Code != http.StatusOK {
		t.Fatalf("the acting admin was logged out by someone else's role change: %d", got.Code)
	}
}

func TestEditUser_DisplayOnlyKeepsSession(t *testing.T) {
	f := newActionFixture(t)
	admin, csrf := loginAs(t, f, "bob")
	carol, _ := loginAs(t, f, "carol")

	rec := postForm(t, f, admin, csrf, "/api/users/carol", url.Values{
		"display": {"Carol"}, "role": {"user"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d, want 200", rec.Code)
	}
	if got := getPage(t, f, carol, "/"); got.Code != http.StatusOK {
		t.Fatalf("a display-only edit logged carol out (status %d)", got.Code)
	}
}

func TestEditUser_Unknown404(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/users/ghost", url.Values{
		"display": {"Ghost"}, "role": {"user"},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("edit unknown user = %d, want 404", rec.Code)
	}
}

// --- last-admin guardrail ------------------------------------------------

func TestLastAdmin_DemoteRefused(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob") // the only admin

	rec := postForm(t, f, c, csrf, "/api/users/bob", url.Values{
		"display": {"Bob"}, "role": {"user"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("demoting the last admin = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "only admin") {
		t.Errorf("body = %q, want a humane last-admin message", rec.Body.String())
	}
	if userRole(t, f, "bob") != store.RoleAdmin {
		t.Fatal("the last admin was demoted anyway")
	}
}

func TestLastAdmin_DeleteRefused(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	// Delete bob from ANOTHER admin's session would be needed to get past the
	// self-delete guard, so promote carol first and act as her.
	if rec := postForm(t, f, c, csrf, "/api/users/carol", url.Values{
		"display": {"carol"}, "role": {"admin"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("promote carol: %d", rec.Code)
	}
	carol, carolCSRF := loginAs(t, f, "carol")
	// Two admins: deleting bob is allowed — once his device is unassigned (the
	// owns-devices guard is a separate rule, exercised elsewhere).
	if rec := postForm(t, f, carol, carolCSRF, "/api/nodes/bob-deck", url.Values{
		"display": {"bob-deck"}, "kind": {"deck"},
		"reach": {"syncthing-share"}, "path": {"/srv/x"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("unassign bob-deck: %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := postForm(t, f, carol, carolCSRF, "/api/users/bob/delete", url.Values{}); rec.Code != http.StatusOK {
		t.Fatalf("deleting a non-last admin = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	// Carol is now the last admin; she owns carol-deck, but the last-admin guard
	// is what must speak here (it is checked before the FK).
	rec := postForm(t, f, carol, carolCSRF, "/api/users/carol/delete", url.Values{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting the last admin = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.GetUser(context.Background(), "carol"); err != nil {
		t.Fatalf("the last admin was deleted anyway: %v", err)
	}
}

func TestLastAdmin_TwoAdminsBothOperationsSucceed(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	// Promote carol -> two admins.
	if rec := postForm(t, f, c, csrf, "/api/users/carol", url.Values{
		"display": {"carol"}, "role": {"admin"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("promote carol: %d", rec.Code)
	}
	// Now demoting bob is fine (carol remains).
	if rec := postForm(t, f, c, csrf, "/api/users/bob", url.Values{
		"display": {"Bob"}, "role": {"user"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("demote with a second admin = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if userRole(t, f, "bob") != store.RoleUser {
		t.Fatal("demote did not apply")
	}
	// And deleting the (now regular) bob works from carol's admin session — he
	// owns bob-deck, so reassign that first (the FK guard is a separate rule).
	carol, carolCSRF := loginAs(t, f, "carol")
	if rec := postForm(t, f, carol, carolCSRF, "/api/nodes/bob-deck", url.Values{
		"display": {"bob-deck"}, "kind": {"deck"},
		"reach": {"syncthing-share"}, "path": {"/srv/x"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("unassign bob-deck: %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := postForm(t, f, carol, carolCSRF, "/api/users/bob/delete", url.Values{}); rec.Code != http.StatusOK {
		t.Fatalf("delete with a second admin = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
}

// --- delete guardrails ---------------------------------------------------

func TestDeleteUser_SelfRefused(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	// Promote carol so bob is NOT the last admin: self-delete must be refused on
	// its own merits, not because of the last-admin rule.
	if rec := postForm(t, f, c, csrf, "/api/users/carol", url.Values{
		"display": {"carol"}, "role": {"admin"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("promote carol: %d", rec.Code)
	}
	rec := postForm(t, f, c, csrf, "/api/users/bob/delete", url.Values{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("self-delete = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "your own account") {
		t.Errorf("body = %q, want a humane self-delete message", rec.Body.String())
	}
	if _, err := f.store.GetUser(context.Background(), "bob"); err != nil {
		t.Fatalf("self-delete went through anyway: %v", err)
	}
}

func TestDeleteUser_OwningDevicesRefusedHumanely(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")
	ctx := context.Background()

	// carol owns carol-deck.
	rec := postForm(t, f, c, csrf, "/api/users/carol/delete", url.Values{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting a device owner = %d, want 409 (never a driver 500)\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"carol", "1 device", "/nodes"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal %q missing %q — it must name the count and point at /nodes", body, want)
		}
	}
	// Both the user AND the device survive: nothing was orphaned or cascaded.
	if _, err := f.store.GetUser(ctx, "carol"); err != nil {
		t.Errorf("the refused delete removed the user: %v", err)
	}
	n, err := f.store.GetNode(ctx, "carol-deck")
	if err != nil {
		t.Fatalf("the refused delete removed the device: %v", err)
	}
	if n.OwnerUserID == nil || *n.OwnerUserID != "carol" {
		t.Errorf("the refused delete orphaned the device: %+v", n)
	}

	// Reassigning the device makes the delete legal.
	if rec := postForm(t, f, c, csrf, "/api/nodes/carol-deck", url.Values{
		"display": {"carol-deck"}, "kind": {"deck"},
		"reach": {"syncthing-share"}, "path": {"/srv/x"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("unassign carol-deck: %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := postForm(t, f, c, csrf, "/api/users/carol/delete", url.Values{}); rec.Code != http.StatusOK {
		t.Fatalf("delete after reassigning = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteUser_RevokesSessionsAndUnknown404(t *testing.T) {
	f := newActionFixture(t)
	admin, csrf := loginAs(t, f, "bob")
	ctx := context.Background()

	// Give carol a session, unassign her device, then delete her.
	carol, _ := loginAs(t, f, "carol")
	if rec := postForm(t, f, admin, csrf, "/api/nodes/carol-deck", url.Values{
		"display": {"carol-deck"}, "kind": {"deck"},
		"reach": {"syncthing-share"}, "path": {"/srv/x"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("unassign carol-deck: %d", rec.Code)
	}
	if rec := postForm(t, f, admin, csrf, "/api/users/carol/delete", url.Values{}); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200", rec.Code)
	}
	if _, err := f.store.GetUser(ctx, "carol"); err == nil {
		t.Fatal("user still present after delete")
	}
	// A deleted user cannot authenticate again, with an old cookie or a password.
	if got := getPage(t, f, carol, "/"); got.Code != http.StatusSeeOther {
		t.Errorf("a deleted user's session still works (status %d)", got.Code)
	}
	if passwordWorks(t, f, "carol", testPassword) {
		t.Error("a deleted user can still log in")
	}
	// Deleting again is a clean 404.
	if rec := postForm(t, f, admin, csrf, "/api/users/carol/delete", url.Values{}); rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown user = %d, want 404", rec.Code)
	}
}

// --- POST /api/users/{id}/password (admin reset) -------------------------

func TestResetPassword_AdminSetsSomeoneElses(t *testing.T) {
	f := newActionFixture(t)
	admin, csrf := loginAs(t, f, "bob")
	carol, _ := loginAs(t, f, "carol")

	rec := postForm(t, f, admin, csrf, "/api/users/carol/password", url.Values{
		"password": {newTestPassword},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if !passwordWorks(t, f, "carol", newTestPassword) {
		t.Error("the new password does not work")
	}
	if passwordWorks(t, f, "carol", testPassword) {
		t.Error("the old password still works after a reset")
	}
	// A reset signs the target out everywhere.
	if got := getPage(t, f, carol, "/"); got.Code != http.StatusSeeOther {
		t.Errorf("carol's session survived a password reset (status %d)", got.Code)
	}
	// Her role and display are untouched.
	u, err := f.store.GetUser(context.Background(), "carol")
	if err != nil {
		t.Fatalf("get carol: %v", err)
	}
	if u.Role != store.RoleUser || u.Display != "carol" {
		t.Errorf("reset changed more than the password: %+v", u)
	}
}

func TestResetPassword_OwnAccountPointedAtSelfService(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/users/bob/password", url.Values{
		"password": {newTestPassword},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("admin resetting their own password = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Settings") {
		t.Errorf("body = %q, want it to point at the self-service page", rec.Body.String())
	}
	// The password is unchanged: an admin cannot skip proving they are still at
	// the keyboard.
	if !passwordWorks(t, f, "bob", testPassword) {
		t.Error("the refused self-reset changed the password anyway")
	}
}

func TestResetPassword_Rejections(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "bob")

	if rec := postForm(t, f, c, csrf, "/api/users/carol/password", url.Values{
		"password": {"short"},
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("short password = %d, want 400", rec.Code)
	}
	if rec := postForm(t, f, c, csrf, "/api/users/ghost/password", url.Values{
		"password": {newTestPassword},
	}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown user = %d, want 404", rec.Code)
	}
	if !passwordWorks(t, f, "carol", testPassword) {
		t.Error("a rejected reset changed the password")
	}
}

// --- /account (self-service) ---------------------------------------------

func TestSettingsPage_AnySignedInUser(t *testing.T) {
	f := newActionFixture(t)
	for _, id := range []string{"bob", "carol"} {
		c, _ := loginAs(t, f, id)
		rec := getPage(t, f, c, "/settings")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s GET /settings = %d, want 200", id, rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{"current_password", "new_password", "confirm_password", "csrf_token"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s settings page missing %q", id, want)
			}
		}
	}
}

func TestSettingsPage_UnauthenticatedRedirects(t *testing.T) {
	f := newActionFixture(t)
	rec := getPage(t, f, nil, "/settings")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("anonymous GET /settings = %d, want 303 to /login", rec.Code)
	}
}

func TestChangeOwnPassword_WrongCurrentRefused(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "carol")

	rec := postForm(t, f, c, csrf, "/api/account/password", url.Values{
		"current_password": {"not-my-password"},
		"new_password":     {newTestPassword},
		"confirm_password": {newTestPassword},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong current password = %d, want 403\n%s", rec.Code, rec.Body.String())
	}
	if passwordWorks(t, f, "carol", newTestPassword) {
		t.Fatal("the password changed despite a wrong current password")
	}
	if !passwordWorks(t, f, "carol", testPassword) {
		t.Fatal("the old password stopped working after a refused change")
	}
	// The refused attempt did not disturb the session.
	if got := getPage(t, f, c, "/settings"); got.Code != http.StatusOK {
		t.Errorf("a refused change logged the user out (status %d)", got.Code)
	}
}

func TestChangeOwnPassword_HappyPathRotatesSession(t *testing.T) {
	f := newActionFixture(t)
	c, csrf := loginAs(t, f, "carol")
	// A second device for carol, which must be signed out by the change.
	other, _ := loginAs(t, f, "carol")

	rec := postForm(t, f, c, csrf, "/api/account/password", url.Values{
		"current_password": {testPassword},
		"new_password":     {newTestPassword},
		"confirm_password": {newTestPassword},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("change status = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/settings?changed=1" {
		t.Errorf("redirect = %q, want /settings?changed=1", loc)
	}
	// The new password works for login; the old one does not.
	if !passwordWorks(t, f, "carol", newTestPassword) {
		t.Error("the new password does not work for login")
	}
	if passwordWorks(t, f, "carol", testPassword) {
		t.Error("the old password still works after a change")
	}
	// The caller keeps working — on a FRESH session cookie...
	fresh := sessionCookie(rec.Result().Cookies())
	if fresh == nil {
		t.Fatal("no replacement session cookie was set")
	}
	if fresh.Value == c.Value {
		t.Error("the session token was not rotated on a password change")
	}
	if got := getPage(t, f, fresh, "/settings"); got.Code != http.StatusOK {
		t.Errorf("the replacement session does not work (status %d)", got.Code)
	}
	// ...while the old token and every other device are signed out.
	if got := getPage(t, f, c, "/settings"); got.Code != http.StatusSeeOther {
		t.Errorf("the pre-change session token still works (status %d)", got.Code)
	}
	if got := getPage(t, f, other, "/settings"); got.Code != http.StatusSeeOther {
		t.Errorf("the other device was not signed out (status %d)", got.Code)
	}
	// The confirmation page renders.
	if got := getPage(t, f, fresh, "/settings?changed=1"); !strings.Contains(got.Body.String(), "Password changed") {
		t.Error("the post-change page does not confirm the change")
	}
}

// --- field-scoped writes -------------------------------------------------

// fieldScopedStore counts which user-write methods a handler actually calls.
//
// This is how the "a password change must not revert a role" rule is pinned at
// the handler level. The guarantee is NOT that a handler writes the right role
// back — that is a read-modify-write, and the value it writes is only as fresh as
// its read. The guarantee is that a password handler never writes a role at all,
// so an admin's demotion committing mid-request cannot be undone by it.
type fieldScopedStore struct {
	store.Store
	profileWrites  int
	passwordWrites int
}

func (s *fieldScopedStore) UpdateUserProfile(ctx context.Context, id string, display string, role store.Role) error {
	s.profileWrites++
	return s.Store.UpdateUserProfile(ctx, id, display, role)
}

func (s *fieldScopedStore) UpdateUserPassword(ctx context.Context, id string, pwHash string) error {
	s.passwordWrites++
	return s.Store.UpdateUserPassword(ctx, id, pwHash)
}

// newFieldScopedFixture builds a server over a counting store. Two admins, so
// role changes are not blocked by the last-admin guard.
func newFieldScopedFixture(t *testing.T) (*actionFixture, *fieldScopedStore) {
	t.Helper()
	st := memory.New()
	ctx := context.Background()
	hash, err := auth.Hash(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	for _, u := range []store.User{
		{ID: "bob", Display: "bob", PwHash: hash, Role: store.RoleAdmin},
		{ID: "dave", Display: "dave", PwHash: hash, Role: store.RoleAdmin},
		{ID: "carol", Display: "carol", PwHash: hash, Role: store.RoleUser},
	} {
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user %s: %v", u.ID, err)
		}
	}
	counting := &fieldScopedStore{Store: st}
	srv, err := New(counting, Options{Actioner: &stubActioner{}, ShareRoot: "/shares"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &actionFixture{srv: srv, store: counting}, counting
}

// TestChangeOwnPassword_WritesOnlyThePassword is the regression test for the
// silent un-demotion: carol changes her own password while an admin demotes her.
// If this handler issues a whole-row write, it carries the role it read before
// the demotion back into the table and restores a privilege that was just
// revoked — with nothing in the logs to say so.
func TestChangeOwnPassword_WritesOnlyThePassword(t *testing.T) {
	f, counts := newFieldScopedFixture(t)
	c, csrf := loginAs(t, f, "carol")

	rec := postForm(t, f, c, csrf, "/api/account/password", url.Values{
		"current_password": {testPassword},
		"new_password":     {newTestPassword},
		"confirm_password": {newTestPassword},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("change status = %d, want 303\n%s", rec.Code, rec.Body.String())
	}
	if counts.passwordWrites != 1 {
		t.Errorf("password writes = %d, want 1", counts.passwordWrites)
	}
	if counts.profileWrites != 0 {
		t.Errorf("a self-service password change issued %d profile write(s); it must touch "+
			"pw_hash only, or it reverts a concurrent demotion", counts.profileWrites)
	}
}

// TestResetPassword_WritesOnlyThePassword is the same rule on the admin reset
// path: resetting someone's password must not resurrect the role they held when
// the handler loaded them.
func TestResetPassword_WritesOnlyThePassword(t *testing.T) {
	f, counts := newFieldScopedFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/users/carol/password", url.Values{
		"password": {newTestPassword},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if counts.passwordWrites != 1 {
		t.Errorf("password writes = %d, want 1", counts.passwordWrites)
	}
	if counts.profileWrites != 0 {
		t.Errorf("an admin password reset issued %d profile write(s), want 0", counts.profileWrites)
	}
}

// TestEditUser_WritesOnlyTheProfile is the mirror image: editing display/role
// must not write pw_hash, or the edit form reverts a password changed since the
// page was loaded.
func TestEditUser_WritesOnlyTheProfile(t *testing.T) {
	f, counts := newFieldScopedFixture(t)
	c, csrf := loginAs(t, f, "bob")

	rec := postForm(t, f, c, csrf, "/api/users/carol", url.Values{
		"display": {"Carol C."},
		"role":    {"admin"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if counts.profileWrites != 1 {
		t.Errorf("profile writes = %d, want 1", counts.profileWrites)
	}
	if counts.passwordWrites != 0 {
		t.Errorf("an edit issued %d password write(s); it must touch display and role "+
			"only, or it reverts a concurrent password change", counts.passwordWrites)
	}
}

func TestChangeOwnPassword_Rejections(t *testing.T) {
	tests := []struct {
		name       string
		fields     url.Values
		wantStatus int
		wantBody   string
	}{
		{
			name: "mismatched confirmation",
			fields: url.Values{
				"current_password": {testPassword},
				"new_password":     {newTestPassword},
				"confirm_password": {newTestPassword + "-typo"},
			},
			wantStatus: http.StatusBadRequest,
			wantBody:   "don't match",
		},
		{
			name: "too short",
			fields: url.Values{
				"current_password": {testPassword},
				"new_password":     {"short"},
				"confirm_password": {"short"},
			},
			wantStatus: http.StatusBadRequest,
			wantBody:   "at least",
		},
		{
			name: "empty current password",
			fields: url.Values{
				"new_password":     {newTestPassword},
				"confirm_password": {newTestPassword},
			},
			wantStatus: http.StatusForbidden,
			wantBody:   "current password",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newActionFixture(t)
			c, csrf := loginAs(t, f, "carol")

			rec := postForm(t, f, c, csrf, "/api/account/password", tc.fields)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to mention %q", rec.Body.String(), tc.wantBody)
			}
			if !passwordWorks(t, f, "carol", testPassword) {
				t.Error("a rejected change altered the password")
			}
		})
	}
}

func TestChangeOwnPassword_RequiresCSRF(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "carol")

	rec := postForm(t, f, c, "", "/api/account/password", url.Values{
		"current_password": {testPassword},
		"new_password":     {newTestPassword},
		"confirm_password": {newTestPassword},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF = %d, want 403", rec.Code)
	}
	if passwordWorks(t, f, "carol", newTestPassword) {
		t.Fatal("a CSRF-less request changed the password")
	}
}

// --- pw_hash never renders ------------------------------------------------

// TestNoPasswordHashInAnyResponse is the blunt instrument: take the REAL stored
// argon2 hashes and assert no page, fragment, or JSON response contains them (or
// any recognizable fragment of one). A hash in a response body is a credential
// leak even though it is not a cleartext password.
func TestNoPasswordHashInAnyResponse(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	c, csrf := loginAs(t, f, "bob")

	users, err := f.store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	var needles []string
	for _, u := range users {
		if u.PwHash == "" {
			t.Fatalf("fixture user %s has no hash; the test would prove nothing", u.ID)
		}
		needles = append(needles, u.PwHash)
		// The salt+key segment alone is enough to be a leak.
		if parts := strings.Split(u.PwHash, "$"); len(parts) == 6 {
			needles = append(needles, parts[4], parts[5])
		}
	}
	// "pw_hash" / "argon2" must not appear either — no field name, no marker.
	needles = append(needles, "pw_hash", "argon2")

	bodies := map[string]string{}
	for _, path := range []string{"/users", "/settings", "/nodes", "/", "/api/status", "/api/nodes", "/api/syncs"} {
		bodies[path] = getPage(t, f, c, path).Body.String()
	}
	// Mutation responses (the refreshed list fragment) too.
	bodies["POST /api/users"] = postForm(t, f, c, csrf, "/api/users", url.Values{
		"id": {"dave"}, "role": {"user"}, "password": {newTestPassword},
	}).Body.String()
	bodies["POST /api/users/dave"] = postForm(t, f, c, csrf, "/api/users/dave", url.Values{
		"display": {"Dave"}, "role": {"user"},
	}).Body.String()
	bodies["POST /api/users/dave/password"] = postForm(t, f, c, csrf, "/api/users/dave/password", url.Values{
		"password": {newTestPassword},
	}).Body.String()
	bodies["POST /api/users/dave/delete"] = postForm(t, f, c, csrf, "/api/users/dave/delete", url.Values{}).Body.String()

	for where, body := range bodies {
		for _, needle := range needles {
			if strings.Contains(body, needle) {
				t.Errorf("%s response leaks %q", where, needle)
			}
		}
	}
}

// --- /settings ------------------------------------------------------------

// TestAccountRedirectsToSettings: /account was the self-service page until
// settings absorbed it. It has to keep working — bookmarks, and the 409 that
// sends an admin there to change their own password.
func TestAccountRedirectsToSettings(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "carol")

	rec := getPage(t, f, c, "/account")
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /account = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/settings" {
		t.Errorf("redirect = %q, want /settings", loc)
	}
	// The post-change confirmation arrives as ?changed=1, so the query has to
	// survive the redirect or the confirmation silently disappears.
	q := getPage(t, f, c, "/account?changed=1")
	if loc := q.Header().Get("Location"); loc != "/settings?changed=1" {
		t.Errorf("redirect = %q, want the query preserved", loc)
	}
}

// TestSettingsPage_PeopleIsAdminOnly: the People section is the reason People
// left the top nav, so an admin must find it here — and a non-admin must not,
// since /users would 403 them anyway.
func TestSettingsPage_PeopleIsAdminOnly(t *testing.T) {
	f := newActionFixture(t)

	admin, _ := loginAs(t, f, "bob")
	body := getPage(t, f, admin, "/settings").Body.String()
	for _, want := range []string{"Administration", "Manage people", `href="/users"`} {
		if !strings.Contains(body, want) {
			t.Errorf("admin settings page missing %q", want)
		}
	}
	// The fixture seeds bob (admin) and carol (user).
	if !strings.Contains(body, "2 people") || !strings.Contains(body, "1 admin") {
		t.Errorf("admin settings page should summarise the registry, got:\n%s", body)
	}

	plain, _ := loginAs(t, f, "carol")
	pbody := getPage(t, f, plain, "/settings").Body.String()
	for _, unwanted := range []string{"Administration", "Manage people"} {
		if strings.Contains(pbody, unwanted) {
			t.Errorf("non-admin settings page offers %q", unwanted)
		}
	}
	// ...but they still get their own password form.
	if !strings.Contains(pbody, "current_password") {
		t.Error("non-admin settings page is missing the password form")
	}
}

// TestNav_SharedAcrossPages: the navigation is one partial now, so every page
// offers the same destinations — and People is not among them.
func TestNav_SharedAcrossPages(t *testing.T) {
	f := newActionFixture(t)
	c, _ := loginAs(t, f, "bob")

	// /syncs/{id}/history is the page that needed a NEW User field for the shared
	// nav — a zero-value userView renders without error, so only an explicit
	// check catches it going missing.
	for _, path := range []string{"/", "/syncs", "/nodes", "/users", "/discover", "/settings", "/syncs/sm-bob/history"} {
		body := getPage(t, f, c, path).Body.String()
		for _, want := range []string{`href="/discover"`, `href="/syncs"`, `href="/nodes"`, `href="/settings"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s nav missing %q", path, want)
			}
		}
		// People moved under Settings; the top nav must not link it directly.
		if strings.Contains(body, `<li><a href="/users">`) {
			t.Errorf("%s still links People from the nav", path)
		}
	}
}
