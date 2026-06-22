package daemon_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/daemon"
	"github.com/a-mcf/retrosync/internal/store"
)

// --- stubs ---------------------------------------------------------------

// stubLister returns a fixed set of syncs, or an error.
type stubLister struct {
	syncs []store.Sync
	err   error
}

func (s stubLister) ListSyncs(context.Context) ([]store.Sync, error) {
	return s.syncs, s.err
}

// stubPoller records which sync ids were polled and can be told to fail for a
// specific sync. Safe for concurrent use, though the daemon polls serially.
type stubPoller struct {
	mu      sync.Mutex
	polled  []string
	failFor map[string]error
}

func newStubPoller() *stubPoller {
	return &stubPoller{failFor: map[string]error{}}
}

func (p *stubPoller) Poll(_ context.Context, syncID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.polled = append(p.polled, syncID)
	return p.failFor[syncID]
}

func (p *stubPoller) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.polled))
	copy(out, p.polled)
	return out
}

// panicPoller panics for one specific sync id and records every sync it was
// asked to poll. It models a Poller that blows up (nil map, slice bounds, an
// SSH-lib panic) for one sync, so we can assert the sweep survives it.
type panicPoller struct {
	mu       sync.Mutex
	polled   []string
	panicFor string
}

func (p *panicPoller) Poll(_ context.Context, syncID string) error {
	p.mu.Lock()
	p.polled = append(p.polled, syncID)
	p.mu.Unlock()
	if syncID == p.panicFor {
		panic("kaboom from " + syncID)
	}
	return nil
}

func (p *panicPoller) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.polled))
	copy(out, p.polled)
	return out
}

// blockingPoller blocks until its ctx is cancelled for one specific sync id
// (modeling a hung node), recording the resulting ctx error for that sync, and
// polls every other sync normally. Deterministic: it blocks on ctx.Done, not a
// fixed sleep, so the per-poll timeout is what releases it.
type blockingPoller struct {
	mu       sync.Mutex
	polled   []string
	blockFor string
	blockErr error
}

func (p *blockingPoller) Poll(ctx context.Context, syncID string) error {
	p.mu.Lock()
	p.polled = append(p.polled, syncID)
	p.mu.Unlock()
	if syncID == p.blockFor {
		<-ctx.Done()
		p.mu.Lock()
		p.blockErr = ctx.Err()
		p.mu.Unlock()
		return ctx.Err()
	}
	return nil
}

func (p *blockingPoller) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.polled))
	copy(out, p.polled)
	return out
}

func syncs(ids ...string) []store.Sync {
	out := make([]store.Sync, len(ids))
	for i, id := range ids {
		out[i] = store.Sync{ID: id, GameID: "g1"}
	}
	return out
}

// --- RunOnce -------------------------------------------------------------

func TestRunOncePollsEverySyncOnce(t *testing.T) {
	lister := stubLister{syncs: syncs("a", "b", "c")}
	poller := newStubPoller()
	d := daemon.New(lister, poller, time.Second, 0, nil)

	res, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Polled != 3 || res.Errors != 0 {
		t.Fatalf("result = %+v, want {Polled:3 Errors:0}", res)
	}
	got := poller.calls()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("polled %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("polled %v, want %v", got, want)
		}
	}
}

func TestRunOnceIsolatesPerSyncError(t *testing.T) {
	lister := stubLister{syncs: syncs("a", "b", "c")}
	poller := newStubPoller()
	poller.failFor["b"] = errors.New("boom")
	d := daemon.New(lister, poller, time.Second, 0, nil)

	res, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce returned error %v; per-sync errors must be isolated", err)
	}
	if res.Polled != 3 {
		t.Fatalf("Polled = %d, want 3 (all syncs attempted despite b failing)", res.Polled)
	}
	if res.Errors != 1 {
		t.Fatalf("Errors = %d, want 1", res.Errors)
	}
	// Every sync including the ones after the failing one were still polled.
	if len(poller.calls()) != 3 {
		t.Fatalf("polled %v, want all three despite b's error", poller.calls())
	}
}

func TestRunOnceRecoversPanicAndContinues(t *testing.T) {
	lister := stubLister{syncs: syncs("a", "b", "c")}
	poller := &panicPoller{panicFor: "b"}
	d := daemon.New(lister, poller, time.Second, 0, nil)

	// The sweep must NOT propagate the panic: if recover() were missing, RunOnce
	// (and the whole daemon) would crash here instead of returning.
	res, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce returned error %v; a per-sync panic must be isolated", err)
	}
	if res.Polled != 3 {
		t.Fatalf("Polled = %d, want 3 (all syncs attempted despite b panicking)", res.Polled)
	}
	if res.Errors != 1 {
		t.Fatalf("Errors = %d, want 1 (the panic counted as a single failure)", res.Errors)
	}
	// Every sync including the one after the panicking one was still polled.
	got := poller.calls()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("polled %v, want %v despite b's panic", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("polled %v, want %v", got, want)
		}
	}
}

func TestRunOncePerPollTimeoutDoesNotStallSweep(t *testing.T) {
	lister := stubLister{syncs: syncs("a", "b", "c")}
	poller := &blockingPoller{blockFor: "a"} // first sync hangs
	// Tiny timeout so the hung poll is released deterministically by the deadline.
	d := daemon.New(lister, poller, time.Second, 5*time.Millisecond, nil)

	res, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Polled != 3 {
		t.Fatalf("Polled = %d, want 3 (sweep continues past the hung sync)", res.Polled)
	}
	if res.Errors != 1 {
		t.Fatalf("Errors = %d, want 1 (the hung sync records a timeout error)", res.Errors)
	}
	// The blocked sync observed a deadline-exceeded cancellation.
	poller.mu.Lock()
	blockErr := poller.blockErr
	poller.mu.Unlock()
	if !errors.Is(blockErr, context.DeadlineExceeded) {
		t.Fatalf("blocked game err = %v, want context.DeadlineExceeded", blockErr)
	}
	// The syncs AFTER the hung one were still polled.
	got := poller.calls()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("polled %v, want %v despite a hanging", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("polled %v, want %v", got, want)
		}
	}
}

func TestRunOnceNoSyncsIsNoop(t *testing.T) {
	lister := stubLister{syncs: nil}
	poller := newStubPoller()
	d := daemon.New(lister, poller, time.Second, 0, nil)

	res, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Polled != 0 || res.Errors != 0 {
		t.Fatalf("result = %+v, want zero", res)
	}
	if len(poller.calls()) != 0 {
		t.Fatalf("polled %v, want none", poller.calls())
	}
}

func TestRunOnceListErrorReturned(t *testing.T) {
	listErr := errors.New("db down")
	lister := stubLister{err: listErr}
	poller := newStubPoller()
	d := daemon.New(lister, poller, time.Second, 0, nil)

	_, err := d.RunOnce(context.Background())
	if !errors.Is(err, listErr) {
		t.Fatalf("err = %v, want %v", err, listErr)
	}
	if len(poller.calls()) != 0 {
		t.Fatalf("polled %v, want none when list fails", poller.calls())
	}
}

func TestRunOnceHonorsCancellationBetweenSyncs(t *testing.T) {
	lister := stubLister{syncs: syncs("a", "b", "c")}
	poller := newStubPoller()
	d := daemon.New(lister, poller, time.Second, 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the sweep starts

	res, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// No syncs should be polled: the loop checks ctx before each Poll.
	if res.Polled != 0 {
		t.Fatalf("Polled = %d, want 0 with a pre-cancelled ctx", res.Polled)
	}
}

// --- Run loop with an injected ticker ------------------------------------

// fakeTicker delivers ticks on demand via tick(), so Run is driven
// deterministically with no real-time sleeps.
type fakeTicker struct {
	ch      chan time.Time
	stopped chan struct{}
	once    sync.Once
}

func newFakeTicker() *fakeTicker {
	return &fakeTicker{ch: make(chan time.Time, 1), stopped: make(chan struct{})}
}

func (f *fakeTicker) C() <-chan time.Time { return f.ch }
func (f *fakeTicker) Stop()               { f.once.Do(func() { close(f.stopped) }) }

// tick delivers one tick (blocking until the loop is ready to receive it).
func (f *fakeTicker) tick() { f.ch <- time.Now() }

func TestRunDoesImmediateSweepThenPerTick(t *testing.T) {
	lister := stubLister{syncs: syncs("a")}
	poller := newStubPoller()
	d := daemon.New(lister, poller, time.Hour, 0, nil)

	ft := newFakeTicker()
	daemon.SetNewTicker(d, func(time.Duration) daemon.Ticker { return ft })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Drive N tick-triggered sweeps. Plus the immediate startup sweep, that's
	// N+1 sweeps and N+1 polls of sync "a". We synchronize by waiting for the
	// poll count to advance after each tick, with a deadline to avoid hangs.
	const ticks = 3
	waitPolls := func(n int) {
		deadline := time.After(2 * time.Second)
		for {
			if len(poller.calls()) >= n {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("timed out waiting for %d polls; got %d", n, len(poller.calls()))
			case <-time.After(time.Millisecond):
			}
		}
	}

	waitPolls(1) // immediate startup sweep
	for i := 1; i <= ticks; i++ {
		ft.tick()
		waitPolls(1 + i)
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancellation (possible goroutine leak / hang)")
	}

	if got := len(poller.calls()); got != ticks+1 {
		t.Fatalf("polled %d times, want %d (1 immediate + %d ticks)", got, ticks+1, ticks)
	}

	// The ticker must have been stopped on Run's return (no leak).
	select {
	case <-ft.stopped:
	default:
		t.Fatal("ticker was not stopped on Run return")
	}
}

func TestRunReturnsPromptlyOnImmediateCancel(t *testing.T) {
	lister := stubLister{syncs: syncs("a", "b")}
	poller := newStubPoller()
	d := daemon.New(lister, poller, time.Hour, 0, nil)

	ft := newFakeTicker()
	daemon.SetNewTicker(d, func(time.Duration) daemon.Ticker { return ft })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run starts

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly with a pre-cancelled ctx")
	}
}
