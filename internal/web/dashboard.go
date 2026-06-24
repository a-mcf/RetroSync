package web

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/a-mcf/retrosync/internal/store"
)

// dashboardData is the full view-model for GET / and the resolve-conflict
// refresh fragment. CSRF is the per-session token embedded so the resolve POST
// can echo it back.
type dashboardData struct {
	User    userView
	MySyncs []mySyncRow
	Nodes   []nodeRow
	CSRF    string
}

// userView is a hash-free projection of store.User for the template. It
// deliberately omits PwHash so no template or serialization path can ever reach
// the password hash (defense-in-depth).
type userView struct {
	ID      string
	Display string
	Role    store.Role
}

// mySyncRow is one "My syncs" STATUS card: a sync with a member on a node I own.
// Under auto-mirror there are no Play buttons — the card shows each member's
// last-known mtime and the sync's state (in sync / conflict). A Resolve button
// appears ONLY when the sync is in conflict.
type mySyncRow struct {
	SyncID   string
	SyncName string
	// Game is the sync's free-text game label (a display grouping; "" when unset).
	Game  string
	Nodes []nodeMtimeLine
	// Conflict is true when the sync forked (conflict_at set); the row shows a
	// red "sync paused" banner with a Resolve button.
	Conflict bool
	// LastSync is the coarse "all in sync N ago" relative time, or "never". Only
	// meaningful when !Conflict.
	LastSync string
}

// nodeMtimeLine is one member node's last-known manifest mtime on a status card.
type nodeMtimeLine struct {
	NodeID string
	Mtime  string
	// Owned marks a line that maps to a node the current user owns (rendered with
	// a subtle "(yours)" marker so the user can see which devices are theirs).
	Owned bool
}

// nodeRow is one node-status card.
type nodeRow struct {
	ID        string
	Display   string
	Reachable bool
	LastSeen  string
}

// buildDashboard assembles the read-only dashboard view-model for user u from
// the Store. It performs only reads. Any single sub-query failure is fatal to
// the page (returns the error); the handler turns that into a 500.
func (s *Server) buildDashboard(ctx context.Context, u store.User) (dashboardData, error) {
	now := s.now()

	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return dashboardData{}, fmt.Errorf("list nodes: %w", err)
	}

	data := dashboardData{User: userView{ID: u.ID, Display: u.Display, Role: u.Role}}

	// --- My syncs (status) ---
	// Nodes owned by this user.
	myNodes := make(map[string]bool)
	for _, n := range nodes {
		if n.OwnerUserID != nil && *n.OwnerUserID == u.ID {
			myNodes[n.ID] = true
		}
	}
	// A sync is "mine" when at least one of its members lives on a node I own.
	mySyncIDs := make(map[string]bool)
	for nodeID := range myNodes {
		members, err := s.store.ListSyncMembersByNode(ctx, nodeID)
		if err != nil {
			return dashboardData{}, fmt.Errorf("list members by node %s: %w", nodeID, err)
		}
		for _, m := range members {
			mySyncIDs[m.SyncID] = true
		}
	}

	for syncID := range mySyncIDs {
		sy, err := s.store.GetSync(ctx, syncID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return dashboardData{}, fmt.Errorf("get sync %s: %w", syncID, err)
		}
		members, err := s.store.ListSyncMembers(ctx, syncID)
		if err != nil {
			return dashboardData{}, fmt.Errorf("list members %s: %w", syncID, err)
		}
		var lines []nodeMtimeLine
		for _, m := range members {
			mtime := "no save yet"
			if me, err := s.store.GetManifest(ctx, syncID, m.NodeID); err == nil {
				mtime = fmtMtime(me.Mtime)
			} else if !errors.Is(err, store.ErrNotFound) {
				return dashboardData{}, fmt.Errorf("get manifest %s/%s: %w", syncID, m.NodeID, err)
			}
			lines = append(lines, nodeMtimeLine{
				NodeID: m.NodeID,
				Mtime:  mtime,
				Owned:  myNodes[m.NodeID],
			})
		}
		sort.Slice(lines, func(i, j int) bool { return lines[i].NodeID < lines[j].NodeID })
		data.MySyncs = append(data.MySyncs, mySyncRow{
			SyncID:   syncID,
			SyncName: sy.Name,
			Game:     sy.Game,
			Nodes:    lines,
			Conflict: sy.ConflictAt != nil,
			LastSync: fmtTimeAgo(sy.LastSynced, now),
		})
	}
	// Deterministic order: by game label then sync id.
	sort.Slice(data.MySyncs, func(i, j int) bool {
		if data.MySyncs[i].Game != data.MySyncs[j].Game {
			return data.MySyncs[i].Game < data.MySyncs[j].Game
		}
		return data.MySyncs[i].SyncID < data.MySyncs[j].SyncID
	})

	// --- Node status ---
	for _, n := range nodes {
		data.Nodes = append(data.Nodes, nodeRow{
			ID:        n.ID,
			Display:   n.Display,
			Reachable: n.LastSeenAt != nil,
			LastSeen:  fmtTimeAgo(n.LastSeenAt, now),
		})
	}

	return data, nil
}

// fmtMtime renders a manifest mtime, or the "no save yet" sentinel when unknown.
func fmtMtime(t *time.Time) string {
	if t == nil {
		return "no save yet"
	}
	return t.Format("2006-01-02 15:04")
}

// fmtSize renders a byte count as a compact human-readable size (B / KB / MB),
// for the conflict modal's per-node disclosure. Save files are small; KB
// precision is plenty.
func fmtSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", n/1024)
	default:
		return fmt.Sprintf("%d MB", n/(1024*1024))
	}
}

// fmtTimeAgo renders a coarse "N ago" relative time; "never" for nil/zero.
func fmtTimeAgo(t *time.Time, now time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	d := now.Sub(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}
