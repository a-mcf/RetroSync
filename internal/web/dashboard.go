package web

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/a-mcf/retrosync/internal/store"
)

// dashboardData is the full view-model for GET / (read-only this slice).
type dashboardData struct {
	User    userView
	Active  []activeRow
	MyGames []myGameRow
	Nodes   []nodeRow
}

// userView is a hash-free projection of store.User for the template. It
// deliberately omits PwHash so no template or serialization path can ever reach
// the password hash (defense-in-depth).
type userView struct {
	ID      string
	Display string
	Role    store.Role
}

// activeRow is one active-binding card.
type activeRow struct {
	GameID      string
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

// myGameRow is one "my games" card: a game with a path on a node I own, with a
// per-node last-known mtime line.
type myGameRow struct {
	GameID      string
	GameDisplay string
	System      string
	Nodes       []nodeMtimeLine
}

type nodeMtimeLine struct {
	NodeID string
	Mtime  string
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

	// --- Active bindings ---
	bindings, err := s.store.ListBindings(ctx)
	if err != nil {
		return dashboardData{}, fmt.Errorf("list bindings: %w", err)
	}
	for _, b := range bindings {
		row := activeRow{
			GameID:      b.GameID,
			GameDisplay: displayOf(gameByID, b.GameID),
			System:      systemOf(gameByID, b.GameID),
			PrimaryNode: b.PrimaryNode,
			Since:       fmtTime(&b.StartedAt),
			Conflict:    b.ConflictAt != nil,
			LastSync:    fmtTimeAgo(b.LastSynced, now),
		}
		// Peers: every node (other than the primary) that has a manifest entry
		// for this game, with its last-known mtime.
		manifest, err := s.store.ListManifestByGame(ctx, b.GameID)
		if err != nil {
			return dashboardData{}, fmt.Errorf("list manifest %s: %w", b.GameID, err)
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

	// --- My games ---
	// Nodes owned by this user.
	myNodes := make(map[string]bool)
	for _, n := range nodes {
		if n.OwnerUserID != nil && *n.OwnerUserID == u.ID {
			myNodes[n.ID] = true
		}
	}
	// For each game, collect path mappings that land on one of my nodes.
	for _, g := range games {
		paths, err := s.store.ListGamePathsByGame(ctx, g.ID)
		if err != nil {
			return dashboardData{}, fmt.Errorf("list paths %s: %w", g.ID, err)
		}
		var lines []nodeMtimeLine
		for _, p := range paths {
			if !myNodes[p.NodeID] {
				continue
			}
			mtime := "no save yet"
			if m, err := s.store.GetManifest(ctx, g.ID, p.NodeID); err == nil {
				mtime = fmtMtime(m.Mtime)
			} else if !errors.Is(err, store.ErrNotFound) {
				return dashboardData{}, fmt.Errorf("get manifest %s/%s: %w", g.ID, p.NodeID, err)
			}
			lines = append(lines, nodeMtimeLine{NodeID: p.NodeID, Mtime: mtime})
		}
		if len(lines) == 0 {
			continue
		}
		sort.Slice(lines, func(i, j int) bool { return lines[i].NodeID < lines[j].NodeID })
		data.MyGames = append(data.MyGames, myGameRow{
			GameID:      g.ID,
			GameDisplay: g.Display,
			System:      g.System,
			Nodes:       lines,
		})
	}

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
