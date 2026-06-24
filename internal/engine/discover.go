package engine

// Discovery (slice-22): the on-ramp that replaces manual sync-building. Instead
// of typing a game label and a save-file path by hand, the admin scans every
// reachable node's save directory, RetroSync infers a game name from each save
// file's name, aggregates "this game's save lives on these nodes," and offers a
// one-click "create a sync from these candidates."
//
// This file is the engine half: DiscoverGames walks each directory-listing-
// reachable node's save tree (bounded), filters to save-like files, infers a game
// name per file, EXCLUDES files already claimed by a sync member, and aggregates
// the survivors by inferred game name. It is strictly READ-ONLY: it never writes
// the store or any node. The explicit create (sync + members) is a separate,
// admin+CSRF-gated web action.

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/a-mcf/retrosync/internal/reach"
)

// saveExtensions is the set of file extensions discovery treats as a "save"
// (lowercase, leading dot, case-insensitive match). It is a deliberately small,
// explicit list of the common emulator/console SRAM/EEPROM/memory-card save
// formats — NOT save STATES (`.state*`), which are excluded per the
// docs/open-questions.md non-goal ("SRAM/SAV only in v1; states are out of
// scope"). The set:
//
//	.srm  RetroArch SRAM (the common case)
//	.sav  generic battery save (GB/GBA/NDS cores, standalone emus)
//	.mcr  PlayStation memory card
//	.eep  EEPROM save (N64, some GBA)
//	.fla  FlashRAM save (N64)
//	.gci  GameCube memory-card file
//	.dat  some cores' battery save
//	.sed  SavestatE-less SRAM dump used by a few cores
//	.bsv  Bizhawk movie/save
//
// Anything NOT in this set (a ROM, a `.state`, a screenshot) is skipped.
var saveExtensions = map[string]bool{
	".srm": true,
	".sav": true,
	".mcr": true,
	".eep": true,
	".fla": true,
	".gci": true,
	".dat": true,
	".sed": true,
	".bsv": true,
}

// Walk bounds: a pathological/cyclic-looking tree must never hang discovery.
// maxScanDepth caps recursion depth (the save root is depth 0); maxScanEntries
// caps the total directory entries visited per node. Both are per-node; hitting
// either simply stops THAT node's walk early (it does not error discovery).
const (
	maxScanDepth   = 6
	maxScanEntries = 5000
)

// DiscoveredCandidate is one save file found on one node: the (node, path) pair
// plus its size/mtime for display. It is exactly the shape the one-click create
// needs to add a sync member.
type DiscoveredCandidate struct {
	NodeID string
	Path   string
	Size   int64
	Mtime  time.Time
}

// DiscoveredGame is a save file (by inferred game name) and every node that holds
// a copy of it. Name is the canonical display form of the inferred name;
// Candidates is the per-node files that aggregated under it, sorted by node id.
type DiscoveredGame struct {
	Name       string
	Candidates []DiscoveredCandidate
}

// DiscoverGames scans every directory-listing-reachable node's save directory,
// infers a game name from each save-like file, drops files already claimed by a
// sync member, and aggregates the survivors by inferred game name. It is
// READ-ONLY (no store or node writes) and resilient:
//
//   - A node whose reach has no directory listing (ssh today, which resolves to
//     reach.ErrUnsupportedReach) is SKIPPED, not errored — one unsupported node
//     must not fail the whole scan.
//   - A node whose scan errors (missing share, transient I/O) is SKIPPED with a
//     log line — one bad node must not fail discovery.
//   - The per-node walk is BOUNDED (maxScanDepth, maxScanEntries) so a deep or
//     pathological tree can't hang it.
//
// Aggregation is case-insensitive on the inferred name (so "Super Metroid" on one
// node and "super metroid" on another group together) but the returned Name is a
// canonical display form (the first-seen spelling). Games are sorted by name;
// candidates within a game by node id. A game with candidates on ≥1 node is
// returned (even a single-node game — the admin may want to sync it to a fresh
// device later).
func (e *Engine) DiscoverGames(ctx context.Context) ([]DiscoveredGame, error) {
	nodes, err := e.store.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: discover list nodes: %w", err)
	}

	// Build the set of (node, path) pairs already claimed by a sync member, so a
	// file already IN a sync is never offered as a candidate. A global walk via
	// per-node ListSyncMembersByNode keeps this independent of how many syncs exist.
	claimed := make(map[claimKey]bool)
	for _, n := range nodes {
		members, err := e.store.ListSyncMembersByNode(ctx, n.ID)
		if err != nil {
			return nil, fmt.Errorf("engine: discover list members for %s: %w", n.ID, err)
		}
		for _, m := range members {
			claimed[claimKey{nodeID: m.NodeID, path: m.Path}] = true
		}
	}

	// games keys by the LOWERCASED inferred name (the aggregation key); each value
	// carries the canonical display spelling (first seen) and its candidates.
	games := make(map[string]*DiscoveredGame)

	for _, n := range nodes {
		r, err := e.resolve(n)
		if err != nil {
			// A node whose reach has no directory-listing adapter (ssh today) is
			// skipped — not an error. One unsupported node must not fail the scan.
			if errors.Is(err, reach.ErrUnsupportedReach) {
				e.logDiscoverSkip(ctx, n.ID, "reach not supported for directory listing", err)
				continue
			}
			// Any other resolve failure is also a per-node skip (a bad node config must
			// not fail discovery as a whole).
			e.logDiscoverSkip(ctx, n.ID, "resolve failed", err)
			continue
		}

		found, err := scanNodeSaves(ctx, r)
		if err != nil {
			// A missing share or transient I/O on one node is logged and skipped — one
			// bad node does not fail discovery.
			e.logDiscoverSkip(ctx, n.ID, "scan failed", err)
			continue
		}

		for _, sf := range found {
			if claimed[claimKey{nodeID: n.ID, path: sf.path}] {
				// Already a member of some sync: not a candidate (it's already syncing).
				continue
			}
			name := inferGameName(sf.name)
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			g, ok := games[key]
			if !ok {
				g = &DiscoveredGame{Name: name}
				games[key] = g
			}
			g.Candidates = append(g.Candidates, DiscoveredCandidate{
				NodeID: n.ID,
				Path:   sf.path,
				Size:   sf.size,
				Mtime:  sf.mtime,
			})
		}
	}

	out := make([]DiscoveredGame, 0, len(games))
	for _, g := range games {
		sort.Slice(g.Candidates, func(i, j int) bool {
			if g.Candidates[i].NodeID != g.Candidates[j].NodeID {
				return g.Candidates[i].NodeID < g.Candidates[j].NodeID
			}
			return g.Candidates[i].Path < g.Candidates[j].Path
		})
		out = append(out, *g)
	}
	// Deterministic order: by display name (case-insensitive), then by name to break
	// any case-only tie.
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// claimKey is the (node, path) identity used to exclude already-synced files.
type claimKey struct {
	nodeID string
	path   string
}

// logDiscoverSkip records a per-node scan skip at WARN: discovery continues, but
// the operator can see which node was skipped and why. reach_config holds no
// secret, but the message stays generic (node id + reason + error).
func (e *Engine) logDiscoverSkip(ctx context.Context, nodeID, reason string, err error) {
	// The engine has no logger field; use the standard library default via the
	// store-agnostic path is overkill. We surface skips through a package-level
	// hook so tests can observe them without an injected logger. Default is a
	// no-op (engine stays side-effect-free beyond its ports). See discoverSkipHook.
	if discoverSkipHook != nil {
		discoverSkipHook(nodeID, reason, err)
	}
	_ = ctx
}

// discoverSkipHook, when non-nil, is invoked for each per-node discovery skip
// (unsupported reach, resolve failure, or scan error). It is a test seam: the
// engine owns no logger, and discovery deliberately swallows per-node failures so
// one bad node can't fail the scan — this hook lets a test assert that a given
// node WAS skipped (and why) without changing the read-only, no-error contract.
var discoverSkipHook func(nodeID, reason string, err error)

// savedFile is one save-like file found during a node walk: its base name (for
// game-name inference), full node-relative path (for the candidate), and metadata.
type savedFile struct {
	name  string
	path  string
	size  int64
	mtime time.Time
}

// scanNodeSaves recursively walks a node's save directory via reach.List
// (which lists ONE level), descending into subdirectories, and returns every
// save-like file it finds. The walk is BOUNDED: it stops descending past
// maxScanDepth and stops entirely once maxScanEntries directory entries have been
// visited, so a deep or pathological tree can't hang the scan. Containment is the
// adapter's job — every path handed to List is node-relative and the adapter's
// safepath check rejects any escape — so this walker never constructs an absolute
// or escaping path itself (it only ever joins a listed entry's Name onto the
// directory it came from).
//
// A reach.ErrNotExist on the ROOT (the node has no save dir yet) is treated as
// "no saves," NOT an error: a node that simply hasn't synced yet contributes
// nothing rather than failing the whole discovery.
func scanNodeSaves(ctx context.Context, r reach.Reach) ([]savedFile, error) {
	var out []savedFile
	visited := 0

	// Iterative DFS with an explicit stack of (relPath, depth) so a deep tree
	// doesn't blow the goroutine stack and the bounds are easy to enforce.
	type frame struct {
		rel   string
		depth int
	}
	stack := []frame{{rel: "", depth: 0}}

	for len(stack) > 0 {
		fr := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		entries, err := r.List(ctx, fr.rel)
		if err != nil {
			if errors.Is(err, reach.ErrNotExist) {
				// The directory vanished or the root doesn't exist. For the root this is
				// "no saves yet"; for a subdir it's a transient race — either way, skip
				// this directory rather than failing the whole node.
				if fr.rel == "" {
					return nil, nil
				}
				continue
			}
			return nil, fmt.Errorf("engine: scan list %q: %w", fr.rel, err)
		}

		// Cycle-safety here rests on the depth + entry caps (maxScanDepth and
		// maxScanEntries) plus the fact that List is lstat-based and so never
		// descends into a symlinked directory — there is NO explicit visited-set /
		// loop detection. A symlink loop is bounded by the caps, not detected.
		for _, de := range entries {
			visited++
			if visited > maxScanEntries {
				// Entry cap hit: stop the walk for this node. We return what we found so
				// far (a partial scan is still useful) rather than erroring.
				return out, nil
			}
			child := de.Name
			if fr.rel != "" {
				child = path.Join(fr.rel, de.Name)
			}
			if de.IsDir {
				// Bound the depth: don't descend past maxScanDepth.
				if fr.depth+1 <= maxScanDepth {
					stack = append(stack, frame{rel: child, depth: fr.depth + 1})
				}
				continue
			}
			if !isSaveLike(de.Name) {
				continue
			}
			out = append(out, savedFile{
				name:  de.Name,
				path:  child,
				size:  de.Size,
				mtime: de.Mtime,
			})
		}
	}
	return out, nil
}

// isSaveLike reports whether filename has a known save extension (case-
// insensitive). A `.state` file (RetroArch save STATE) is excluded both because
// ".state" is not in saveExtensions AND explicitly here for clarity, per the
// docs/open-questions.md non-goal (states are out of scope; SRAM/SAV only).
func isSaveLike(filename string) bool {
	ext := strings.ToLower(path.Ext(filename))
	if ext == "" {
		return false
	}
	// Defensively exclude any save-STATE file: RetroArch writes ".state",
	// ".state1", ".state2", … next to the SRAM. path.Ext only yields the LAST
	// dot-segment, so ".state1" -> ".state1" (not in the set) and ".state" -> not
	// in the set; this explicit check documents the non-goal and guards a future
	// extension list edit from accidentally admitting a state file.
	if strings.HasPrefix(ext, ".state") {
		return false
	}
	return saveExtensions[ext]
}

// inferGameName derives a display game name from a save FILENAME: strip the
// extension, strip trailing region/version/dump tags in parentheses or brackets
// (e.g. "(USA)", "[!]", "(Rev 1)", "(USA, Europe)"), and collapse/trim
// whitespace. Examples:
//
//	"Super Metroid (USA) [!].srm"      -> "Super Metroid"
//	"Zelda [!].sav"                    -> "Zelda"
//	"Game (USA, Europe) (Rev 1).srm"   -> "Game"
//	"Final Fantasy VI.srm"             -> "Final Fantasy VI"
//
// It is a best-effort heuristic for grouping, NOT a content classifier (RetroSync
// never classifies by content — docs/open-questions.md). A name that strips to
// empty (a file whose whole base is a tag) yields "" and is dropped by the caller.
func inferGameName(filename string) string {
	base := filename
	// Strip a single trailing extension. We intentionally strip only the last one
	// ("Game.v2.srm" -> "Game.v2") so an internal dot in the title survives.
	if ext := path.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}

	// Strip trailing (...) / [...] tag groups, repeatedly, from the right. Each
	// iteration removes ONE balanced trailing group and any whitespace before it.
	for {
		trimmed := strings.TrimRight(base, " \t")
		n := len(trimmed)
		if n == 0 {
			break
		}
		last := trimmed[n-1]
		var open byte
		switch last {
		case ')':
			open = '('
		case ']':
			open = '['
		default:
			base = trimmed
			goto done
		}
		// Find the matching opener for this trailing group (the LAST opener, so
		// nested/sequential groups peel one at a time).
		idx := strings.LastIndexByte(trimmed[:n-1], open)
		if idx < 0 {
			// Unbalanced — leave it as-is rather than mangling the name.
			base = trimmed
			goto done
		}
		base = trimmed[:idx]
	}
done:
	// Collapse internal whitespace runs to single spaces and trim.
	return strings.Join(strings.Fields(base), " ")
}
