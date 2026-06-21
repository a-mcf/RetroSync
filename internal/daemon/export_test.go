package daemon

// SetNewTicker overrides the daemon's ticker constructor for deterministic
// tests. It is exported only to the test package (export_test.go), never to
// production callers, so the injection seam stays test-only.
func SetNewTicker(d *Daemon, nt NewTicker) {
	d.newTicker = nt
}
