// Command retrosync is the RetroSync server entrypoint and admin CLI.
//
// Subcommands (dispatched on os.Args[1]):
//
//	serve            run the web UI + play-sync daemon (the default)
//	user set <id>    upsert a web user (bootstrap the first admin)
//
// The `serve` path connects to Postgres, runs registry migrations, then runs
// BOTH the HTTP server (internal/web) and the play-sync daemon (internal/daemon)
// under one signal context, shutting both down cleanly on SIGINT/SIGTERM.
//
// main stays thin: env parsing lives in parseConfig, the user command in
// user.go, and all loop logic in internal/daemon — each unit-tested without a
// process or a database.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/a-mcf/retrosync/internal/daemon"
	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/migrate"
	"github.com/a-mcf/retrosync/internal/reach/resolve"
	"github.com/a-mcf/retrosync/internal/store/postgres"
	"github.com/a-mcf/retrosync/internal/web"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// defaultHTTPAddr is the listen address when RETROSYNC_HTTP_ADDR is unset.
const defaultHTTPAddr = ":8080"

// errStartupDatabase is the single error returned for ANY database startup
// failure (parse/connect/acquire/migrate). It carries no detail so the DSN
// (which can embed a password) can never reach a log or the process exit path;
// the specific cause is logged separately with non-secret fields only.
var errStartupDatabase = errors.New("database startup failed")

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := dispatch(os.Args[1:], logger); err != nil {
		logger.Error("retrosync: fatal", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

// dispatch routes on the first arg. No subcommand (or "serve") runs the
// service; "user set" runs the bootstrap command. Keeping this separate from
// main keeps arg handling testable.
func dispatch(args []string, logger *slog.Logger) error {
	if len(args) == 0 || args[0] == "serve" {
		return run(logger)
	}
	switch args[0] {
	case "user":
		return userCmd(args[1:], logger)
	default:
		return fmt.Errorf("unknown command %q (want: serve | user set)", args[0])
	}
}

// userCmd handles `user <subcommand>`. Only `set` exists this slice.
func userCmd(args []string, logger *slog.Logger) error {
	if len(args) == 0 || args[0] != "set" {
		return fmt.Errorf("usage: retrosync user set <id> [--display NAME] [--role user|admin]")
	}
	a, err := parseUserSet(args[1:])
	if err != nil {
		return err
	}
	password, err := readPassword(os.Getenv, os.Stdin)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cfg, err := parseConfig(os.Getenv)
	if err != nil {
		return err
	}
	pool, err := openPool(startCtx, cfg.DatabaseURL, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := runMigrations(startCtx, pool, logger); err != nil {
		return err
	}

	st := postgres.New(pool)
	if err := runUserSet(startCtx, st, a, password); err != nil {
		return fmt.Errorf("user set: %w", err)
	}
	logger.Info("user upserted", slog.String("id", a.id), slog.String("role", string(a.role)))
	return nil
}

// run is the `serve` path: connect+migrate, then run web + daemon together.
func run(logger *slog.Logger) error {
	cfg, err := parseConfig(os.Getenv)
	if err != nil {
		return err
	}

	// ctx is cancelled on SIGINT/SIGTERM for graceful shutdown; both the daemon
	// loop and the HTTP server return promptly when it fires.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Bound the connect/migrate phase so startup can't wedge forever, but cancel
	// it if we're shutting down mid-startup.
	startCtx, cancelStart := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStart()

	pool, err := openPool(startCtx, cfg.DatabaseURL, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := runMigrations(startCtx, pool, logger); err != nil {
		return err
	}
	cancelStart()

	st := postgres.New(pool)
	eng := engine.New(st, resolve.ResolveReach, time.Now)
	d := daemon.New(st, eng, cfg.PollInterval, cfg.PollTimeout, logger)

	srv, err := web.New(st, web.Options{Logger: logger})
	if err != nil {
		return fmt.Errorf("web server: %w", err)
	}
	httpAddr := os.Getenv("RETROSYNC_HTTP_ADDR")
	if httpAddr == "" {
		httpAddr = defaultHTTPAddr
	}
	httpSrv := &http.Server{
		Addr:              httpAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info("retrosync starting",
		slog.String("version", version),
		slog.String("http_addr", httpAddr),
		slog.Duration("poll_interval", cfg.PollInterval),
		slog.Duration("poll_timeout", cfg.PollTimeout))

	// Run the daemon and HTTP server concurrently under one errgroup. When ctx
	// is cancelled (signal), the daemon returns ctx.Err() and we Shutdown the
	// HTTP server gracefully; if either crashes, the group ctx cancels the other.
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		if err := d.Run(gctx); err != nil && gctx.Err() == nil {
			return err
		}
		return nil
	})

	g.Go(func() error {
		errc := make(chan error, 1)
		go func() {
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
				return
			}
			errc <- nil
		}()
		select {
		case <-gctx.Done():
			// Graceful shutdown with a bounded deadline that does NOT inherit the
			// already-cancelled gctx.
			shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := httpSrv.Shutdown(shutCtx); err != nil {
				logger.Warn("http shutdown error", slog.String("err", err.Error()))
			}
			return nil
		case err := <-errc:
			return err
		}
	})

	if err := g.Wait(); err != nil {
		return err
	}

	logger.Info("retrosync stopped cleanly")
	return nil
}

// openPool parses the DSN, connects, and returns a pool. Every failure is
// collapsed to errStartupDatabase with a non-secret host logged, so the DSN
// (which can embed a password) never reaches the error or the logs.
func openPool(ctx context.Context, dsn string, logger *slog.Logger) (*pgxpool.Pool, error) {
	pgCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		logger.Error("database config invalid (check DATABASE_URL)")
		return nil, errStartupDatabase
	}
	pool, err := pgxpool.NewWithConfig(ctx, pgCfg)
	if err != nil {
		logger.Error("database connect failed", slog.String("host", pgCfg.ConnConfig.Host))
		return nil, errStartupDatabase
	}
	return pool, nil
}

// runMigrations acquires one connection and runs registry migrations. Errors
// are collapsed to errStartupDatabase (with a non-secret host) for a uniform,
// leak-proof startup path.
func runMigrations(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		logger.Error("database acquire failed")
		return errStartupDatabase
	}
	defer conn.Release()
	if err := migrate.Up(ctx, conn.Conn()); err != nil {
		logger.Error("database migrate failed", slog.String("err", err.Error()))
		return errStartupDatabase
	}
	return nil
}
