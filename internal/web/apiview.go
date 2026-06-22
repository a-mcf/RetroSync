package web

import (
	"context"
	"fmt"
	"time"

	"github.com/a-mcf/retrosync/internal/store"
)

// JSON view-models. These mirror the shapes documented in docs/api.md. They are
// separate from the store types so the wire format is stable and decoupled from
// the persistence layer. Times are RFC3339 strings (or null) for a clean,
// machine-readable contract.

// statusResponse is the body of GET /api/status.
type statusResponse struct {
	Active []statusActive `json:"active"`
	Nodes  []statusNode   `json:"nodes"`
}

type statusActive struct {
	SyncID      string `json:"sync_id"`
	PrimaryNode string `json:"primary_node"`
	Since       string `json:"since"`
	Conflict    bool   `json:"conflict"`
}

type statusNode struct {
	ID        string  `json:"id"`
	Reachable bool    `json:"reachable"`
	LastSeen  *string `json:"last_seen,omitempty"`
}

func (s *Server) buildStatus(ctx context.Context) (statusResponse, error) {
	bindings, err := s.store.ListBindings(ctx)
	if err != nil {
		return statusResponse{}, fmt.Errorf("list bindings: %w", err)
	}
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return statusResponse{}, fmt.Errorf("list nodes: %w", err)
	}

	out := statusResponse{
		Active: make([]statusActive, 0, len(bindings)),
		Nodes:  make([]statusNode, 0, len(nodes)),
	}
	for _, b := range bindings {
		out.Active = append(out.Active, statusActive{
			SyncID:      b.SyncID,
			PrimaryNode: b.PrimaryNode,
			Since:       b.StartedAt.UTC().Format(time.RFC3339),
			Conflict:    b.ConflictAt != nil,
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

// gameResponse is one element of GET /api/games.
type gameResponse struct {
	ID      string           `json:"id"`
	Display string           `json:"display"`
	System  string           `json:"system"`
	Active  *gameActive      `json:"active"`
	Paths   []gamePathOutput `json:"paths"`
}

type gameActive struct {
	PrimaryNode string `json:"primary_node"`
	Since       string `json:"since"`
}

type gamePathOutput struct {
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

		// A game is "active" when any of its syncs has an active binding. The
		// binding is sync-keyed now, so we surface the primary of the first active
		// sync (TODO(slice-registry-sync): the /api/games registry view moves onto
		// per-sync state; for now this preserves the "is this title in play" hint).
		syncs, err := s.store.ListSyncsByGame(ctx, g.ID)
		if err != nil {
			return nil, fmt.Errorf("list syncs %s: %w", g.ID, err)
		}
		for _, sy := range syncs {
			if b, err := s.store.GetBinding(ctx, sy.ID); err == nil {
				gr.Active = &gameActive{
					PrimaryNode: b.PrimaryNode,
					Since:       b.StartedAt.UTC().Format(time.RFC3339),
				}
				break
			} else if err != store.ErrNotFound {
				return nil, fmt.Errorf("get binding %s: %w", sy.ID, err)
			}
		}

		paths, err := s.store.ListGamePathsByGame(ctx, g.ID)
		if err != nil {
			return nil, fmt.Errorf("list paths %s: %w", g.ID, err)
		}
		gr.Paths = make([]gamePathOutput, 0, len(paths))
		for _, p := range paths {
			// TODO(slice-registry-sync): the manifest is now sync-keyed, so there is
			// no per-(game,node) mtime to surface here. The registry game-paths JSON
			// keeps the mtime FIELD (always present, per the api.md contract) but it
			// is null until the registry view is re-pointed onto syncs.
			gr.Paths = append(gr.Paths, gamePathOutput{NodeID: p.NodeID, Path: p.Path})
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
