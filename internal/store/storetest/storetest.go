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
		{"Bindings", testBindings},
		{"BindingInvalidReference", testBindingInvalidReference},
		{"BindingInvalidValue", testBindingInvalidValue},
		{"BindingDeleteIdempotent", testBindingDeleteIdempotent},
		{"BindingConflictFlag", testBindingConflictFlag},
		{"SyncLog", testSyncLog},
		{"SyncLogOrderByTS", testSyncLogOrderByTS},
		{"SyncLogInvalidValue", testSyncLogInvalidValue},
		{"SyncLogInvalidReference", testSyncLogInvalidReference},
		{"Manifest", testManifest},
		{"DeleteBlockedByActiveBinding", testDeleteBlockedByActiveBinding},
		{"CascadeRuntimeOnGameDelete", testCascadeRuntimeOnGameDelete},
		{"CascadeManifestOnNodeDelete", testCascadeManifestOnNodeDelete},
		{"Syncs", testSyncs},
		{"SyncInvalidReference", testSyncInvalidReference},
		{"SyncMembers", testSyncMembers},
		{"SyncMemberUniquePathInvariant", testSyncMemberUniquePathInvariant},
		{"SyncMemberInvalidReference", testSyncMemberInvalidReference},
		{"SyncCascades", testSyncCascades},
		{"SyncMembersByNode", testSyncMembersByNode},
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

// ---- runtime: active_bindings ----

func testBindings(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	mustNode(t, s, "mister", nil)

	b := store.ActiveBinding{
		GameID:      "super-metroid",
		PrimaryNode: "bob-deck",
		Direction:   "from-primary",
	}
	if err := s.CreateBinding(c, b); err != nil {
		t.Fatalf("CreateBinding: %v", err)
	}
	// Duplicate game_id -> conflict (one active session per game invariant).
	if err := s.CreateBinding(c, store.ActiveBinding{
		GameID: "super-metroid", PrimaryNode: "mister", Direction: "from-primary",
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate CreateBinding: want ErrConflict, got %v", err)
	}

	got, err := s.GetBinding(c, "super-metroid")
	if err != nil {
		t.Fatalf("GetBinding: %v", err)
	}
	if got.GameID != "super-metroid" || got.PrimaryNode != "bob-deck" ||
		got.Direction != "from-primary" {
		t.Fatalf("GetBinding mismatch: %+v", got)
	}
	// peer_scope defaults to all-configured.
	if got.PeerScope != "all-configured" {
		t.Fatalf("PeerScope default = %q, want all-configured", got.PeerScope)
	}
	// started_at defaulted to a real time.
	if got.StartedAt.IsZero() {
		t.Fatalf("StartedAt is zero, want a default now()")
	}
	if got.ConflictAt != nil || got.LastSynced != nil {
		t.Fatalf("new binding should have nil conflict_at/last_synced: %+v", got)
	}

	if _, err := s.GetBinding(c, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetBinding(missing): want ErrNotFound, got %v", err)
	}

	// A peer-direction binding for a second game (and explicit peer_scope).
	mustGame(t, s, "zelda")
	if err := s.CreateBinding(c, store.ActiveBinding{
		GameID: "zelda", PrimaryNode: "mister", Direction: "from-peer-bob-deck",
		PeerScope: "bob-deck,mister",
	}); err != nil {
		t.Fatalf("CreateBinding(peer direction): %v", err)
	}
	list, err := s.ListBindings(c)
	if err != nil {
		t.Fatalf("ListBindings: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListBindings len = %d, want 2", len(list))
	}
	if list[0].GameID != "super-metroid" || list[1].GameID != "zelda" {
		t.Fatalf("ListBindings not ordered by game_id: %+v", list)
	}

	// Update mutable fields.
	synced := time.Date(2026, 6, 21, 14, 0, 0, 0, time.UTC)
	got.PrimaryNode = "mister"
	got.Direction = "from-peer-mister"
	got.LastSynced = &synced
	if err := s.UpdateBinding(c, got); err != nil {
		t.Fatalf("UpdateBinding: %v", err)
	}
	reread, _ := s.GetBinding(c, "super-metroid")
	if reread.PrimaryNode != "mister" || reread.Direction != "from-peer-mister" {
		t.Fatalf("UpdateBinding not applied: %+v", reread)
	}
	if reread.LastSynced == nil || !reread.LastSynced.Equal(synced) {
		t.Fatalf("UpdateBinding last_synced = %v, want %v", reread.LastSynced, synced)
	}

	// Update missing -> not found.
	if err := s.UpdateBinding(c, store.ActiveBinding{
		GameID: "ghost", PrimaryNode: "mister", Direction: "from-primary",
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateBinding(missing): want ErrNotFound, got %v", err)
	}

	// Delete returns the game to idle.
	if err := s.DeleteBinding(c, "super-metroid"); err != nil {
		t.Fatalf("DeleteBinding: %v", err)
	}
	if _, err := s.GetBinding(c, "super-metroid"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after DeleteBinding: want ErrNotFound, got %v", err)
	}
}

func testBindingInvalidReference(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)

	// Missing game.
	if err := s.CreateBinding(c, store.ActiveBinding{
		GameID: "ghost-game", PrimaryNode: "bob-deck", Direction: "from-primary",
	}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("CreateBinding(missing game): want ErrInvalidReference, got %v", err)
	}
	// Missing node.
	if err := s.CreateBinding(c, store.ActiveBinding{
		GameID: "super-metroid", PrimaryNode: "ghost-node", Direction: "from-primary",
	}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("CreateBinding(missing node): want ErrInvalidReference, got %v", err)
	}
}

func testBindingInvalidValue(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)

	// Bad direction (neither from-primary nor from-peer-*).
	if err := s.CreateBinding(c, store.ActiveBinding{
		GameID: "super-metroid", PrimaryNode: "bob-deck", Direction: "newer-wins",
	}); !errors.Is(err, store.ErrInvalidValue) {
		t.Fatalf("CreateBinding(bad direction): want ErrInvalidValue, got %v", err)
	}

	// A valid binding, then an update to a bad direction.
	must(t, s.CreateBinding(c, store.ActiveBinding{
		GameID: "super-metroid", PrimaryNode: "bob-deck", Direction: "from-primary",
	}))
	if err := s.UpdateBinding(c, store.ActiveBinding{
		GameID: "super-metroid", PrimaryNode: "bob-deck", Direction: "garbage",
	}); !errors.Is(err, store.ErrInvalidValue) {
		t.Fatalf("UpdateBinding(bad direction): want ErrInvalidValue, got %v", err)
	}
}

func testBindingDeleteIdempotent(t *testing.T, s store.Store) {
	c := ctx()
	// Deleting an absent binding is a no-op (api.md: deactivate is idempotent).
	if err := s.DeleteBinding(c, "never-bound"); err != nil {
		t.Fatalf("DeleteBinding(absent): want nil, got %v", err)
	}

	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.CreateBinding(c, store.ActiveBinding{
		GameID: "super-metroid", PrimaryNode: "bob-deck", Direction: "from-primary",
	}))
	if err := s.DeleteBinding(c, "super-metroid"); err != nil {
		t.Fatalf("DeleteBinding: %v", err)
	}
	// Second delete is still a no-op, not ErrNotFound.
	if err := s.DeleteBinding(c, "super-metroid"); err != nil {
		t.Fatalf("DeleteBinding(repeat): want nil, got %v", err)
	}
}

func testBindingConflictFlag(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.CreateBinding(c, store.ActiveBinding{
		GameID: "super-metroid", PrimaryNode: "bob-deck", Direction: "from-primary",
	}))

	b, _ := s.GetBinding(c, "super-metroid")
	if b.ConflictAt != nil {
		t.Fatalf("new binding conflict_at = %v, want nil", b.ConflictAt)
	}

	// Set conflict_at.
	conflictTS := time.Date(2026, 6, 21, 15, 30, 0, 0, time.UTC)
	b.ConflictAt = &conflictTS
	if err := s.UpdateBinding(c, b); err != nil {
		t.Fatalf("UpdateBinding(set conflict): %v", err)
	}
	got, _ := s.GetBinding(c, "super-metroid")
	if got.ConflictAt == nil || !got.ConflictAt.Equal(conflictTS) {
		t.Fatalf("conflict_at = %v, want %v", got.ConflictAt, conflictTS)
	}

	// Clear conflict_at (resolution).
	got.ConflictAt = nil
	if err := s.UpdateBinding(c, got); err != nil {
		t.Fatalf("UpdateBinding(clear conflict): %v", err)
	}
	cleared, _ := s.GetBinding(c, "super-metroid")
	if cleared.ConflictAt != nil {
		t.Fatalf("conflict_at after clear = %v, want nil", cleared.ConflictAt)
	}
}

// ---- runtime: sync_log ----

func testSyncLog(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustGame(t, s, "zelda")

	bytes := int64(2048)
	src := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	dst := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)

	// Three entries for super-metroid, appended oldest-first.
	must(t, s.AppendLog(c, store.LogEntry{
		GameID: "super-metroid", FromNode: "bob-deck", ToNode: "mister",
		Bytes: &bytes, SrcMtime: &src, DstMtime: &dst,
		Outcome: store.OutcomeOK, Message: "",
	}))
	must(t, s.AppendLog(c, store.LogEntry{
		GameID: "super-metroid", FromNode: "bob-deck", ToNode: "alice-deck",
		Outcome: store.OutcomeNoop,
	}))
	must(t, s.AppendLog(c, store.LogEntry{
		GameID: "super-metroid", Outcome: store.OutcomeError, Message: "boom",
	}))
	// An entry for a different game must not leak into the listing.
	must(t, s.AppendLog(c, store.LogEntry{GameID: "zelda", Outcome: store.OutcomeOK}))

	// Most-recent-first: the error entry (appended last) comes first.
	all, err := s.ListLogByGame(c, "super-metroid", 0)
	if err != nil {
		t.Fatalf("ListLogByGame: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListLogByGame len = %d, want 3", len(all))
	}
	if all[0].Outcome != store.OutcomeError || all[0].Message != "boom" {
		t.Fatalf("newest entry = %+v, want the error entry", all[0])
	}
	if all[2].Outcome != store.OutcomeOK || all[2].ToNode != "mister" {
		t.Fatalf("oldest entry = %+v, want the first ok entry", all[2])
	}
	// Round-trip of detail fields on the oldest entry.
	if all[2].Bytes == nil || *all[2].Bytes != bytes {
		t.Fatalf("Bytes = %v, want %d", all[2].Bytes, bytes)
	}
	if all[2].SrcMtime == nil || !all[2].SrcMtime.Equal(src) {
		t.Fatalf("SrcMtime = %v, want %v", all[2].SrcMtime, src)
	}
	if all[2].DstMtime == nil || !all[2].DstMtime.Equal(dst) {
		t.Fatalf("DstMtime = %v, want %v", all[2].DstMtime, dst)
	}
	// ids are assigned and distinct.
	if all[0].ID == 0 || all[0].ID == all[1].ID {
		t.Fatalf("ids not assigned/distinct: %+v", all)
	}

	// Limit caps the result, keeping most-recent-first.
	limited, err := s.ListLogByGame(c, "super-metroid", 2)
	if err != nil {
		t.Fatalf("ListLogByGame(limit): %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("ListLogByGame(limit=2) len = %d, want 2", len(limited))
	}
	if limited[0].Outcome != store.OutcomeError {
		t.Fatalf("limited newest = %+v, want error entry", limited[0])
	}
}

// testSyncLogOrderByTS asserts ListLogByGame orders by ts DESC (ties broken by
// id DESC), independent of insertion order. Clock skew is a core concern in
// this project (RTC-less MiSTer, drifting Anbernics), so a caller may supply a
// ts that does not rise monotonically with append order. We insert entries
// whose ts order deliberately disagrees with their id order and check both
// impls return the same ts-then-id ordering.
func testSyncLogOrderByTS(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")

	t1 := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) // earliest
	t2 := time.Date(2026, 6, 21, 13, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 6, 21, 14, 0, 0, 0, time.UTC) // latest

	// Append in an order where insertion (id) order != ts order:
	//   id=1 -> t3 (latest), id=2 -> t1 (earliest), id=3 -> t2 (middle).
	// Relying on append order would yield id=3,2,1; the ts contract yields
	// id=1 (t3), id=3 (t2), id=2 (t1).
	must(t, s.AppendLog(c, store.LogEntry{GameID: "super-metroid", TS: t3, Message: "id1-t3", Outcome: store.OutcomeOK}))
	must(t, s.AppendLog(c, store.LogEntry{GameID: "super-metroid", TS: t1, Message: "id2-t1", Outcome: store.OutcomeOK}))
	must(t, s.AppendLog(c, store.LogEntry{GameID: "super-metroid", TS: t2, Message: "id3-t2", Outcome: store.OutcomeOK}))

	got, err := s.ListLogByGame(c, "super-metroid", 0)
	if err != nil {
		t.Fatalf("ListLogByGame: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListLogByGame len = %d, want 3", len(got))
	}

	wantTS := []time.Time{t3, t2, t1}
	for i, w := range wantTS {
		if !got[i].TS.Equal(w) {
			t.Fatalf("entry %d ts = %v, want %v (order: %q,%q,%q)",
				i, got[i].TS, w, got[0].Message, got[1].Message, got[2].Message)
		}
	}

	// Tie-break by id DESC: two entries sharing one ts must come back
	// highest-id-first.
	mustGame(t, s, "zelda")
	tie := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	must(t, s.AppendLog(c, store.LogEntry{GameID: "zelda", TS: tie, Message: "first", Outcome: store.OutcomeOK}))
	must(t, s.AppendLog(c, store.LogEntry{GameID: "zelda", TS: tie, Message: "second", Outcome: store.OutcomeOK}))

	ties, err := s.ListLogByGame(c, "zelda", 0)
	if err != nil {
		t.Fatalf("ListLogByGame(zelda): %v", err)
	}
	if len(ties) != 2 {
		t.Fatalf("ListLogByGame(zelda) len = %d, want 2", len(ties))
	}
	if ties[0].ID <= ties[1].ID {
		t.Fatalf("tie-break not id DESC: got ids %d then %d", ties[0].ID, ties[1].ID)
	}
	if ties[0].Message != "second" || ties[1].Message != "first" {
		t.Fatalf("tie-break order = %q,%q, want second,first", ties[0].Message, ties[1].Message)
	}
}

func testSyncLogInvalidValue(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	if err := s.AppendLog(c, store.LogEntry{
		GameID: "super-metroid", Outcome: store.Outcome("exploded"),
	}); !errors.Is(err, store.ErrInvalidValue) {
		t.Fatalf("AppendLog(bad outcome): want ErrInvalidValue, got %v", err)
	}
}

func testSyncLogInvalidReference(t *testing.T, s store.Store) {
	c := ctx()
	if err := s.AppendLog(c, store.LogEntry{
		GameID: "ghost-game", Outcome: store.OutcomeOK,
	}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("AppendLog(missing game): want ErrInvalidReference, got %v", err)
	}
}

// ---- runtime: manifest ----

func testManifest(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	mustNode(t, s, "mister", nil)

	mtime := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	size := int64(512)
	checked := time.Date(2026, 6, 21, 9, 0, 30, 0, time.UTC)
	m := store.ManifestEntry{
		GameID: "super-metroid", NodeID: "bob-deck",
		Mtime: &mtime, Size: &size, LastChecked: &checked,
	}
	if err := s.SetManifest(c, m); err != nil {
		t.Fatalf("SetManifest: %v", err)
	}
	got, err := s.GetManifest(c, "super-metroid", "bob-deck")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if got.Mtime == nil || !got.Mtime.Equal(mtime) || got.Size == nil || *got.Size != size {
		t.Fatalf("GetManifest mismatch: %+v", got)
	}
	if got.SHA256 != nil {
		t.Fatalf("SHA256 = %v, want nil (lazy)", got.SHA256)
	}

	if _, err := s.GetManifest(c, "super-metroid", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetManifest(missing): want ErrNotFound, got %v", err)
	}

	// Upsert: same PK, new mtime/size and a now-computed sha256.
	mtime2 := mtime.Add(time.Hour)
	size2 := int64(1024)
	sha := "abc123"
	m.Mtime, m.Size, m.SHA256 = &mtime2, &size2, &sha
	if err := s.SetManifest(c, m); err != nil {
		t.Fatalf("SetManifest(upsert): %v", err)
	}
	got, _ = s.GetManifest(c, "super-metroid", "bob-deck")
	if got.Mtime == nil || !got.Mtime.Equal(mtime2) || got.Size == nil || *got.Size != size2 {
		t.Fatalf("upsert not applied: %+v", got)
	}
	if got.SHA256 == nil || *got.SHA256 != sha {
		t.Fatalf("upsert sha256 = %v, want %q", got.SHA256, sha)
	}

	// Second node, then list by game.
	must(t, s.SetManifest(c, store.ManifestEntry{GameID: "super-metroid", NodeID: "mister"}))
	list, err := s.ListManifestByGame(c, "super-metroid")
	if err != nil {
		t.Fatalf("ListManifestByGame: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListManifestByGame len = %d, want 2", len(list))
	}
	if list[0].NodeID != "bob-deck" || list[1].NodeID != "mister" {
		t.Fatalf("ListManifestByGame not ordered by node_id: %+v", list)
	}

	// Missing game/node -> invalid reference.
	if err := s.SetManifest(c, store.ManifestEntry{GameID: "ghost", NodeID: "bob-deck"}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("SetManifest(missing game): want ErrInvalidReference, got %v", err)
	}
	if err := s.SetManifest(c, store.ManifestEntry{GameID: "super-metroid", NodeID: "ghost"}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("SetManifest(missing node): want ErrInvalidReference, got %v", err)
	}
}

// ---- runtime: cross-cutting delete behavior ----

// testDeleteBlockedByActiveBinding asserts both impls refuse to delete a game
// or node that is referenced by an active binding (api.md "delete forbidden if
// active"; the active_bindings NO-ACTION FKs in Postgres, an explicit check in
// memory).
func testDeleteBlockedByActiveBinding(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.CreateBinding(c, store.ActiveBinding{
		GameID: "super-metroid", PrimaryNode: "bob-deck", Direction: "from-primary",
	}))

	if err := s.DeleteGame(c, "super-metroid"); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("DeleteGame(active): want ErrInvalidReference, got %v", err)
	}
	if err := s.DeleteNode(c, "bob-deck"); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("DeleteNode(active primary): want ErrInvalidReference, got %v", err)
	}

	// After deactivation, both deletes succeed.
	must(t, s.DeleteBinding(c, "super-metroid"))
	if err := s.DeleteNode(c, "bob-deck"); err != nil {
		t.Fatalf("DeleteNode after unbind: %v", err)
	}
	if err := s.DeleteGame(c, "super-metroid"); err != nil {
		t.Fatalf("DeleteGame after unbind: %v", err)
	}
}

// testCascadeRuntimeOnGameDelete asserts manifest and sync_log rows cascade
// when a game with no active binding is deleted.
func testCascadeRuntimeOnGameDelete(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.SetManifest(c, store.ManifestEntry{GameID: "super-metroid", NodeID: "bob-deck"}))
	must(t, s.AppendLog(c, store.LogEntry{GameID: "super-metroid", Outcome: store.OutcomeOK}))

	// No active binding -> delete succeeds and cascades runtime rows.
	if err := s.DeleteGame(c, "super-metroid"); err != nil {
		t.Fatalf("DeleteGame: %v", err)
	}
	man, _ := s.ListManifestByGame(c, "super-metroid")
	if len(man) != 0 {
		t.Fatalf("manifest after game delete = %d, want 0 (cascade)", len(man))
	}
	logs, _ := s.ListLogByGame(c, "super-metroid", 0)
	if len(logs) != 0 {
		t.Fatalf("sync_log after game delete = %d, want 0 (cascade)", len(logs))
	}
}

// testCascadeManifestOnNodeDelete asserts manifest rows cascade when a node is
// deleted (sync_log from/to_node are unconstrained text, so they do not block
// or cascade).
func testCascadeManifestOnNodeDelete(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.SetManifest(c, store.ManifestEntry{GameID: "super-metroid", NodeID: "bob-deck"}))

	if err := s.DeleteNode(c, "bob-deck"); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	man, _ := s.ListManifestByGame(c, "super-metroid")
	if len(man) != 0 {
		t.Fatalf("manifest after node delete = %d, want 0 (cascade)", len(man))
	}
}

// ---- syncs / sync_members ----

func testSyncs(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")

	sy := store.Sync{ID: "sm-bob", GameID: "super-metroid", Name: "Bob's stream"}
	if err := s.CreateSync(c, sy); err != nil {
		t.Fatalf("CreateSync: %v", err)
	}
	// Duplicate id -> conflict.
	if err := s.CreateSync(c, sy); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate CreateSync: want ErrConflict, got %v", err)
	}

	got, err := s.GetSync(c, "sm-bob")
	if err != nil {
		t.Fatalf("GetSync: %v", err)
	}
	if got != sy {
		t.Fatalf("GetSync = %+v, want %+v", got, sy)
	}
	if _, err := s.GetSync(c, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSync(missing): want ErrNotFound, got %v", err)
	}

	// A second, independent sync for the SAME game (the multi-sync case).
	must(t, s.CreateSync(c, store.Sync{ID: "sm-alice", GameID: "super-metroid", Name: "Alice's stream"}))
	// A sync for a different game must not leak into the listing.
	mustGame(t, s, "zelda")
	must(t, s.CreateSync(c, store.Sync{ID: "z-1", GameID: "zelda"}))

	list, err := s.ListSyncsByGame(c, "super-metroid")
	if err != nil {
		t.Fatalf("ListSyncsByGame: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListSyncsByGame len = %d, want 2", len(list))
	}
	if list[0].ID != "sm-alice" || list[1].ID != "sm-bob" {
		t.Fatalf("ListSyncsByGame not ordered by id: %+v", list)
	}

	// Update mutable fields.
	sy.Name = "Bob renamed"
	if err := s.UpdateSync(c, sy); err != nil {
		t.Fatalf("UpdateSync: %v", err)
	}
	reread, _ := s.GetSync(c, "sm-bob")
	if reread.Name != "Bob renamed" {
		t.Fatalf("UpdateSync not applied: %+v", reread)
	}
	// Update missing -> not found.
	if err := s.UpdateSync(c, store.Sync{ID: "ghost", GameID: "super-metroid"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateSync(missing): want ErrNotFound, got %v", err)
	}

	// Delete; second delete -> not found (not idempotent, like other deletes).
	if err := s.DeleteSync(c, "sm-bob"); err != nil {
		t.Fatalf("DeleteSync: %v", err)
	}
	if err := s.DeleteSync(c, "sm-bob"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteSync(missing): want ErrNotFound, got %v", err)
	}
}

func testSyncInvalidReference(t *testing.T, s store.Store) {
	c := ctx()
	// CreateSync naming a missing game.
	if err := s.CreateSync(c, store.Sync{ID: "orphan", GameID: "ghost-game"}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("CreateSync(missing game): want ErrInvalidReference, got %v", err)
	}
	// UpdateSync re-pointing to a missing game.
	mustGame(t, s, "super-metroid")
	must(t, s.CreateSync(c, store.Sync{ID: "sm", GameID: "super-metroid"}))
	if err := s.UpdateSync(c, store.Sync{ID: "sm", GameID: "ghost-game"}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("UpdateSync(missing game): want ErrInvalidReference, got %v", err)
	}
}

func testSyncMembers(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	mustNode(t, s, "bob-mister", nil)
	must(t, s.CreateSync(c, store.Sync{ID: "sm-bob", GameID: "super-metroid"}))

	m := store.SyncMember{SyncID: "sm-bob", NodeID: "bob-deck", Path: "retroarch/saves/Super Metroid.srm"}
	if err := s.SetSyncMember(c, m); err != nil {
		t.Fatalf("SetSyncMember: %v", err)
	}
	got, err := s.GetSyncMember(c, "sm-bob", "bob-deck")
	if err != nil {
		t.Fatalf("GetSyncMember: %v", err)
	}
	if got != m {
		t.Fatalf("GetSyncMember = %+v, want %+v", got, m)
	}
	if _, err := s.GetSyncMember(c, "sm-bob", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSyncMember(missing): want ErrNotFound, got %v", err)
	}

	// Second member, then list by sync.
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sm-bob", NodeID: "bob-mister", Path: "SNES/Super Metroid.sav"}))
	members, err := s.ListSyncMembers(c, "sm-bob")
	if err != nil {
		t.Fatalf("ListSyncMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("ListSyncMembers len = %d, want 2", len(members))
	}
	if members[0].NodeID != "bob-deck" || members[1].NodeID != "bob-mister" {
		t.Fatalf("ListSyncMembers not ordered by node_id: %+v", members)
	}

	// Upsert: re-set the same (sync, node) to a new path in place.
	m.Path = "retroarch/saves/SM.srm"
	if err := s.SetSyncMember(c, m); err != nil {
		t.Fatalf("SetSyncMember(upsert): %v", err)
	}
	got, _ = s.GetSyncMember(c, "sm-bob", "bob-deck")
	if got.Path != "retroarch/saves/SM.srm" {
		t.Fatalf("upsert path = %q, want retroarch/saves/SM.srm", got.Path)
	}
	// Still exactly two members (upsert, not insert).
	members, _ = s.ListSyncMembers(c, "sm-bob")
	if len(members) != 2 {
		t.Fatalf("after upsert len = %d, want 2", len(members))
	}

	// Delete a member.
	if err := s.DeleteSyncMember(c, "sm-bob", "bob-deck"); err != nil {
		t.Fatalf("DeleteSyncMember: %v", err)
	}
	if err := s.DeleteSyncMember(c, "sm-bob", "bob-deck"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteSyncMember(missing): want ErrNotFound, got %v", err)
	}
}

// testSyncMemberUniquePathInvariant is THE core invariant: a given (node, path)
// lives in at most one sync. Adding a (node, path) already claimed by a
// DIFFERENT sync -> ErrConflict; the same node at a DIFFERENT path may join
// another sync (multi-slot); re-setting the same (sync, node) upserts its path.
func testSyncMemberUniquePathInvariant(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.CreateSync(c, store.Sync{ID: "sync-a", GameID: "super-metroid"}))
	must(t, s.CreateSync(c, store.Sync{ID: "sync-b", GameID: "super-metroid"}))

	// bob-deck's slot-1 file joins sync-a.
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sync-a", NodeID: "bob-deck", Path: "saves/slot1.srm"}))

	// The SAME (node, path) cannot also join sync-b.
	if err := s.SetSyncMember(c, store.SyncMember{SyncID: "sync-b", NodeID: "bob-deck", Path: "saves/slot1.srm"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("cross-sync (node,path) reuse: want ErrConflict, got %v", err)
	}

	// But the same NODE at a DIFFERENT path may join sync-b (multi-slot).
	if err := s.SetSyncMember(c, store.SyncMember{SyncID: "sync-b", NodeID: "bob-deck", Path: "saves/slot2.srm"}); err != nil {
		t.Fatalf("multi-slot (same node, different path): want nil, got %v", err)
	}

	// Re-setting sync-a's bob-deck member to a NEW path is an in-place upsert
	// (frees slot1, which no other sync may yet claim).
	if err := s.SetSyncMember(c, store.SyncMember{SyncID: "sync-a", NodeID: "bob-deck", Path: "saves/slot1b.srm"}); err != nil {
		t.Fatalf("upsert same (sync,node) to new path: want nil, got %v", err)
	}
	got, _ := s.GetSyncMember(c, "sync-a", "bob-deck")
	if got.Path != "saves/slot1b.srm" {
		t.Fatalf("upsert path = %q, want saves/slot1b.srm", got.Path)
	}

	// sync-a still has exactly one member (the upsert did not duplicate).
	a, _ := s.ListSyncMembers(c, "sync-a")
	if len(a) != 1 {
		t.Fatalf("sync-a members = %d, want 1", len(a))
	}
}

func testSyncMemberInvalidReference(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.CreateSync(c, store.Sync{ID: "sm", GameID: "super-metroid"}))

	// Missing sync (node exists).
	if err := s.SetSyncMember(c, store.SyncMember{SyncID: "ghost-sync", NodeID: "bob-deck", Path: "p"}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("SetSyncMember(missing sync): want ErrInvalidReference, got %v", err)
	}
	// Missing node (sync exists).
	if err := s.SetSyncMember(c, store.SyncMember{SyncID: "sm", NodeID: "ghost-node", Path: "p"}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("SetSyncMember(missing node): want ErrInvalidReference, got %v", err)
	}
}

// testSyncCascades asserts: delete sync -> members gone; delete the parent game
// -> syncs + members gone; delete a node -> its sync_member rows gone.
func testSyncCascades(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	mustNode(t, s, "bob-mister", nil)

	// Delete sync -> its members cascade.
	must(t, s.CreateSync(c, store.Sync{ID: "sync-del", GameID: "super-metroid"}))
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sync-del", NodeID: "bob-deck", Path: "a"}))
	must(t, s.DeleteSync(c, "sync-del"))
	if mem, _ := s.ListSyncMembers(c, "sync-del"); len(mem) != 0 {
		t.Fatalf("members after DeleteSync = %d, want 0 (cascade)", len(mem))
	}
	// The freed (node, path) may now be reused by a fresh sync.
	must(t, s.CreateSync(c, store.Sync{ID: "sync-reuse", GameID: "super-metroid"}))
	if err := s.SetSyncMember(c, store.SyncMember{SyncID: "sync-reuse", NodeID: "bob-deck", Path: "a"}); err != nil {
		t.Fatalf("reuse freed (node,path) after cascade: want nil, got %v", err)
	}

	// Delete the parent game -> its syncs AND members cascade.
	mustGame(t, s, "zelda")
	must(t, s.CreateSync(c, store.Sync{ID: "z-sync", GameID: "zelda"}))
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "z-sync", NodeID: "bob-mister", Path: "z"}))
	must(t, s.DeleteGame(c, "zelda"))
	if syncs, _ := s.ListSyncsByGame(c, "zelda"); len(syncs) != 0 {
		t.Fatalf("syncs after DeleteGame = %d, want 0 (cascade)", len(syncs))
	}
	if _, err := s.GetSync(c, "z-sync"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("sync after DeleteGame: want ErrNotFound, got %v", err)
	}
	if mem, _ := s.ListSyncMembersByNode(c, "bob-mister"); len(mem) != 0 {
		t.Fatalf("members after DeleteGame cascade = %d, want 0", len(mem))
	}

	// Delete a node -> its sync_member rows cascade (sync itself survives).
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sync-reuse", NodeID: "bob-deck", Path: "a"}))
	must(t, s.DeleteNode(c, "bob-deck"))
	if mem, _ := s.ListSyncMembersByNode(c, "bob-deck"); len(mem) != 0 {
		t.Fatalf("members after DeleteNode = %d, want 0 (cascade)", len(mem))
	}
	if _, err := s.GetSync(c, "sync-reuse"); err != nil {
		t.Fatalf("sync should survive node delete: %v", err)
	}
}

func testSyncMembersByNode(t *testing.T, s store.Store) {
	c := ctx()
	mustGame(t, s, "super-metroid")
	mustGame(t, s, "zelda")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.CreateSync(c, store.Sync{ID: "sm", GameID: "super-metroid"}))
	must(t, s.CreateSync(c, store.Sync{ID: "z", GameID: "zelda"}))

	// One node, two different files, in two different syncs (multi-slot).
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sm", NodeID: "bob-deck", Path: "sm.srm"}))
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "z", NodeID: "bob-deck", Path: "z.srm"}))

	byNode, err := s.ListSyncMembersByNode(c, "bob-deck")
	if err != nil {
		t.Fatalf("ListSyncMembersByNode: %v", err)
	}
	if len(byNode) != 2 {
		t.Fatalf("ListSyncMembersByNode len = %d, want 2", len(byNode))
	}
	if byNode[0].SyncID != "sm" || byNode[1].SyncID != "z" {
		t.Fatalf("ListSyncMembersByNode not ordered by sync_id: %+v", byNode)
	}
	// A node with no members returns empty, not error.
	mustNode(t, s, "lonely", nil)
	empty, _ := s.ListSyncMembersByNode(c, "lonely")
	if len(empty) != 0 {
		t.Fatalf("ListSyncMembersByNode(lonely) = %d, want 0", len(empty))
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
