// Package daemon drives the engine on a timer: it is the background ticker the
// engine deliberately does not own (see internal/engine, TODO(slice-daemon)).
//
// Every interval the daemon runs a "sweep": it lists the active bindings and
// calls Poll once per bound sync. Errors are isolated per sync — one sync's
// failed poll (or a surfaced conflict) is logged and never aborts the sweep or
// crashes the loop, since the loop is the only thing keeping every OTHER sync
// syncing. The sweep is idempotent (the engine re-stats and resumes), so a
// crash/restart simply picks up on the next tick (docs/state-machine.md,
// "Crash safety").
//
// The daemon depends on narrow interfaces (Poller, BindingLister), not the
// concrete *engine.Engine or *store.Store, so its logic is trivially testable
// with stubs and the tick source is injectable for deterministic tests.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/a-mcf/retrosync/internal/store"
)

// defaultPollTimeout bounds a single sync's Poll. Saves are tiny, so this only
// ever fires on a genuinely hung node; it stops one stuck poll from stalling
// the serial sweep forever. Overridable via New's pollTimeout argument.
const defaultPollTimeout = 60 * time.Second

// Poller runs one sync pass for a single sync. *engine.Engine satisfies this
// via its Poll method. A conflict is not an error here: the engine flags the
// binding and returns nil; the daemon keeps sweeping. The id passed is the
// active binding's sync id.
type Poller interface {
	Poll(ctx context.Context, syncID string) error
}

// BindingLister enumerates the active bindings (the syncs to poll). store.Store
// satisfies this via ListBindings.
type BindingLister interface {
	ListBindings(ctx context.Context) ([]store.ActiveBinding, error)
}

// Ticker is the minimal slice of time.Ticker the daemon uses. Injecting it
// lets tests drive sweeps deterministically without real sleeps.
type Ticker interface {
	// C is the channel on which ticks are delivered.
	C() <-chan time.Time
	// Stop releases the ticker's resources.
	Stop()
}

// NewTicker constructs a Ticker firing every interval. The daemon defaults to
// realTicker (a thin time.Ticker wrapper); tests inject a fake.
type NewTicker func(interval time.Duration) Ticker

// Daemon polls every active binding on a fixed interval. Construct with New.
type Daemon struct {
	lister      BindingLister
	poller      Poller
	interval    time.Duration
	pollTimeout time.Duration
	logger      *slog.Logger
	newTicker   NewTicker
}

// New builds a Daemon. A nil logger discards output (slog.New of a discard
// handler) so callers and tests need not supply one. interval must be > 0 for
// Run; RunOnce ignores it. pollTimeout bounds each individual game's Poll; a
// value <= 0 falls back to defaultPollTimeout.
func New(lister BindingLister, poller Poller, interval, pollTimeout time.Duration, logger *slog.Logger) *Daemon {
	if logger == nil {
		logger = slog.New(discardHandler{})
	}
	if pollTimeout <= 0 {
		pollTimeout = defaultPollTimeout
	}
	return &Daemon{
		lister:      lister,
		poller:      poller,
		interval:    interval,
		pollTimeout: pollTimeout,
		logger:      logger,
		newTicker:   realTickerFunc,
	}
}

// SweepResult summarizes one sweep, for tests and structured logging.
type SweepResult struct {
	// Polled is the number of bindings for which Poll was attempted.
	Polled int
	// Errors is the number of bindings whose Poll returned an error.
	Errors int
}

// RunOnce performs one sweep: list the active bindings and Poll each exactly
// once. Errors are isolated per sync (logged, counted, not propagated) so a
// single bad sync never aborts the sweep. ctx cancellation is honored between
// syncs. A failure to LIST the bindings is the one error returned, since
// without the list there is nothing to sweep.
func (d *Daemon) RunOnce(ctx context.Context) (SweepResult, error) {
	// If the ctx is already cancelled (e.g. shutdown raced the tick), don't even
	// issue the ListBindings query.
	if ctx.Err() != nil {
		return SweepResult{}, nil
	}

	bindings, err := d.lister.ListBindings(ctx)
	if err != nil {
		d.logger.ErrorContext(ctx, "sweep: list bindings failed", slog.String("err", err.Error()))
		return SweepResult{}, err
	}

	d.logger.DebugContext(ctx, "sweep start", slog.Int("active_bindings", len(bindings)))

	var res SweepResult
	for _, b := range bindings {
		// Honor cancellation between syncs so shutdown is prompt even with many
		// active bindings.
		if err := ctx.Err(); err != nil {
			d.logger.InfoContext(ctx, "sweep cancelled",
				slog.Int("polled", res.Polled), slog.Int("remaining", len(bindings)-res.Polled))
			return res, nil
		}

		res.Polled++
		if err := d.pollOne(ctx, b.SyncID); err != nil {
			// Per-sync error isolation: log and continue. A poll error (including a
			// timeout or a recovered panic) means the next sweep will re-stat and
			// retry; one broken sync must not stall every other sync.
			res.Errors++
			d.logger.ErrorContext(ctx, "poll failed",
				slog.String("sync_id", b.SyncID), slog.String("err", err.Error()))
			continue
		}
		d.logger.DebugContext(ctx, "polled", slog.String("sync_id", b.SyncID))
	}

	d.logger.DebugContext(ctx, "sweep done",
		slog.Int("polled", res.Polled), slog.Int("errors", res.Errors))
	return res, nil
}

// pollOne runs exactly one sync's Poll with two layers of isolation, so a
// single misbehaving sync can never take down the sweep or the daemon loop:
//
//   - a per-sync timeout (d.pollTimeout) so a hung Poll surfaces as a normal
//     deadline error and the sweep moves on, and
//   - a recover() that converts any panic inside Poll (nil map, slice bounds, a
//     future SSH-lib panic) into an ordinary error. The recovered value is kept
//     out of any secret-bearing context: only the panic value's default format
//     is included, never the ctx or binding internals.
func (d *Daemon) pollOne(ctx context.Context, syncID string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("poll panicked: %v", r)
		}
	}()

	pollCtx, cancel := context.WithTimeout(ctx, d.pollTimeout)
	defer cancel()

	return d.poller.Poll(pollCtx, syncID)
}

// Run loops: one sweep immediately, then one per interval tick, until ctx is
// cancelled. It returns ctx.Err() on shutdown (always non-nil — the loop only
// returns from the ctx.Done branch). The ticker is stopped on return, so there
// is no goroutine or timer leak.
func (d *Daemon) Run(ctx context.Context) error {
	d.logger.InfoContext(ctx, "daemon starting", slog.Duration("interval", d.interval))

	// Immediate first sweep so we don't wait a full interval to start syncing.
	if _, err := d.RunOnce(ctx); err != nil {
		// A list failure is transient (DB blip); keep looping rather than dying.
		d.logger.WarnContext(ctx, "initial sweep failed; continuing", slog.String("err", err.Error()))
	}

	t := d.newTicker(d.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			d.logger.InfoContext(ctx, "daemon stopping", slog.String("reason", ctx.Err().Error()))
			return ctx.Err()
		case <-t.C():
			if _, err := d.RunOnce(ctx); err != nil {
				d.logger.WarnContext(ctx, "sweep failed; continuing", slog.String("err", err.Error()))
			}
		}
	}
}

// --- real ticker ---------------------------------------------------------

// realTicker adapts *time.Ticker to the Ticker interface.
type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

func realTickerFunc(interval time.Duration) Ticker {
	return realTicker{t: time.NewTicker(interval)}
}

// --- discard logger ------------------------------------------------------

// discardHandler is a slog.Handler that drops every record, used when New is
// given a nil logger. (slog has no exported discard handler before Go 1.24.)
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }
