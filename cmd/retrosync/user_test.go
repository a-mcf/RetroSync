package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/a-mcf/retrosync/internal/auth"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

// testLogger returns a logger that discards output, for dispatch tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseUserSetDefaults(t *testing.T) {
	a, err := parseUserSet([]string{"bob"})
	if err != nil {
		t.Fatalf("parseUserSet: %v", err)
	}
	if a.id != "bob" || a.display != "bob" || a.role != store.RoleUser {
		t.Fatalf("got %+v, want id/display=bob role=user", a)
	}
}

func TestParseUserSetFlags(t *testing.T) {
	a, err := parseUserSet([]string{"--display", "Alice A.", "--role", "admin", "alice"})
	if err != nil {
		t.Fatalf("parseUserSet: %v", err)
	}
	if a.id != "alice" || a.display != "Alice A." || a.role != store.RoleAdmin {
		t.Fatalf("got %+v", a)
	}
}

// TestParseUserSetArgOrder asserts the id parses in either position: leading
// (the advertised `user set <id> --flags`) or trailing (flags first). Both must
// yield the same result.
func TestParseUserSetArgOrder(t *testing.T) {
	cases := map[string][]string{
		"id first":    {"alice", "--role", "user"},
		"flags first": {"--role", "user", "alice"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			a, err := parseUserSet(args)
			if err != nil {
				t.Fatalf("parseUserSet(%v): %v", args, err)
			}
			if a.id != "alice" || a.role != store.RoleUser {
				t.Fatalf("got %+v, want id=alice role=user", a)
			}
		})
	}
}

func TestParseUserSetRejectsBadRole(t *testing.T) {
	_, err := parseUserSet([]string{"--role", "superuser", "bob"})
	if err == nil {
		t.Fatal("want error for invalid --role")
	}
	if !strings.Contains(err.Error(), "role") {
		t.Fatalf("error %q should mention role", err)
	}
}

func TestParseUserSetRequiresID(t *testing.T) {
	if _, err := parseUserSet([]string{"--role", "admin"}); err == nil {
		t.Fatal("want error when no id positional given")
	}
}

func TestReadPasswordFromEnv(t *testing.T) {
	env := func(k string) string {
		if k == "RETROSYNC_PASSWORD" {
			return "from-env"
		}
		return ""
	}
	// Stdin holds a different value; env must win and stdin must be untouched.
	pw, err := readPassword(env, strings.NewReader("from-stdin\n"))
	if err != nil {
		t.Fatalf("readPassword: %v", err)
	}
	if pw != "from-env" {
		t.Fatalf("pw = %q, want from-env", pw)
	}
}

func TestReadPasswordFromStdin(t *testing.T) {
	env := func(string) string { return "" }
	pw, err := readPassword(env, strings.NewReader("piped-secret\n"))
	if err != nil {
		t.Fatalf("readPassword: %v", err)
	}
	if pw != "piped-secret" {
		t.Fatalf("pw = %q, want piped-secret", pw)
	}
}

func TestReadPasswordEmptyErrors(t *testing.T) {
	env := func(string) string { return "" }
	if _, err := readPassword(env, strings.NewReader("")); err == nil {
		t.Fatal("want error when no password available")
	}
}

func TestRunUserSetCreatesVerifiableUser(t *testing.T) {
	st := memory.New()
	ctx := context.Background()
	const pw = "couch-coop-password"

	a := userSetArgs{id: "bob", display: "Bob", role: store.RoleAdmin}
	if err := runUserSet(ctx, st, a, pw); err != nil {
		t.Fatalf("runUserSet: %v", err)
	}

	u, err := st.GetUser(ctx, "bob")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.Display != "Bob" || u.Role != store.RoleAdmin {
		t.Fatalf("stored user = %+v", u)
	}
	if u.PwHash == pw || u.PwHash == "" {
		t.Fatal("stored hash is empty or cleartext")
	}
	ok, err := auth.Verify(u.PwHash, pw)
	if err != nil || !ok {
		t.Fatalf("stored hash does not verify: ok=%v err=%v", ok, err)
	}
}

func TestRunUserSetUpsertsExisting(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	if err := runUserSet(ctx, st, userSetArgs{id: "bob", display: "Old", role: store.RoleUser}, "pw1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := runUserSet(ctx, st, userSetArgs{id: "bob", display: "New", role: store.RoleAdmin}, "pw2"); err != nil {
		t.Fatalf("update: %v", err)
	}
	u, err := st.GetUser(ctx, "bob")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.Display != "New" || u.Role != store.RoleAdmin {
		t.Fatalf("upsert did not update: %+v", u)
	}
	if ok, _ := auth.Verify(u.PwHash, "pw2"); !ok {
		t.Fatal("new password does not verify after upsert")
	}
	if ok, _ := auth.Verify(u.PwHash, "pw1"); ok {
		t.Fatal("old password still verifies after upsert")
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	if err := dispatch([]string{"frobnicate"}, testLogger()); err == nil {
		t.Fatal("want error for unknown command")
	}
}
