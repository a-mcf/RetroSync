package web

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/a-mcf/retrosync/internal/store"
)

// JSON view-models. These mirror the shapes documented in docs/api.md. They are
// separate from the store types so the wire format is stable and decoupled from
// the persistence layer. Times are RFC3339 strings (or null) for a clean,
// machine-readable contract.

// statusResponse is the body of GET /api/status. Under auto-mirror there is no
// "active session": every sync mirrors. The `syncs` array reports each sync's
// runtime state (conflicted, last_synced) so a client can see what is paused.
type statusResponse struct {
	Syncs []statusSync `json:"syncs"`
	Nodes []statusNode `json:"nodes"`
}

type statusSync struct {
	SyncID     string  `json:"sync_id"`
	GameID     string  `json:"game_id"`
	Conflict   bool    `json:"conflict"`
	ConflictAt *string `json:"conflict_at"`
	LastSynced *string `json:"last_synced"`
}

type statusNode struct {
	ID        string  `json:"id"`
	Reachable bool    `json:"reachable"`
	LastSeen  *string `json:"last_seen,omitempty"`
}

func (s *Server) buildStatus(ctx context.Context) (statusResponse, error) {
	syncs, err := s.store.ListSyncs(ctx)
	if err != nil {
		return statusResponse{}, fmt.Errorf("list syncs: %w", err)
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return statusResponse{}, fmt.Errorf("list nodes: %w", err)
	}

	out := statusResponse{
		Syncs: make([]statusSync, 0, len(syncs)),
		Nodes: make([]statusNode, 0, len(nodes)),
	}
	for _, sy := range syncs {
		out.Syncs = append(out.Syncs, statusSync{
			SyncID:     sy.ID,
			GameID:     sy.GameID,
			Conflict:   sy.ConflictAt != nil,
			ConflictAt: rfc3339Ptr(sy.ConflictAt),
			LastSynced: rfc3339Ptr(sy.LastSynced),
		})
	}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, statusNode{
			ID:        n.ID,
			Reachable: n.LastSeenAt != nil,
			LastSeen:  rfc3339Ptr(n.LastSeenAt),
		})
	}
	return out, nil
}

// gameResponse is one element of GET /api/games. With syncs now first-class,
// each game lists its syncs (id, name, members with node + path + last-known
// manifest mtime, and auto-mirror runtime state) — the registry view re-pointed
// onto syncs (slice 16).
type gameResponse struct {
	ID      string         `json:"id"`
	Display string         `json:"display"`
	System  string         `json:"system"`
	Syncs   []syncResponse `json:"syncs"`
}

// syncResponse is one sync of a game, with its auto-mirror runtime state.
type syncResponse struct {
	ID      string             `json:"id"`
	Name    string             `json:"name"`
	State   syncState          `json:"state"`
	Members []syncMemberOutput `json:"members"`
}

// syncState is a sync's auto-mirror runtime state: whether it is paused on a
// conflict, and when it last successfully synced.
type syncState struct {
	Conflict   bool    `json:"conflict"`
	ConflictAt *string `json:"conflict_at"`
	LastSynced *string `json:"last_synced"`
}

// syncMemberOutput is one member of a sync: its node, the save-file path, and
// the last-known manifest mtime (present-or-null) for that (sync, node).
type syncMemberOutput struct {
	NodeID string  `json:"node_id"`
	Path   string  `json:"path"`
	Mtime  *string `json:"mtime"`
}

func (s *Server) buildGames(ctx context.Context, f store.GameFilter) ([]gameResponse, error) {
	games, err := s.store.ListGames(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("list games: %w", err)
	}

	out := make([]gameResponse, 0, len(games))
	for _, g := range games {
		gr := gameResponse{ID: g.ID, Display: g.Display, System: g.System}

		syncs, err := s.store.ListSyncsByGame(ctx, g.ID)
		if err != nil {
			return nil, fmt.Errorf("list syncs %s: %w", g.ID, err)
		}
		gr.Syncs = make([]syncResponse, 0, len(syncs))
		for _, sy := range syncs {
			sr := syncResponse{
				ID:   sy.ID,
				Name: sy.Name,
				State: syncState{
					Conflict:   sy.ConflictAt != nil,
					ConflictAt: rfc3339Ptr(sy.ConflictAt),
					LastSynced: rfc3339Ptr(sy.LastSynced),
				},
			}

			members, err := s.store.ListSyncMembers(ctx, sy.ID)
			if err != nil {
				return nil, fmt.Errorf("list members %s: %w", sy.ID, err)
			}
			sr.Members = make([]syncMemberOutput, 0, len(members))
			for _, m := range members {
				out := syncMemberOutput{NodeID: m.NodeID, Path: m.Path}
				if me, err := s.store.GetManifest(ctx, sy.ID, m.NodeID); err == nil {
					out.Mtime = rfc3339Ptr(me.Mtime)
				} else if !errors.Is(err, store.ErrNotFound) {
					return nil, fmt.Errorf("get manifest %s/%s: %w", sy.ID, m.NodeID, err)
				}
				sr.Members = append(sr.Members, out)
			}
			gr.Syncs = append(gr.Syncs, sr)
		}
		out = append(out, gr)
	}
	return out, nil
}

// nodeResponse is one element of GET /api/nodes. Secrets (reach_config) are
// deliberately omitted this slice — the read-only dashboard never needs them
// and they must not leak. The registry-editing UI handles reach_config later.
type nodeResponse struct {
	ID          string  `json:"id"`
	OwnerUserID *string `json:"owner_user_id,omitempty"`
	Display     string  `json:"display"`
	Kind        string  `json:"kind"`
	Reach       string  `json:"reach"`
	LastSeen    *string `json:"last_seen,omitempty"`
}

func (s *Server) buildNodes(ctx context.Context) ([]nodeResponse, error) {
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	out := make([]nodeResponse, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeResponse{
			ID:          n.ID,
			OwnerUserID: n.OwnerUserID,
			Display:     n.Display,
			Kind:        string(n.Kind),
			Reach:       string(n.Reach),
			LastSeen:    rfc3339Ptr(n.LastSeenAt),
		})
	}
	return out, nil
}

// rfc3339Ptr formats *t as an RFC3339 string pointer, or nil.
func rfc3339Ptr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}
