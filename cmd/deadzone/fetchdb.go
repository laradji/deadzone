package main

// fetchdb is the `deadzone fetch-db` subcommand introduced in #108.
// It exposes the same db.Bootstrap flow `deadzone server` uses, as an
// explicit cache-warmup / refresh entry point. Useful for:
//
//   - Pre-populating the cache before going offline.
//   - Force-refreshing without restarting the server (combined with
//     a separate restart afterwards — the running server holds the DB
//     file open, so picking up a new release requires a restart).
//   - CI / scripted setups that want a deterministic "fetch now" step
//     instead of relying on the implicit on-startup path.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/logs"
)

func runFetchDB(args []string) error {
	fs := flag.NewFlagSet("fetch-db", flag.ExitOnError)
	force := fs.Bool("force", false, "re-fetch even when the cached DB tag matches the latest release")
	repo := fs.String("repo", "", "GitHub owner/name (default: "+db.BootstrapDefaultRepo+")")
	verbose := fs.Bool("verbose", false, "enable Debug-level slog output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	slog.SetDefault(logs.New(os.Stderr, *verbose))

	path, upgraded, err := db.BootstrapWithOptions(context.Background(), db.BootstrapOptions{
		Repo:  *repo,
		Force: *force,
	})
	if err != nil {
		return fmt.Errorf("fetch-db: %w", err)
	}
	if upgraded || *force {
		fmt.Printf("deadzone.db upgraded to latest at %s\n", path)
	} else {
		fmt.Printf("deadzone.db already up to date at %s\n", path)
	}
	return nil
}
