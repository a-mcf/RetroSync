// Package storetest is a shared conformance suite for store.Store
// implementations. Both the in-memory and Postgres stores run it so they are
// held to identical behavior.
//
// Usage:
//
//	storetest.Run(t, func(t *testing.T) store.Store { return memory.New() })
//
// The factory is called once per subtest and must return an empty store.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/store"
)

// Factory returns a fresh, empty Store for a single subtest.
type Factory func(t *testing.T) store.Store

// Run executes the full conformance suite against stores produced by newStore.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(t *testing.T, s store.Store)
	}{
		{"Users", testUsers},
		{"Nodes", testNodes},
		{"Games", testGames},
		{"GamesFilter", testGamesFilter},
		{"GamePaths", testGamePaths},
		{"GamePathUpsert", testGamePathUpsert},
		{"CascadeDeleteGame", testCascadeDeleteGame},
		{"CascadeDeleteNode", testCascadeDeleteNode},
		{"InvalidReference", testInvalidReference},
		{"InvalidValue", testInvalidValue},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, newStore(t))
		})
	}
}

func ctx() context.Context { return context.Background() }

func ptr[T any](v T) *T { return &v }

func testUsers(t *testing.T, s store.Store) {
	c := ctx()
	u := store.User{ID: "bob", Display: "Bob", PwHash: "argon2$x", Role: store.RoleUser}

	if err := s.CreateUser(c, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Duplicate id -> conflict.
	if err := s.CreateUser(c, u); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate CreateUser: want ErrConflict, got %v", err)
	}

	got, err := s.GetUser(c, "bob")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got != u {
		t.Fatalf("GetUser = %+v, want %+v", got, u)
	}

	// Missing -> not found.
	if _, err := s.GetUser(c, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetUser(missing): want ErrNotFound, got %v", err)
	}

	// Update.
	u.Display = "Bobby"
	u.Role = store.RoleAdmin
	if err := s.UpdateUser(c, u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	got, _ = s.GetUser(c, "bob")
	if got.Display != "Bobby" || got.Role != store.RoleAdmin {
		t.Fatalf("UpdateUser not applied: %+v", got)
	}
	// Update missing -> not found.
	if err := s.UpdateUser(c, store.User{ID: "ghost"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateUser(missing): want ErrNotFound, got %v", err)
	}

	// List.
	if err := s.CreateUser(c, store.User{ID: "alice", Display: "Alice", PwHash: "h", Role: store.RoleUser}); err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	list, err := s.ListUsers(c)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListUsers len = %d, want 2", len(list))
	}

	// Delete.
	if err := s.DeleteUser(c, "bob"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.GetUser(c, "bob"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
	if err := s.DeleteUser(c, "bob"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteUser(missing): want ErrNotFound, got %v", err)
	}
}

func testNodes(t *testing.T, s store.Store) {
	c := ctx()
	// Owner user must exist for the FK in Postgres.
	mustUser(t, s, "bob")

	seen := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	n := store.Node{
		ID:          "bob-deck",
		OwnerUserID: ptr("bob"),
		Display:     "Bob's Deck",
		Kind:        store.KindDeck,
		Reach:       store.ReachSyncthingShare,
		ReachConfig: store.ReachConfig{Path: "/srv/syncthing/bob-deck-saves"},
		LastSeenAt:  &seen,
	}
	if err := s.CreateNode(c, n); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if err := s.CreateNode(c, n); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate CreateNode: want ErrConflict, got %v", err)
	}

	got, err := s.GetNode(c, "bob-deck")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.ID != n.ID || got.Display != n.Display || got.Kind != n.Kind ||
		got.Reach != n.Reach || got.ReachConfig != n.ReachConfig {
		t.Fatalf("GetNode mismatch: %+v", got)
	}
	if got.OwnerUserID == nil || *got.OwnerUserID != "bob" {
		t.Fatalf("OwnerUserID = %v, want bob", got.OwnerUserID)
	}
	if got.LastSeenAt == nil || !got.LastSeenAt.Equal(seen) {
		t.Fatalf("LastSeenAt = %v, want %v", got.LastSeenAt, seen)
	}

	// Shared node: nil owner, ssh reach with secret_ref (no cleartext secret).
	mister := store.Node{
		ID:          "living-room-mister",
		OwnerUserID: nil,
		Display:     "Living Room MiSTer",
		Kind:        store.KindMister,
		Reach:       store.ReachSSH,
		ReachConfig: store.ReachConfig{Host: "172.16.7.12", User: "root", SecretRef: "mister-1"},
	}
	if err := s.CreateNode(c, mister); err != nil {
		t.Fatalf("CreateNode mister: %v", err)
	}
	gm, err := s.GetNode(c, "living-room-mister")
	if err != nil {
		t.Fatalf("GetNode mister: %v", err)
	}
	if gm.OwnerUserID != nil {
		t.Fatalf("shared node OwnerUserID = %v, want nil", gm.OwnerUserID)
	}
	if gm.ReachConfig.SecretRef != "mister-1" {
		t.Fatalf("SecretRef = %q, want mister-1", gm.ReachConfig.SecretRef)
	}

	if _, err := s.GetNode(c, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetNode(missing): want ErrNotFound, got %v", err)
	}

	// Update: clear last_seen, change display.
	n.Display = "Bob Deck 2"
	n.LastSeenAt = nil
	if err := s.UpdateNode(c, n); err != nil {
		t.Fatalf("UpdateNode: %v", err)
	}
	got, _ = s.GetNode(c, "bob-deck")
	if got.Display != "Bob Deck 2" || got.LastSeenAt != nil {
		t.Fatalf("UpdateNode not applied: %+v", got)
	}
	if err := s.UpdateNode(c, store.Node{ID: "ghost", Kind: store.KindGeneric, Reach: store.ReachSSH}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateNode(missing): want ErrNotFound, got %v", err)
	}

	list, err := s.ListNodes(c)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListNodes len = %d, want 2", len(list))
	}

	if err := s.DeleteNode(c, "bob-deck"); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	if err := s.DeleteNode(c, "bob-deck"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteNode(missing): want ErrNotFound, got %v", err)
	}
}

func testGames(t *testing.T, s store.Store) {
	c := ctx()
	g := store.Game{ID: "super-metroid", Display: "Super Metroid", System: "snes", Notes: ""}
	if err := s.CreateGame(c, g); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	if err := s.CreateGame(c, g); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate CreateGame: want ErrConflict, got %v", err)
	}
	got, err := s.GetGame(c, "super-metroid")
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if got != g {
		t.Fatalf("GetGame = %+v, want %+v", got, g)
	}
	if _, err := s.GetGame(c, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetGame(missing): want ErrNotFound, got %v", err)
	}
	g.Notes = "100% run"
	if err := s.UpdateGame(c, g); err != nil {
		t.Fatalf("UpdateGame: %v", err)
	}
	got, _ = s.GetGame(c, "super-metroid")
	if got.Notes != "100% run" {
		t.Fatalf("UpdateGame not applied: %+v", got)
	}
	if err := s.UpdateGame(c, store.Game{ID: "ghost"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateGame(missing): want ErrNotFound, got %v", err)
	}
	if err := s.DeleteGame(c, "super-metroid"); err != nil {
		t.Fatalf("DeleteGame: %v", err)
	}
	if err := s.DeleteGame(c, "super-metroid"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteGame(missing): want ErrNotFound, got %v", err)
	}
}

func testGamesFilter(t *testing.T, s store.Store) {
	c := ctx()
	games := []store.Game{
		{ID: "super-metroid", Display: "Super Metroid", System: "snes"},
		{ID: "super-mario-64", Display: "Super Mario 64", System: "n64"},
		{ID: "zelda-oot", Display: "Ocarina of Time", System: "n64"},
	}
	for _, g := range games {
		if err := s.CreateGame(c, g); err != nil {
			t.Fatalf("CreateGame %s: %v", g.ID, err)
		}
	}

	all, err := s.ListGames(c, store.GameFilter{})
	if err != nil {
		t.Fatalf("ListGames(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListGames(all) len = %d, want 3", len(all))
	}

	// System filter.
	n64, _ := s.ListGames(c, store.GameFilter{System: "n64"})
	if len(n64) != 2 {
		t.Fatalf("ListGames(n64) len = %d, want 2", len(n64))
	}

	// Substring filter (case-insensitive), matches display + id.
	sup, _ := s.ListGames(c, store.GameFilter{Q: "SUPER"})
	if len(sup) != 2 {
		t.Fatalf("ListGames(q=SUPER) len = %d, want 2", len(sup))
	}

	// Combined.
	combo, _ := s.ListGames(c, store.GameFilter{Q: "super", System: "n64"})
	if len(combo) != 1 || combo[0].ID != "super-mario-64" {
		t.Fatalf("ListGames(combo) = %+v, want only super-mario-64", combo)
	}

	// Substring on id only.
	byID, _ := s.ListGames(c, store.GameFilter{Q: "oot"})
	if len(byID) != 1 || byID[0].ID != "zelda-oot" {
		t.Fatalf("ListGames(q=oot) = %+v, want zelda-oot", byID)
	}
}

func testGamePaths(t *testing.T, s store.Store) {
	c := ctx()
	mustUser(t, s, "bob")
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", ptr("bob"))
	mustNode(t, s, "mister", nil)

	gp := store.GamePath{GameID: "super-metroid", NodeID: "bob-deck", Path: "retroarch/saves/Super Metroid.srm"}
	if err := s.SetGamePath(c, gp); err != nil {
		t.Fatalf("SetGamePath: %v", err)
	}
	got, err := s.GetGamePath(c, "super-metroid", "bob-deck")
	if err != nil {
		t.Fatalf("GetGamePath: %v", err)
	}
	if got != gp {
		t.Fatalf("GetGamePath = %+v, want %+v", got, gp)
	}
	if _, err := s.GetGamePath(c, "super-metroid", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetGamePath(missing): want ErrNotFound, got %v", err)
	}

	gp2 := store.GamePath{GameID: "super-metroid", NodeID: "mister", Path: "SNES/Super Metroid.sav"}
	if err := s.SetGamePath(c, gp2); err != nil {
		t.Fatalf("SetGamePath 2: %v", err)
	}

	byGame, err := s.ListGamePathsByGame(c, "super-metroid")
	if err != nil {
		t.Fatalf("ListGamePathsByGame: %v", err)
	}
	if len(byGame) != 2 {
		t.Fatalf("ListGamePathsByGame len = %d, want 2", len(byGame))
	}

	byNode, err := s.ListGamePathsByNode(c, "bob-deck")
	if err != nil {
		t.Fatalf("ListGamePathsByNode: %v", err)
	}
	if len(byNode) != 1 || byNode[0].NodeID != "bob-deck" {
		t.Fatalf("ListGamePathsByNode = %+v, want one bob-deck row", byNode)
	}

	if err := s.DeleteGamePath(c, "super-metroid", "bob-deck"); err != nil {
		t.Fatalf("DeleteGamePath: %v", err)
	}
	if err := s.DeleteGamePath(c, "super-metroid", "bob-deck"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteGamePath(missing): want ErrNotFound, got %v", err)
	}
}

func testGamePathUpsert(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)

	gp := store.GamePath{GameID: "super-metroid", NodeID: "bob-deck", Path: "old/path.srm"}
	if err := s.SetGamePath(c, gp); err != nil {
		t.Fatalf("SetGamePath: %v", err)
	}
	// Upsert: same PK, new path. Must not conflict; must overwrite.
	gp.Path = "new/path.srm"
	if err := s.SetGamePath(c, gp); err != nil {
		t.Fatalf("SetGamePath(upsert): %v", err)
	}
	got, _ := s.GetGamePath(c, "super-metroid", "bob-deck")
	if got.Path != "new/path.srm" {
		t.Fatalf("upsert path = %q, want new/path.srm", got.Path)
	}
	// Still exactly one row.
	rows, _ := s.ListGamePathsByGame(c, "super-metroid")
	if len(rows) != 1 {
		t.Fatalf("after upsert len = %d, want 1", len(rows))
	}
}

func testCascadeDeleteGame(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	mustNode(t, s, "mister", nil)
	must(t, s.SetGamePath(c, store.GamePath{GameID: "super-metroid", NodeID: "bob-deck", Path: "a"}))
	must(t, s.SetGamePath(c, store.GamePath{GameID: "super-metroid", NodeID: "mister", Path: "b"}))

	if err := s.DeleteGame(c, "super-metroid"); err != nil {
		t.Fatalf("DeleteGame: %v", err)
	}
	rows, _ := s.ListGamePathsByGame(c, "super-metroid")
	if len(rows) != 0 {
		t.Fatalf("game_paths after game delete = %d, want 0 (cascade)", len(rows))
	}
	// Node-side listing also empty for those.
	bn, _ := s.ListGamePathsByNode(c, "bob-deck")
	if len(bn) != 0 {
		t.Fatalf("ListGamePathsByNode after cascade = %d, want 0", len(bn))
	}
}

func testCascadeDeleteNode(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustGame(t, s, "zelda")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.SetGamePath(c, store.GamePath{GameID: "super-metroid", NodeID: "bob-deck", Path: "a"}))
	must(t, s.SetGamePath(c, store.GamePath{GameID: "zelda", NodeID: "bob-deck", Path: "b"}))

	if err := s.DeleteNode(c, "bob-deck"); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	rows, _ := s.ListGamePathsByNode(c, "bob-deck")
	if len(rows) != 0 {
		t.Fatalf("game_paths after node delete = %d, want 0 (cascade)", len(rows))
	}
	// Game-side listing also empty.
	g, _ := s.ListGamePathsByGame(c, "super-metroid")
	if len(g) != 0 {
		t.Fatalf("ListGamePathsByGame after node cascade = %d, want 0", len(g))
	}
}

// testInvalidReference asserts both stores reject writes that name a missing
// parent row with ErrInvalidReference (FK enforcement).
func testInvalidReference(t *testing.T, s store.Store) {
	c := ctx()

	// CreateNode with a non-existent owner_user_id.
	err := s.CreateNode(c, store.Node{
		ID: "orphan-node", OwnerUserID: ptr("ghost-user"), Display: "Orphan",
		Kind: store.KindGeneric, Reach: store.ReachSSH,
		ReachConfig: store.ReachConfig{Host: "h", User: "u", SecretRef: "r"},
	})
	if !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("CreateNode(bad owner): want ErrInvalidReference, got %v", err)
	}

	// SetGamePath with a missing game (node exists).
	mustNode(t, s, "real-node", nil)
	err = s.SetGamePath(c, store.GamePath{GameID: "ghost-game", NodeID: "real-node", Path: "p"})
	if !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("SetGamePath(missing game): want ErrInvalidReference, got %v", err)
	}

	// SetGamePath with a missing node (game exists).
	mustGame(t, s, "real-game")
	err = s.SetGamePath(c, store.GamePath{GameID: "real-game", NodeID: "ghost-node", Path: "p"})
	if !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("SetGamePath(missing node): want ErrInvalidReference, got %v", err)
	}
}

// testInvalidValue asserts both stores reject out-of-domain enum values for
// role, kind, and reach with ErrInvalidValue (CHECK enforcement).
func testInvalidValue(t *testing.T, s store.Store) {
	c := ctx()

	// Bad role.
	err := s.CreateUser(c, store.User{ID: "u1", Display: "U", PwHash: "h", Role: store.Role("superuser")})
	if !errors.Is(err, store.ErrInvalidValue) {
		t.Fatalf("CreateUser(bad role): want ErrInvalidValue, got %v", err)
	}

	// A valid owner so the node inserts below fail on the enum, not the FK.
	mustUser(t, s, "owner")

	// Bad kind.
	err = s.CreateNode(c, store.Node{
		ID: "n1", OwnerUserID: ptr("owner"), Display: "N",
		Kind: store.Kind("toaster"), Reach: store.ReachSSH,
		ReachConfig: store.ReachConfig{Host: "h", User: "u", SecretRef: "r"},
	})
	if !errors.Is(err, store.ErrInvalidValue) {
		t.Fatalf("CreateNode(bad kind): want ErrInvalidValue, got %v", err)
	}

	// Bad reach.
	err = s.CreateNode(c, store.Node{
		ID: "n2", OwnerUserID: ptr("owner"), Display: "N",
		Kind: store.KindGeneric, Reach: store.Reach("carrier-pigeon"),
		ReachConfig: store.ReachConfig{},
	})
	if !errors.Is(err, store.ErrInvalidValue) {
		t.Fatalf("CreateNode(bad reach): want ErrInvalidValue, got %v", err)
	}
}

// ---- helpers ----

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func mustUser(t *testing.T, s store.Store, id string) {
	t.Helper()
	must(t, s.CreateUser(ctx(), store.User{ID: id, Display: id, PwHash: "h", Role: store.RoleUser}))
}

func mustGame(t *testing.T, s store.Store, id string) {
	t.Helper()
	must(t, s.CreateGame(ctx(), store.Game{ID: id, Display: id, System: "snes"}))
}

func mustNode(t *testing.T, s store.Store, id string, owner *string) {
	t.Helper()
	if owner != nil {
		// Tolerate an owner the caller already created.
		if err := s.CreateUser(ctx(), store.User{ID: *owner, Display: *owner, PwHash: "h", Role: store.RoleUser}); err != nil && !errors.Is(err, store.ErrConflict) {
			t.Fatalf("setup owner %q: %v", *owner, err)
		}
	}
	must(t, s.CreateNode(ctx(), store.Node{
		ID:          id,
		OwnerUserID: owner,
		Display:     id,
		Kind:        store.KindGeneric,
		Reach:       store.ReachSSH,
		ReachConfig: store.ReachConfig{Host: "h", User: "u", SecretRef: "ref"},
	}))
}
