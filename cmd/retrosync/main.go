// Command retrosync is the RetroSync server entrypoint. For this slice it
// connects to Postgres, runs registry migrations, logs a startup line, and
// exits cleanly. The HTTP server and play loop arrive in later slices.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/a-mcf/retrosync/internal/migrate"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatalf("retrosync: %v", err)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	if err := migrate.Up(ctx, conn); err != nil {
		return err
	}

	log.Printf("retrosync %s starting", version)

	// TODO(slice-api): start the HTTP server (web UI + REST) here instead of
	// returning. For now we exit cleanly after migrating.
	return nil
}
