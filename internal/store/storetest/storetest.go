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
	"fmt"
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
		{"InvalidReference", testInvalidReference},
		{"InvalidValue", testInvalidValue},
		{"ListSyncs", testListSyncs},
		{"SyncMarkSynced", testSyncMarkSynced},
		{"SyncConflictFlag", testSyncConflictFlag},
		{"SyncStateSurvivesRename", testSyncStateSurvivesRename},
		{"SyncLog", testSyncLog},
		{"SyncLogOrderByTS", testSyncLogOrderByTS},
		{"SyncLogInvalidValue", testSyncLogInvalidValue},
		{"SyncLogInvalidReference", testSyncLogInvalidReference},
		{"Manifest", testManifest},
		{"CascadeManifestOnNodeDelete", testCascadeManifestOnNodeDelete},
		{"Syncs", testSyncs},
		{"SyncFreeTextGameLabel", testSyncFreeTextGameLabel},
		{"SyncMembers", testSyncMembers},
		{"SyncMemberUniquePathInvariant", testSyncMemberUniquePathInvariant},
		{"SyncMemberInvalidReference", testSyncMemberInvalidReference},
		{"SyncCascades", testSyncCascades},
		{"SyncMembersByNode", testSyncMembersByNode},
		{"RuntimeCascadesOnSyncDelete", testRuntimeCascadesOnSyncDelete},
		{"SaveVersionRetentionAndOrder", testSaveVersionRetentionAndOrder},
		{"SaveVersionDedup", testSaveVersionDedup},
		{"SaveVersionBlobDedupAndGC", testSaveVersionBlobDedupAndGC},
		{"SaveVersionPerMemberIsolation", testSaveVersionPerMemberIsolation},
		{"SaveVersionCascadeAndGC", testSaveVersionCascadeAndGC},
		{"SaveVersionInvalidReference", testSaveVersionInvalidReference},
		{"SaveVersionGetData", testSaveVersionGetData},
		{"SaveVersionGetDataCrossSyncBinding", testSaveVersionGetDataCrossSyncBinding},
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

	n := store.Node{
		ID:          "bob-deck",
		OwnerUserID: ptr("bob"),
		Display:     "Bob's Deck",
		Kind:        store.KindDeck,
		Reach:       store.ReachSyncthingShare,
		ReachConfig: store.ReachConfig{Path: "/srv/syncthing/bob-deck-saves"},
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

	// Update: change display.
	n.Display = "Bob Deck 2"
	if err := s.UpdateNode(c, n); err != nil {
		t.Fatalf("UpdateNode: %v", err)
	}
	got, _ = s.GetNode(c, "bob-deck")
	if got.Display != "Bob Deck 2" {
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

// testInvalidReference asserts both stores reject writes that name a missing
// parent row with ErrInvalidReference (FK enforcement). Note: a sync's game is a
// free-text LABEL, not an FK, so CreateSync never raises this for the game.
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

	// CreateSync carrying any game label succeeds (the label is free text, no FK).
	if err := s.CreateSync(c, store.Sync{ID: "ghost-sync", Game: "any-label"}); err != nil {
		t.Fatalf("CreateSync(free-text game label): want nil, got %v", err)
	}

	// SetSyncMember naming a missing sync (node exists).
	mustNode(t, s, "real-node", nil)
	err = s.SetSyncMember(c, store.SyncMember{SyncID: "no-such-sync", NodeID: "real-node", Path: "p"})
	if !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("SetSyncMember(missing sync): want ErrInvalidReference, got %v", err)
	}

	// SetSyncMember naming a missing node (sync exists).
	must(t, s.CreateSync(c, store.Sync{ID: "real-sync", Game: "real"}))
	err = s.SetSyncMember(c, store.SyncMember{SyncID: "real-sync", NodeID: "ghost-node", Path: "p"})
	if !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("SetSyncMember(missing node): want ErrInvalidReference, got %v", err)
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

// ---- runtime: per-sync state (conflict_at / last_synced) ----

func testListSyncs(t *testing.T, s store.Store) {
	c := ctx()
	// ListSyncs returns ALL syncs across games, ordered by id. The daemon sweeps
	// every one (there is no "active" subset under auto-mirror).
	mustSync(t, s, "z-1", "zelda")
	mustSync(t, s, "sm-bob", "super-metroid")
	mustSync(t, s, "sm-alice", "super-metroid")

	list, err := s.ListSyncs(c)
	if err != nil {
		t.Fatalf("ListSyncs: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListSyncs len = %d, want 3", len(list))
	}
	if list[0].ID != "sm-alice" || list[1].ID != "sm-bob" || list[2].ID != "z-1" {
		t.Fatalf("ListSyncs not ordered by id: %+v", list)
	}
	// A freshly-created sync has no runtime state.
	for _, sy := range list {
		if sy.ConflictAt != nil || sy.LastSynced != nil {
			t.Fatalf("new sync %q should have nil conflict_at/last_synced: %+v", sy.ID, sy)
		}
	}
}

func testSyncMarkSynced(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")

	synced := time.Date(2026, 6, 21, 14, 0, 0, 0, time.UTC)
	if err := s.MarkSyncSynced(c, "sm-bob", synced); err != nil {
		t.Fatalf("MarkSyncSynced: %v", err)
	}
	got, _ := s.GetSync(c, "sm-bob")
	if got.LastSynced == nil || !got.LastSynced.Equal(synced) {
		t.Fatalf("last_synced = %v, want %v", got.LastSynced, synced)
	}
	// MarkSyncSynced does not touch conflict_at.
	if got.ConflictAt != nil {
		t.Fatalf("MarkSyncSynced should not set conflict_at: %+v", got)
	}

	// Missing sync -> ErrNotFound.
	if err := s.MarkSyncSynced(c, "ghost", synced); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("MarkSyncSynced(missing): want ErrNotFound, got %v", err)
	}
}

func testSyncConflictFlag(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")

	got, _ := s.GetSync(c, "sm-bob")
	if got.ConflictAt != nil {
		t.Fatalf("new sync conflict_at = %v, want nil", got.ConflictAt)
	}

	// Set conflict_at (a fork was detected; mirroring pauses).
	conflictTS := time.Date(2026, 6, 21, 15, 30, 0, 0, time.UTC)
	if err := s.SetSyncConflict(c, "sm-bob", &conflictTS); err != nil {
		t.Fatalf("SetSyncConflict(set): %v", err)
	}
	set, _ := s.GetSync(c, "sm-bob")
	if set.ConflictAt == nil || !set.ConflictAt.Equal(conflictTS) {
		t.Fatalf("conflict_at = %v, want %v", set.ConflictAt, conflictTS)
	}

	// Setting conflict does not touch last_synced.
	if set.LastSynced != nil {
		t.Fatalf("SetSyncConflict should not set last_synced: %+v", set)
	}

	// Clear conflict_at (resolution).
	if err := s.SetSyncConflict(c, "sm-bob", nil); err != nil {
		t.Fatalf("SetSyncConflict(clear): %v", err)
	}
	cleared, _ := s.GetSync(c, "sm-bob")
	if cleared.ConflictAt != nil {
		t.Fatalf("conflict_at after clear = %v, want nil", cleared.ConflictAt)
	}

	// Missing sync -> ErrNotFound.
	if err := s.SetSyncConflict(c, "ghost", &conflictTS); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetSyncConflict(missing): want ErrNotFound, got %v", err)
	}
}

func testSyncStateSurvivesRename(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")
	synced := time.Date(2026, 6, 21, 14, 0, 0, 0, time.UTC)
	conflictTS := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	must(t, s.MarkSyncSynced(c, "sm-bob", synced))
	must(t, s.SetSyncConflict(c, "sm-bob", &conflictTS))

	// UpdateSync rewrites the registry fields (name) only; the runtime state
	// (conflict_at, last_synced) must survive a rename.
	sy, _ := s.GetSync(c, "sm-bob")
	sy.Name = "Bob's renamed stream"
	must(t, s.UpdateSync(c, sy))

	got, _ := s.GetSync(c, "sm-bob")
	if got.Name != "Bob's renamed stream" {
		t.Fatalf("rename not applied: %+v", got)
	}
	if got.LastSynced == nil || !got.LastSynced.Equal(synced) {
		t.Fatalf("UpdateSync clobbered last_synced: %v, want %v", got.LastSynced, synced)
	}
	if got.ConflictAt == nil || !got.ConflictAt.Equal(conflictTS) {
		t.Fatalf("UpdateSync clobbered conflict_at: %v, want %v", got.ConflictAt, conflictTS)
	}
}

// ---- runtime: sync_log ----

func testSyncLog(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")
	mustSync(t, s, "z-1", "zelda")

	bytes := int64(2048)
	src := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	dst := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)

	// Three entries for sm-bob, appended oldest-first.
	must(t, s.AppendLog(c, store.LogEntry{
		SyncID: "sm-bob", FromNode: "bob-deck", ToNode: "mister",
		Bytes: &bytes, SrcMtime: &src, DstMtime: &dst,
		Outcome: store.OutcomeOK, Message: "",
	}))
	must(t, s.AppendLog(c, store.LogEntry{
		SyncID: "sm-bob", FromNode: "bob-deck", ToNode: "alice-deck",
		Outcome: store.OutcomeNoop,
	}))
	must(t, s.AppendLog(c, store.LogEntry{
		SyncID: "sm-bob", Outcome: store.OutcomeError, Message: "boom",
	}))
	// An entry for a different sync must not leak into the listing.
	must(t, s.AppendLog(c, store.LogEntry{SyncID: "z-1", Outcome: store.OutcomeOK}))

	// Most-recent-first: the error entry (appended last) comes first.
	all, err := s.ListLogBySync(c, "sm-bob", 0)
	if err != nil {
		t.Fatalf("ListLogBySync: %v", err)
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
	limited, err := s.ListLogBySync(c, "sm-bob", 2)
	if err != nil {
		t.Fatalf("ListLogBySync(limit): %v", err)
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
	mustSync(t, s, "sm-bob", "super-metroid")

	t1 := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) // earliest
	t2 := time.Date(2026, 6, 21, 13, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 6, 21, 14, 0, 0, 0, time.UTC) // latest

	// Append in an order where insertion (id) order != ts order:
	//   id=1 -> t3 (latest), id=2 -> t1 (earliest), id=3 -> t2 (middle).
	// Relying on append order would yield id=3,2,1; the ts contract yields
	// id=1 (t3), id=3 (t2), id=2 (t1).
	must(t, s.AppendLog(c, store.LogEntry{SyncID: "sm-bob", TS: t3, Message: "id1-t3", Outcome: store.OutcomeOK}))
	must(t, s.AppendLog(c, store.LogEntry{SyncID: "sm-bob", TS: t1, Message: "id2-t1", Outcome: store.OutcomeOK}))
	must(t, s.AppendLog(c, store.LogEntry{SyncID: "sm-bob", TS: t2, Message: "id3-t2", Outcome: store.OutcomeOK}))

	got, err := s.ListLogBySync(c, "sm-bob", 0)
	if err != nil {
		t.Fatalf("ListLogBySync: %v", err)
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
	mustSync(t, s, "z-1", "zelda")
	tie := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	must(t, s.AppendLog(c, store.LogEntry{SyncID: "z-1", TS: tie, Message: "first", Outcome: store.OutcomeOK}))
	must(t, s.AppendLog(c, store.LogEntry{SyncID: "z-1", TS: tie, Message: "second", Outcome: store.OutcomeOK}))

	ties, err := s.ListLogBySync(c, "z-1", 0)
	if err != nil {
		t.Fatalf("ListLogBySync(z-1): %v", err)
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
	mustSync(t, s, "sm-bob", "super-metroid")
	if err := s.AppendLog(c, store.LogEntry{
		SyncID: "sm-bob", Outcome: store.Outcome("exploded"),
	}); !errors.Is(err, store.ErrInvalidValue) {
		t.Fatalf("AppendLog(bad outcome): want ErrInvalidValue, got %v", err)
	}
}

func testSyncLogInvalidReference(t *testing.T, s store.Store) {
	c := ctx()
	if err := s.AppendLog(c, store.LogEntry{
		SyncID: "ghost-sync", Outcome: store.OutcomeOK,
	}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("AppendLog(missing sync): want ErrInvalidReference, got %v", err)
	}
}

// ---- runtime: manifest ----

func testManifest(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	mustNode(t, s, "mister", nil)

	mtime := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	size := int64(512)
	checked := time.Date(2026, 6, 21, 9, 0, 30, 0, time.UTC)
	m := store.ManifestEntry{
		SyncID: "sm-bob", NodeID: "bob-deck",
		Mtime: &mtime, Size: &size, LastChecked: &checked,
	}
	if err := s.SetManifest(c, m); err != nil {
		t.Fatalf("SetManifest: %v", err)
	}
	got, err := s.GetManifest(c, "sm-bob", "bob-deck")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if got.Mtime == nil || !got.Mtime.Equal(mtime) || got.Size == nil || *got.Size != size {
		t.Fatalf("GetManifest mismatch: %+v", got)
	}
	if got.SHA256 != nil {
		t.Fatalf("SHA256 = %v, want nil (lazy)", got.SHA256)
	}

	if _, err := s.GetManifest(c, "sm-bob", "nope"); !errors.Is(err, store.ErrNotFound) {
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
	got, _ = s.GetManifest(c, "sm-bob", "bob-deck")
	if got.Mtime == nil || !got.Mtime.Equal(mtime2) || got.Size == nil || *got.Size != size2 {
		t.Fatalf("upsert not applied: %+v", got)
	}
	if got.SHA256 == nil || *got.SHA256 != sha {
		t.Fatalf("upsert sha256 = %v, want %q", got.SHA256, sha)
	}

	// Second node, then list by sync.
	must(t, s.SetManifest(c, store.ManifestEntry{SyncID: "sm-bob", NodeID: "mister"}))
	list, err := s.ListManifestBySync(c, "sm-bob")
	if err != nil {
		t.Fatalf("ListManifestBySync: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListManifestBySync len = %d, want 2", len(list))
	}
	if list[0].NodeID != "bob-deck" || list[1].NodeID != "mister" {
		t.Fatalf("ListManifestBySync not ordered by node_id: %+v", list)
	}

	// Missing sync/node -> invalid reference.
	if err := s.SetManifest(c, store.ManifestEntry{SyncID: "ghost", NodeID: "bob-deck"}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("SetManifest(missing sync): want ErrInvalidReference, got %v", err)
	}
	if err := s.SetManifest(c, store.ManifestEntry{SyncID: "sm-bob", NodeID: "ghost"}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("SetManifest(missing node): want ErrInvalidReference, got %v", err)
	}
}

// ---- runtime: cross-cutting delete behavior ----

// testCascadeManifestOnNodeDelete asserts manifest rows cascade when a node is
// deleted (sync_log from/to_node are unconstrained text, so they do not block
// or cascade).
func testCascadeManifestOnNodeDelete(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.SetManifest(c, store.ManifestEntry{SyncID: "sm-bob", NodeID: "bob-deck"}))

	if err := s.DeleteNode(c, "bob-deck"); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	man, _ := s.ListManifestBySync(c, "sm-bob")
	if len(man) != 0 {
		t.Fatalf("manifest after node delete = %d, want 0 (cascade)", len(man))
	}
}

// ---- syncs / sync_members ----

func testSyncs(t *testing.T, s store.Store) {
	c := ctx()

	sy := store.Sync{ID: "sm-bob", Game: "Super Metroid", Name: "Bob's stream"}
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
	if got.Game != "Super Metroid" {
		t.Fatalf("GetSync game label = %q, want Super Metroid", got.Game)
	}
	if _, err := s.GetSync(c, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSync(missing): want ErrNotFound, got %v", err)
	}

	// A second, independent sync sharing the SAME game label (the multi-sync case).
	must(t, s.CreateSync(c, store.Sync{ID: "sm-alice", Game: "Super Metroid", Name: "Alice's stream"}))
	// A sync with an EMPTY game label is allowed (the label is free text).
	must(t, s.CreateSync(c, store.Sync{ID: "z-1", Game: ""}))

	list, err := s.ListSyncs(c)
	if err != nil {
		t.Fatalf("ListSyncs: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListSyncs len = %d, want 3", len(list))
	}

	// Update mutable fields, including the free-text label.
	sy.Name = "Bob renamed"
	sy.Game = "Super Metroid (relabeled)"
	if err := s.UpdateSync(c, sy); err != nil {
		t.Fatalf("UpdateSync: %v", err)
	}
	reread, _ := s.GetSync(c, "sm-bob")
	if reread.Name != "Bob renamed" || reread.Game != "Super Metroid (relabeled)" {
		t.Fatalf("UpdateSync not applied: %+v", reread)
	}
	// Update missing -> not found.
	if err := s.UpdateSync(c, store.Sync{ID: "ghost", Game: "x"}); !errors.Is(err, store.ErrNotFound) {
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

// testSyncFreeTextGameLabel asserts the game label is free text (no FK): create
// and update accept ANY label — including one that names no registry row and the
// empty string — without ErrInvalidReference.
func testSyncFreeTextGameLabel(t *testing.T, s store.Store) {
	c := ctx()
	// CreateSync with an arbitrary label succeeds.
	if err := s.CreateSync(c, store.Sync{ID: "orphan", Game: "Whatever Title"}); err != nil {
		t.Fatalf("CreateSync(free-text label): want nil, got %v", err)
	}
	// UpdateSync to any other label (including a brand-new one) succeeds.
	if err := s.UpdateSync(c, store.Sync{ID: "orphan", Game: "Some Other Title"}); err != nil {
		t.Fatalf("UpdateSync(free-text label): want nil, got %v", err)
	}
	got, _ := s.GetSync(c, "orphan")
	if got.Game != "Some Other Title" {
		t.Fatalf("relabel not applied: %+v", got)
	}
}

func testSyncMembers(t *testing.T, s store.Store) {
	c := ctx()
	mustNode(t, s, "bob-deck", nil)
	mustNode(t, s, "bob-mister", nil)
	must(t, s.CreateSync(c, store.Sync{ID: "sm-bob", Game: "super-metroid"}))

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
	mustNode(t, s, "bob-deck", nil)
	must(t, s.CreateSync(c, store.Sync{ID: "sync-a", Game: "super-metroid"}))
	must(t, s.CreateSync(c, store.Sync{ID: "sync-b", Game: "super-metroid"}))

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
	mustNode(t, s, "bob-deck", nil)
	must(t, s.CreateSync(c, store.Sync{ID: "sm", Game: "super-metroid"}))

	// Missing sync (node exists).
	if err := s.SetSyncMember(c, store.SyncMember{SyncID: "ghost-sync", NodeID: "bob-deck", Path: "p"}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("SetSyncMember(missing sync): want ErrInvalidReference, got %v", err)
	}
	// Missing node (sync exists).
	if err := s.SetSyncMember(c, store.SyncMember{SyncID: "sm", NodeID: "ghost-node", Path: "p"}); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("SetSyncMember(missing node): want ErrInvalidReference, got %v", err)
	}
}

// testSyncCascades asserts: delete sync -> members gone; delete a node -> its
// sync_member rows gone. (There is no games table to cascade from anymore — the
// game label is free text on the sync.)
func testSyncCascades(t *testing.T, s store.Store) {
	c := ctx()
	mustNode(t, s, "bob-deck", nil)
	mustNode(t, s, "bob-mister", nil)

	// Delete sync -> its members cascade.
	must(t, s.CreateSync(c, store.Sync{ID: "sync-del", Game: "Super Metroid"}))
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sync-del", NodeID: "bob-deck", Path: "a"}))
	must(t, s.DeleteSync(c, "sync-del"))
	if mem, _ := s.ListSyncMembers(c, "sync-del"); len(mem) != 0 {
		t.Fatalf("members after DeleteSync = %d, want 0 (cascade)", len(mem))
	}
	// The freed (node, path) may now be reused by a fresh sync.
	must(t, s.CreateSync(c, store.Sync{ID: "sync-reuse", Game: "Super Metroid"}))
	if err := s.SetSyncMember(c, store.SyncMember{SyncID: "sync-reuse", NodeID: "bob-deck", Path: "a"}); err != nil {
		t.Fatalf("reuse freed (node,path) after cascade: want nil, got %v", err)
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
	mustNode(t, s, "bob-deck", nil)
	must(t, s.CreateSync(c, store.Sync{ID: "sm", Game: "super-metroid"}))
	must(t, s.CreateSync(c, store.Sync{ID: "z", Game: "zelda"}))

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

// testRuntimeCascadesOnSyncDelete asserts the FK behavior: deleting a sync
// cascades its manifest and sync_log rows (both REFERENCE syncs ON DELETE
// CASCADE). The sync's own runtime state (conflict_at, last_synced) lives on the
// sync row, so it goes with the delete. DeleteSync does NOT refuse a conflicted
// sync — it intentionally discards the unresolved fork.
func testRuntimeCascadesOnSyncDelete(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	conflictTS := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	must(t, s.SetSyncConflict(c, "sm-bob", &conflictTS))
	must(t, s.SetManifest(c, store.ManifestEntry{SyncID: "sm-bob", NodeID: "bob-deck"}))
	must(t, s.AppendLog(c, store.LogEntry{SyncID: "sm-bob", Outcome: store.OutcomeOK}))

	if err := s.DeleteSync(c, "sm-bob"); err != nil {
		t.Fatalf("DeleteSync(conflicted): want nil (discards fork), got %v", err)
	}
	if _, err := s.GetSync(c, "sm-bob"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("sync after delete: want ErrNotFound, got %v", err)
	}
	if man, _ := s.ListManifestBySync(c, "sm-bob"); len(man) != 0 {
		t.Fatalf("manifest after sync delete = %d, want 0 (cascade)", len(man))
	}
	if logs, _ := s.ListLogBySync(c, "sm-bob", 0); len(logs) != 0 {
		t.Fatalf("sync_log after sync delete = %d, want 0 (cascade)", len(logs))
	}
	// The node, freed of its member, is now deletable.
	if err := s.DeleteNode(c, "bob-deck"); err != nil {
		t.Fatalf("DeleteNode after sync delete: %v", err)
	}
}

// ---- helpers ----

// ---- SaveVersions (the recovery net) ----

// testSaveVersionRetentionAndOrder asserts that putting N+3 versions keeps only
// the newest store.SaveVersionRetention, ordered by seq (newest-first), and that
// retention is by SEQ not by captured_at (a bad clock must never misorder).
func testSaveVersionRetentionAndOrder(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sm-bob", NodeID: "bob-deck", Path: "p"}))

	n := store.SaveVersionRetention + 3
	hashes := make([]string, n)
	for i := 0; i < n; i++ {
		// Distinct content (and hash) per version so none dedups.
		h := fmt.Sprintf("h%02d", i)
		hashes[i] = h
		must(t, s.PutSaveVersion(c, "sm-bob", "bob-deck", h, []byte(h+"-data"), "propagate"))
	}

	got, err := s.ListSaveVersions(c, "sm-bob", "bob-deck", 0)
	if err != nil {
		t.Fatalf("ListSaveVersions: %v", err)
	}
	if len(got) != store.SaveVersionRetention {
		t.Fatalf("kept %d versions, want %d", len(got), store.SaveVersionRetention)
	}
	// Newest-first by seq, descending and strictly monotonic.
	for i := 1; i < len(got); i++ {
		if got[i-1].Seq <= got[i].Seq {
			t.Fatalf("versions not strictly seq-descending: %d then %d", got[i-1].Seq, got[i].Seq)
		}
	}
	// The newest store.SaveVersionRetention hashes survived; the oldest 3 were pruned.
	wantNewest := hashes[n-1]
	if got[0].Hash != wantNewest {
		t.Fatalf("newest version hash = %q want %q", got[0].Hash, wantNewest)
	}
	survivors := make(map[string]bool, len(got))
	for _, v := range got {
		survivors[v.Hash] = true
	}
	for _, oldH := range hashes[:3] {
		if survivors[oldH] {
			t.Fatalf("pruned hash %q should not have survived", oldH)
		}
	}

	// Size is carried from the blob.
	for _, v := range got {
		if v.Size != int64(len(v.Hash+"-data")) {
			t.Fatalf("version %q size = %d, want %d", v.Hash, v.Size, len(v.Hash+"-data"))
		}
		if v.Reason != "propagate" {
			t.Fatalf("version %q reason = %q, want propagate", v.Hash, v.Reason)
		}
	}

	// limit caps the result newest-first.
	lim, err := s.ListSaveVersions(c, "sm-bob", "bob-deck", 2)
	if err != nil {
		t.Fatalf("ListSaveVersions(limit): %v", err)
	}
	if len(lim) != 2 || lim[0].Hash != wantNewest {
		t.Fatalf("limit-2 list = %+v, want newest two", lim)
	}
}

// testSaveVersionDedup asserts that an identical consecutive hash does NOT add a
// new version (no churn on a no-op/identical re-save), but a different hash in
// between lets the same hash be captured again.
func testSaveVersionDedup(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sm-bob", NodeID: "bob-deck", Path: "p"}))

	must(t, s.PutSaveVersion(c, "sm-bob", "bob-deck", "A", []byte("a"), "propagate"))
	// Identical consecutive hash: no new row.
	must(t, s.PutSaveVersion(c, "sm-bob", "bob-deck", "A", []byte("a"), "propagate"))
	got, _ := s.ListSaveVersions(c, "sm-bob", "bob-deck", 0)
	if len(got) != 1 {
		t.Fatalf("identical consecutive put added a row: %d, want 1", len(got))
	}

	// A different hash, then A again: both A captures exist (not consecutive).
	must(t, s.PutSaveVersion(c, "sm-bob", "bob-deck", "B", []byte("b"), "propagate"))
	must(t, s.PutSaveVersion(c, "sm-bob", "bob-deck", "A", []byte("a"), "propagate"))
	got, _ = s.ListSaveVersions(c, "sm-bob", "bob-deck", 0)
	if len(got) != 3 {
		t.Fatalf("non-consecutive A should re-capture: %d versions, want 3", len(got))
	}
	if got[0].Hash != "A" || got[1].Hash != "B" || got[2].Hash != "A" {
		t.Fatalf("unexpected version order: %q %q %q", got[0].Hash, got[1].Hash, got[2].Hash)
	}
}

// testSaveVersionBlobDedupAndGC asserts content-addressed dedup (a hash shared
// by two versions stores its bytes once and survives while either version does)
// and orphan-blob GC (a hash used only by a pruned version is removed, a shared
// hash is kept).
func testSaveVersionBlobDedupAndGC(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	mustNode(t, s, "carol-deck", nil)
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sm-bob", NodeID: "bob-deck", Path: "p"}))
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sm-bob", NodeID: "carol-deck", Path: "q"}))

	// SHARED hash across two different members: stored once, referenced twice.
	must(t, s.PutSaveVersion(c, "sm-bob", "bob-deck", "SHARED", []byte("shared"), "propagate"))
	must(t, s.PutSaveVersion(c, "sm-bob", "carol-deck", "SHARED", []byte("shared"), "propagate"))

	// A UNIQUE hash on bob-deck that we will then push out of retention.
	must(t, s.PutSaveVersion(c, "sm-bob", "bob-deck", "UNIQUE", []byte("unique"), "propagate"))
	uniqueSeq := findSeq(t, s, "sm-bob", "bob-deck", "UNIQUE")

	// Churn bob-deck past retention so UNIQUE (and bob's SHARED) are pruned, but
	// carol-deck still references SHARED -> SHARED's blob must survive.
	for i := 0; i < store.SaveVersionRetention+1; i++ {
		h := fmt.Sprintf("churn%02d", i)
		must(t, s.PutSaveVersion(c, "sm-bob", "bob-deck", h, []byte(h), "propagate"))
	}

	// UNIQUE's version is gone (pruned) -> GetSaveVersionData -> ErrNotFound, and
	// its orphan blob was GC'd (no other version references "UNIQUE").
	if _, _, err := s.GetSaveVersionData(c, "sm-bob", uniqueSeq); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pruned UNIQUE version: want ErrNotFound, got %v", err)
	}

	// SHARED still has carol-deck's live version -> its blob survives and is
	// readable.
	sharedSeq := findSeq(t, s, "sm-bob", "carol-deck", "SHARED")
	v, data, err := s.GetSaveVersionData(c, "sm-bob", sharedSeq)
	if err != nil {
		t.Fatalf("GetSaveVersionData(SHARED): %v", err)
	}
	if v.Hash != "SHARED" || string(data) != "shared" {
		t.Fatalf("SHARED data = %q (hash %q), want shared", data, v.Hash)
	}
}

// testSaveVersionPerMemberIsolation asserts retention is per (sync,node): heavy
// churn on one member does not evict another member's versions.
func testSaveVersionPerMemberIsolation(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	mustNode(t, s, "carol-deck", nil)
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sm-bob", NodeID: "bob-deck", Path: "p"}))
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sm-bob", NodeID: "carol-deck", Path: "q"}))

	// One version for carol-deck.
	must(t, s.PutSaveVersion(c, "sm-bob", "carol-deck", "carol-v", []byte("c"), "propagate"))

	// Heavy churn on bob-deck (well past retention).
	for i := 0; i < store.SaveVersionRetention+5; i++ {
		h := fmt.Sprintf("bob%02d", i)
		must(t, s.PutSaveVersion(c, "sm-bob", "bob-deck", h, []byte(h), "propagate"))
	}

	carol, _ := s.ListSaveVersions(c, "sm-bob", "carol-deck", 0)
	if len(carol) != 1 || carol[0].Hash != "carol-v" {
		t.Fatalf("carol-deck version evicted by bob-deck churn: %+v", carol)
	}
	bob, _ := s.ListSaveVersions(c, "sm-bob", "bob-deck", 0)
	if len(bob) != store.SaveVersionRetention {
		t.Fatalf("bob-deck kept %d, want %d", len(bob), store.SaveVersionRetention)
	}
}

// testSaveVersionCascadeAndGC asserts versions cascade when their sync (or node)
// is deleted, and that the orphaned blobs are GC'd.
func testSaveVersionCascadeAndGC(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sm-bob", NodeID: "bob-deck", Path: "p"}))
	must(t, s.PutSaveVersion(c, "sm-bob", "bob-deck", "X", []byte("x"), "propagate"))
	seq := findSeq(t, s, "sm-bob", "bob-deck", "X")

	// Delete the sync: its versions cascade away and the orphan blob is GC'd.
	must(t, s.DeleteSync(c, "sm-bob"))
	got, err := s.ListSaveVersions(c, "sm-bob", "bob-deck", 0)
	if err != nil {
		t.Fatalf("ListSaveVersions after sync delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("versions survived sync delete: %d", len(got))
	}
	if _, _, err := s.GetSaveVersionData(c, "sm-bob", seq); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("version after sync delete: want ErrNotFound, got %v", err)
	}

	// Re-create and verify NODE delete cascades too.
	mustSync(t, s, "sm-bob2", "super-metroid")
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sm-bob2", NodeID: "bob-deck", Path: "p"}))
	must(t, s.PutSaveVersion(c, "sm-bob2", "bob-deck", "Y", []byte("y"), "propagate"))
	must(t, s.DeleteNode(c, "bob-deck"))
	got, _ = s.ListSaveVersions(c, "sm-bob2", "bob-deck", 0)
	if len(got) != 0 {
		t.Fatalf("versions survived node delete: %d", len(got))
	}
}

// testSaveVersionInvalidReference asserts a put against a missing sync or node
// is rejected with ErrInvalidReference.
func testSaveVersionInvalidReference(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sm-bob", NodeID: "bob-deck", Path: "p"}))

	if err := s.PutSaveVersion(c, "ghost", "bob-deck", "h", []byte("d"), "r"); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("put missing sync: want ErrInvalidReference, got %v", err)
	}
	if err := s.PutSaveVersion(c, "sm-bob", "ghost", "h", []byte("d"), "r"); !errors.Is(err, store.ErrInvalidReference) {
		t.Fatalf("put missing node: want ErrInvalidReference, got %v", err)
	}
}

// testSaveVersionGetData asserts GetSaveVersionData returns the version metadata
// plus its blob bytes, and ErrNotFound for an unknown seq.
func testSaveVersionGetData(t *testing.T, s store.Store) {
	c := ctx()
	mustSync(t, s, "sm-bob", "super-metroid")
	mustNode(t, s, "bob-deck", nil)
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sm-bob", NodeID: "bob-deck", Path: "p"}))
	must(t, s.PutSaveVersion(c, "sm-bob", "bob-deck", "Z", []byte("zebra"), "conflict-resolve"))
	seq := findSeq(t, s, "sm-bob", "bob-deck", "Z")

	v, data, err := s.GetSaveVersionData(c, "sm-bob", seq)
	if err != nil {
		t.Fatalf("GetSaveVersionData: %v", err)
	}
	if v.SyncID != "sm-bob" || v.NodeID != "bob-deck" || v.Hash != "Z" || v.Reason != "conflict-resolve" {
		t.Fatalf("version metadata mismatch: %+v", v)
	}
	if string(data) != "zebra" {
		t.Fatalf("blob data = %q, want zebra", data)
	}
	if v.Size != int64(len("zebra")) {
		t.Fatalf("size = %d, want %d", v.Size, len("zebra"))
	}
	if _, _, err := s.GetSaveVersionData(c, "sm-bob", seq+9999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSaveVersionData(unknown): want ErrNotFound, got %v", err)
	}
}

// testSaveVersionGetDataCrossSyncBinding pins the store-enforced authz floor for
// restore: GetSaveVersionData binds the seq to the requested sync. A seq that
// belongs to sync B is ErrNotFound when asked for under sync A (so a member of A
// who guesses B's bigserial seq can never load — let alone restore — B's bytes),
// while the SAME seq under its OWN sync loads normally.
func testSaveVersionGetDataCrossSyncBinding(t *testing.T, s store.Store) {
	c := ctx()
	// Two independent syncs of the same game, each with its own member + version.
	mustSync(t, s, "sync-a", "super-metroid")
	mustSync(t, s, "sync-b", "super-metroid")
	mustNode(t, s, "node-a", nil)
	mustNode(t, s, "node-b", nil)
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sync-a", NodeID: "node-a", Path: "a"}))
	must(t, s.SetSyncMember(c, store.SyncMember{SyncID: "sync-b", NodeID: "node-b", Path: "b"}))

	must(t, s.PutSaveVersion(c, "sync-b", "node-b", "B", []byte("b-secret"), "propagate"))
	seqB := findSeq(t, s, "sync-b", "node-b", "B")

	// B's seq under A (the cross-sync attempt): not found, NOT B's data.
	if _, _, err := s.GetSaveVersionData(c, "sync-a", seqB); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-sync GetSaveVersionData(sync-a, seqOfB): want ErrNotFound, got %v", err)
	}
	// B's seq under its OWN sync: loads normally (proving the seq itself is valid;
	// the rejection above is purely the sync binding, not a bad seq).
	v, data, err := s.GetSaveVersionData(c, "sync-b", seqB)
	if err != nil {
		t.Fatalf("GetSaveVersionData(sync-b, seqOfB): %v", err)
	}
	if v.SyncID != "sync-b" || string(data) != "b-secret" {
		t.Fatalf("own-sync load mismatch: sync=%q data=%q", v.SyncID, data)
	}
}

// findSeq returns the seq of the version for (sync,node) with the given hash, or
// fails the test. Used so cascade/GC assertions can reference a specific version
// by seq without depending on bigserial numbering across stores.
func findSeq(t *testing.T, s store.Store, syncID, nodeID, hash string) int64 {
	t.Helper()
	vs, err := s.ListSaveVersions(ctx(), syncID, nodeID, 0)
	if err != nil {
		t.Fatalf("ListSaveVersions: %v", err)
	}
	for _, v := range vs {
		if v.Hash == hash {
			return v.Seq
		}
	}
	t.Fatalf("no version with hash %q for %s/%s", hash, syncID, nodeID)
	return 0
}

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

// mustSync creates a sync carrying the given free-text game label so
// manifest/sync_log rows — sync_id-keyed — have a valid parent. The label is
// free text (no games table), so it is just set on the sync row.
func mustSync(t *testing.T, s store.Store, syncID, gameLabel string) {
	t.Helper()
	must(t, s.CreateSync(ctx(), store.Sync{ID: syncID, Game: gameLabel}))
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
