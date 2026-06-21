package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/a-mcf/retrosync/internal/auth"
	"github.com/a-mcf/retrosync/internal/store"
)

// userUpserter is the slice of store.Store the `user set` command needs. Kept
// narrow so the command is testable with the in-memory store (or a stub)
// without a database.
type userUpserter interface {
	GetUser(ctx context.Context, id string) (store.User, error)
	CreateUser(ctx context.Context, u store.User) error
	UpdateUser(ctx context.Context, u store.User) error
}

// userSetArgs is the parsed `user set` invocation.
type userSetArgs struct {
	id      string
	display string
	role    store.Role
}

// parseUserSet parses the args following `user set`:
//
//	user set <id> [--display NAME] [--role user|admin]
//
// <id> is the positional username. --role defaults to "user" and is validated.
// The password is NOT a flag (argv leaks via ps); it is read by the caller from
// the environment or stdin.
func parseUserSet(args []string) (userSetArgs, error) {
	fs := flag.NewFlagSet("user set", flag.ContinueOnError)
	display := fs.String("display", "", "display name (defaults to the id)")
	role := fs.String("role", string(store.RoleUser), "role: user|admin")

	// Go's flag package stops parsing at the first non-flag argument, so an id
	// in its natural leading position (e.g. `user set alice --role admin`)
	// would leave the trailing flags unparsed. Repeatedly parse, lifting each
	// positional out, so flags and the id may appear in any order.
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return userSetArgs{}, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
	if len(positionals) != 1 {
		return userSetArgs{}, fmt.Errorf("usage: retrosync user set <id> [--display NAME] [--role user|admin]")
	}
	id := strings.TrimSpace(positionals[0])
	if id == "" {
		return userSetArgs{}, errors.New("user id must not be empty")
	}
	r := store.Role(*role)
	if !store.ValidRole(r) {
		return userSetArgs{}, fmt.Errorf("invalid --role %q: want user or admin", *role)
	}
	disp := *display
	if disp == "" {
		disp = id
	}
	return userSetArgs{id: id, display: disp, role: r}, nil
}

// readPassword sources the password WITHOUT touching argv: it prefers
// RETROSYNC_PASSWORD, falling back to a single line read from stdin. Returns an
// error if both are empty.
func readPassword(lookup getenv, stdin io.Reader) (string, error) {
	if pw := lookup("RETROSYNC_PASSWORD"); pw != "" {
		return pw, nil
	}
	sc := bufio.NewScanner(stdin)
	if sc.Scan() {
		pw := strings.TrimRight(sc.Text(), "\r\n")
		if pw != "" {
			return pw, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return "", errors.New("no password: set RETROSYNC_PASSWORD or pipe it on stdin")
}

// runUserSet hashes the password and upserts the user. It creates the user if
// absent, otherwise updates display/role/hash, preserving the id. The password
// is hashed with argon2id; the cleartext never reaches the store or logs.
func runUserSet(ctx context.Context, st userUpserter, a userSetArgs, password string) error {
	hash, err := auth.Hash(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	u := store.User{ID: a.id, Display: a.display, PwHash: hash, Role: a.role}

	_, err = st.GetUser(ctx, a.id)
	switch {
	case err == nil:
		return st.UpdateUser(ctx, u)
	case errors.Is(err, store.ErrNotFound):
		return st.CreateUser(ctx, u)
	default:
		return fmt.Errorf("look up user: %w", err)
	}
}
