package engine

// Test-only exports for white-box assertions from the engine_test (black-box)
// package. These let the discovery tests exercise the unexported game-name
// inference helper and observe the per-node skip hook without widening the public
// API or changing discovery's read-only, no-error-on-bad-node contract.

// InferGameNameForTest exposes inferGameName for the table test.
func InferGameNameForTest(filename string) string { return inferGameName(filename) }

// SetDiscoverSkipHookForTest installs (or clears, with nil) the per-node discovery
// skip hook so a test can assert which nodes were skipped and why.
func SetDiscoverSkipHookForTest(fn func(nodeID, reason string, err error)) {
	discoverSkipHook = fn
}
