// Command retrosync is the RetroSync server entrypoint. It connects to
// Postgres, runs registry migrations, then runs the play-sync daemon: a loop
// that polls every active binding on a fixed interval, fanning the primary's
// save out to its peers and flagging conflicts (see internal/daemon and
// docs/state-machine.md). It shuts down cleanly on SIGINT/SIGTERM.
//
// main stays thin: config parsing lives in parseConfig and all loop logic in
// internal/daemon, both unit-tested without a process or a database.
//
// TODO(slice-api): start the HTTP server (web UI + REST) alongside the daemon.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/a-mcf/retrosync/internal/daemon"
	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/migrate"
	"github.com/a-mcf/retrosync/internal/reach/resolve"
	"github.com/a-mcf/retrosync/internal/store/postgres"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// errStartupDatabase is the single error returned for ANY database startup
// failure (parse/connect/acquire/migrate). It carries no detail so the DSN
// (which can embed a password) can never reach a log or the process exit path;
// the specific cause is logged separately with non-secret fields only.
var errStartupDatabase = errors.New("database startup failed")

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("retrosync: fatal", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := parseConfig(os.Getenv)
	if err != nil {
		return err
	}

	// ctx is cancelled on SIGINT/SIGTERM for graceful shutdown; the daemon's Run
	// loop returns promptly when it fires.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A pool backs the Store; migrations run on one acquired connection. Use a
	// bounded connect/migrate timeout that does NOT inherit the long-lived run
	// ctx, so connect can't be wedged forever, but cancel it if we're shutting
	// down mid-startup.
	startCtx, cancelStart := context.WithTimeout(ctx, 30*time.Second)
	// cancelStart is invoked explicitly after migrations and again via defer;
	// context.CancelFunc is idempotent, so the intentional double call is fine.
	defer cancelStart()

	// Parse the DSN before connecting so we can log a non-secret host on connect
	// failures. The raw err from ParseConfig can embed the DSN (password), so on
	// parse failure we log a FIXED message with NO err — never err.Error().
	pgCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		logger.Error("database config invalid (check DATABASE_URL)")
		return errStartupDatabase
	}

	pool, err := pgxpool.NewWithConfig(startCtx, pgCfg)
	if err != nil {
		// pgxpool errors can embed the DSN; log only the non-secret host.
		logger.Error("database connect failed", slog.String("host", pgCfg.ConnConfig.Host))
		return errStartupDatabase
	}
	defer pool.Close()

	conn, err := pool.Acquire(startCtx)
	if err != nil {
		logger.Error("database acquire failed", slog.String("host", pgCfg.ConnConfig.Host))
		return errStartupDatabase
	}
	if err := migrate.Up(startCtx, conn.Conn()); err != nil {
		conn.Release()
		// Migration errors do not carry the DSN, but keep the same fixed-message
		// treatment for a uniform, leak-proof startup-error path.
		logger.Error("database migrate failed", slog.String("err", err.Error()))
		return errStartupDatabase
	}
	conn.Release()
	cancelStart()

	st := postgres.New(pool)
	eng := engine.New(st, resolve.ResolveReach, time.Now)
	d := daemon.New(st, eng, cfg.PollInterval, cfg.PollTimeout, logger)

	logger.Info("retrosync starting",
		slog.String("version", version),
		slog.Duration("poll_interval", cfg.PollInterval),
		slog.Duration("poll_timeout", cfg.PollTimeout))

	// Run blocks until ctx is cancelled (signal), returning ctx.Err(). That is a
	// clean shutdown, not a failure.
	if err := d.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}

	logger.Info("retrosync stopped cleanly")
	return nil
}
