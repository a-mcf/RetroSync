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
	UpdateUserProfile(ctx context.Context, id string, display string, role store.Role) error
	UpdateUserPassword(ctx context.Context, id string, pwHash string) error
}

// userSetArgs is the parsed `user set` invocation.
//
// displaySet/roleSet record whether the flag was actually GIVEN, as distinct
// from holding its default. On an existing user the difference matters: --role
// defaults to "user", so applying the default unconditionally would demote an
// admin who ran `user set <id>` intending only to change their password. That is
// the documented recovery path, and on a single-admin install the store would
// then refuse the whole write with ErrLastAdmin — the recovery tool breaking on
// exactly the account it exists to recover.
type userSetArgs struct {
	id         string
	display    string
	displaySet bool
	role       store.Role
	roleSet    bool
}

// parseUserSet parses the args following `user set`:
//
//	user set <id> [--display NAME] [--role user|admin]
//
// <id> is the positional username. --role defaults to "user" and is validated;
// the default applies when CREATING a user, while an omitted flag leaves an
// existing user's role alone (see userSetArgs). The password is NOT a flag (argv
// leaks via ps); it is read by the caller from the environment or stdin.
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
	// Which flags were actually typed, as opposed to sitting at their defaults.
	// fs.Visit walks only the set ones, and accumulates across the repeated
	// Parse calls above.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	return userSetArgs{
		id:         id,
		display:    disp,
		displaySet: given["display"],
		role:       r,
		roleSet:    given["role"],
	}, nil
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

// runUserSet hashes the password and upserts the user. Absent -> create with the
// given (or defaulted) display and role. Present -> set the password, and change
// display/role ONLY where a flag was given. The password is hashed with argon2id;
// the cleartext never reaches the store or logs.
//
// The password must satisfy auth.ValidatePassword — the same rule the web UI's
// create/reset/change forms apply, so the two entry points cannot drift.
//
// The password is written FIRST and separately. This command is the documented
// recovery path, so when a role change is refused (ErrLastAdmin) the password
// must still have been set — you ran it to get back in. The two writes are not
// atomic; the failure message says exactly what did happen.
func runUserSet(ctx context.Context, st userUpserter, a userSetArgs, password string) error {
	if err := auth.ValidatePassword(password); err != nil {
		return err
	}
	hash, err := auth.Hash(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	existing, err := st.GetUser(ctx, a.id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return st.CreateUser(ctx, store.User{
			ID: a.id, Display: a.display, PwHash: hash, Role: a.role,
		})
	case err != nil:
		return fmt.Errorf("look up user: %w", err)
	}

	if err := st.UpdateUserPassword(ctx, a.id, hash); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if !a.displaySet && !a.roleSet {
		return nil
	}

	// Filling the un-flagged field from the row we read is a read-modify-write,
	// with the small window that implies. It is left here deliberately: this is a
	// one-off admin command, run by one operator in a one-shot pod, so there is no
	// concurrent writer to lose to. The web handlers, which DO have concurrent
	// admins, never do this — see internal/web/users.go.
	display, role := existing.Display, existing.Role
	if a.displaySet {
		display = a.display
	}
	if a.roleSet {
		role = a.role
	}
	if err := st.UpdateUserProfile(ctx, a.id, display, role); err != nil {
		if errors.Is(err, store.ErrLastAdmin) {
			return fmt.Errorf("the password for %s was changed, but the role was not: "+
				"%s is the only admin, so demoting them would leave nobody able to "+
				"manage RetroSync. Make someone else an admin first, or re-run without --role", a.id, a.id)
		}
		return fmt.Errorf("set display/role: %w", err)
	}
	return nil
}
