// Package resolve maps a store.Node to the concrete reach.Reach adapter that
// talks to it. It is the production wiring the engine injects as its
// engine.ResolveReach: given a node, pick the right adapter from the node's
// reach strategy and reach_config.
//
// It lives in its own package (not in internal/reach) so it can import the
// concrete adapters (localfs today) without creating an import cycle —
// those adapters depend on internal/reach, which must stay adapter-free.
package resolve

import (
	"fmt"

	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/localfs"
	"github.com/a-mcf/retrosync/internal/store"
)

// ResolveReach returns the reach.Reach adapter for node. Its signature matches
// engine.ResolveReach so it can be passed directly to engine.New.
//
//   - syncthing-share: a localfs adapter rooted at reach_config.path. That path
//     is treated as the node's save ROOT; the engine's per-node game_paths.path
//     is then resolved beneath it by the adapter (via safepath). The data
//     model allows an extra save_root indirection between reach_config.path and
//     game_paths.path; for this slice we simplify to "reach_config.path IS the
//     save root" and join game_paths.path directly under it.
//     TODO(slice-save-root): honor a separate save_root once it exists in the
//     registry schema.
//   - anything else: reach.ErrUnsupportedReach. Both the CHECK constraint and
//     store.ValidReach limit `reach` to the strategies above, so an unrecognized
//     value is a data fault (a hand-edited row, or a database written by a newer
//     build). It is reported with the sentinel rather than a bare error so bulk
//     callers — discovery, the poll loop — skip that one node and keep going
//     instead of failing wholesale.
func ResolveReach(node store.Node) (reach.Reach, error) {
	switch node.Reach {
	case store.ReachSyncthingShare:
		root := node.ReachConfig.Path
		if root == "" {
			return nil, fmt.Errorf("resolve: node %q: syncthing-share reach_config.path is empty", node.ID)
		}
		r, err := localfs.New(root)
		if err != nil {
			return nil, fmt.Errorf("resolve: node %q: %w", node.ID, err)
		}
		return r, nil
	default:
		return nil, fmt.Errorf("resolve: node %q has unknown reach %q: %w",
			node.ID, node.Reach, reach.ErrUnsupportedReach)
	}
}
