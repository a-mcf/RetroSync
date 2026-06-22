package web

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/a-mcf/retrosync/internal/store"
)

// dashboardData is the full view-model for GET / and the action refresh
// fragment. CSRF is the per-session token embedded so every action POST
// (Play / Done / takeover) can echo it back.
type dashboardData struct {
	User    userView
	Active  []activeRow
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

// activeRow is one active-binding card, keyed by sync.
type activeRow struct {
	SyncID      string
	SyncName    string
	GameDisplay string
	System      string
	PrimaryNode string
	Since       string
	Conflict    bool
	LastSync    string
	Peers       []peerLine
}

// peerLine is one mirrored node under an active binding, with its last-known
// mtime from the manifest.
type peerLine struct {
	NodeID string
	Mtime  string
}

// mySyncRow is one "my syncs" card: a sync with a member on a node I own, with a
// per-node last-known mtime line and a "Play on <node>" (or "Take over") action
// per owned member node.
type mySyncRow struct {
	SyncID      string
	SyncName    string
	GameDisplay string
	System      string
	Nodes       []nodeMtimeLine
	// ActivePrimary is the node currently bound as primary for this sync, or ""
	// if the sync is idle. When set and != an owned node, the Play button reads
	// "Take over from <ActivePrimary>" and posts force=true.
	ActivePrimary string
	// Conflict is true when the sync's active binding is in conflict; the row
	// shows a "Resolve conflict" banner.
	Conflict bool
	// MultiSource is true when more than one node is a member of this sync, so
	// the Play button opens the "use my save" modal (hx-get) instead of posting
	// directly (docs/ui.md: modal only appears with multiple saves/sources).
	MultiSource bool
}

type nodeMtimeLine struct {
	NodeID string
	Mtime  string
	// Owned marks a line that maps to a node the current user owns: only these
	// get an action button (docs/ui.md "Play on <node>" per owned node).
	Owned bool
	// Takeover is true when this owned node would take over a session currently
	// primaried on a different node (button reads "Take over from <other>" and
	// posts force=true).
	Takeover bool
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

	games, err := s.store.ListGames(ctx, store.GameFilter{})
	if err != nil {
		return dashboardData{}, fmt.Errorf("list games: %w", err)
	}
	gameByID := make(map[string]store.Game, len(games))
	for _, g := range games {
		gameByID[g.ID] = g
	}

	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return dashboardData{}, fmt.Errorf("list nodes: %w", err)
	}

	data := dashboardData{User: userView{ID: u.ID, Display: u.Display, Role: u.Role}}

	// --- Active bindings (keyed by sync) ---
	bindings, err := s.store.ListBindings(ctx)
	if err != nil {
		return dashboardData{}, fmt.Errorf("list bindings: %w", err)
	}
	// bindingBySync: sync_id -> its active binding. Used in the "my syncs"
	// section to decide Play vs "Take over from <other>" and the conflict banner.
	bindingBySync := make(map[string]store.ActiveBinding, len(bindings))
	for _, b := range bindings {
		bindingBySync[b.SyncID] = b
	}
	for _, b := range bindings {
		sy, gErr := s.store.GetSync(ctx, b.SyncID)
		if gErr != nil && !errors.Is(gErr, store.ErrNotFound) {
			return dashboardData{}, fmt.Errorf("get sync %s: %w", b.SyncID, gErr)
		}
		row := activeRow{
			SyncID:      b.SyncID,
			SyncName:    sy.Name,
			GameDisplay: displayOf(gameByID, sy.GameID),
			System:      systemOf(gameByID, sy.GameID),
			PrimaryNode: b.PrimaryNode,
			Since:       fmtTime(&b.StartedAt),
			Conflict:    b.ConflictAt != nil,
			LastSync:    fmtTimeAgo(b.LastSynced, now),
		}
		// Peers: every node (other than the primary) that has a manifest entry
		// for this sync, with its last-known mtime.
		manifest, err := s.store.ListManifestBySync(ctx, b.SyncID)
		if err != nil {
			return dashboardData{}, fmt.Errorf("list manifest %s: %w", b.SyncID, err)
		}
		for _, m := range manifest {
			if m.NodeID == b.PrimaryNode {
				continue
			}
			row.Peers = append(row.Peers, peerLine{NodeID: m.NodeID, Mtime: fmtMtime(m.Mtime)})
		}
		sort.Slice(row.Peers, func(i, j int) bool { return row.Peers[i].NodeID < row.Peers[j].NodeID })
		data.Active = append(data.Active, row)
	}
	sort.Slice(data.Active, func(i, j int) bool { return data.Active[i].SyncID < data.Active[j].SyncID })

	// --- My syncs ---
	// Nodes owned by this user.
	myNodes := make(map[string]bool)
	for _, n := range nodes {
		if n.OwnerUserID != nil && *n.OwnerUserID == u.ID {
			myNodes[n.ID] = true
		}
	}
	// A sync is "mine" when at least one of its members lives on a node I own.
	// ListSyncMembersByNode for each of my nodes yields the candidate sync ids.
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
		b, hasBinding := bindingBySync[syncID]
		activePrimary := ""
		conflict := false
		if hasBinding {
			activePrimary = b.PrimaryNode
			conflict = b.ConflictAt != nil
		}
		var lines []nodeMtimeLine
		for _, m := range members {
			mtime := "no save yet"
			if me, err := s.store.GetManifest(ctx, syncID, m.NodeID); err == nil {
				mtime = fmtMtime(me.Mtime)
			} else if !errors.Is(err, store.ErrNotFound) {
				return dashboardData{}, fmt.Errorf("get manifest %s/%s: %w", syncID, m.NodeID, err)
			}
			owned := myNodes[m.NodeID]
			// Takeover when the sync is active on a DIFFERENT node than this one.
			takeover := owned && activePrimary != "" && activePrimary != m.NodeID
			lines = append(lines, nodeMtimeLine{
				NodeID:   m.NodeID,
				Mtime:    mtime,
				Owned:    owned,
				Takeover: takeover,
			})
		}
		sort.Slice(lines, func(i, j int) bool { return lines[i].NodeID < lines[j].NodeID })
		data.MySyncs = append(data.MySyncs, mySyncRow{
			SyncID:        syncID,
			SyncName:      sy.Name,
			GameDisplay:   displayOf(gameByID, sy.GameID),
			System:        systemOf(gameByID, sy.GameID),
			Nodes:         lines,
			ActivePrimary: activePrimary,
			Conflict:      conflict,
			MultiSource:   len(members) > 1,
		})
	}
	// Deterministic order: by game display then sync id.
	sort.Slice(data.MySyncs, func(i, j int) bool {
		if data.MySyncs[i].GameDisplay != data.MySyncs[j].GameDisplay {
			return data.MySyncs[i].GameDisplay < data.MySyncs[j].GameDisplay
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

func displayOf(m map[string]store.Game, id string) string {
	if g, ok := m[id]; ok && g.Display != "" {
		return g.Display
	}
	return id
}

func systemOf(m map[string]store.Game, id string) string {
	if g, ok := m[id]; ok {
		return g.System
	}
	return ""
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

// fmtTime renders an absolute time; "—" for nil/zero.
func fmtTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04")
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
