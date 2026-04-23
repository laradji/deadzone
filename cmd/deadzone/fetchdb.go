package main

// fetchdb is the `deadzone fetch-db` subcommand introduced in #108.
// It exposes the same db.Bootstrap flow `deadzone server` uses, as an
// explicit cache-warmup / refresh entry point. Useful for:
//
//   - Pre-populating the cache before going offline.
//   - Recovering from local corruption via --force (same tag, fresh
//     bytes, verified sha256).
//   - CI / scripted setups that want a deterministic "fetch now" step
//     instead of relying on the implicit on-startup path.
//
// Revised contract from PR #110 review: the fetched DB is pinned to
// the binary's own version (same as `deadzone server`); fetch-db does
// NOT pull "the newest DB on GitHub" unless the binary itself is a
// dev build.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/logs"
)

var (
	fetchDBForce   bool
	fetchDBRepo    string
	fetchDBVerbose bool
)

var fetchDBCmd = &cobra.Command{
	Use:   "fetch-db",
	Short: "Download/refresh the cached deadzone.db from the latest GH Release",
	Long: `Explicit cache-warmup / refresh entry point for the same db.Bootstrap flow
` + "`deadzone server`" + ` uses implicitly. The fetched DB is pinned to the binary's
own version; pass --force to re-fetch the same tag to recover from local
corruption.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runFetchDB()
	},
}

func init() {
	fetchDBCmd.Flags().BoolVar(&fetchDBForce, "force", false,
		"re-fetch even when the cached DB tag matches the binary's version (use to recover from local corruption)")
	fetchDBCmd.Flags().StringVar(&fetchDBRepo, "repo", "",
		"GitHub owner/name override — primarily for testing against a fork (default: "+db.BootstrapDefaultRepo+")")
	fetchDBCmd.Flags().BoolVar(&fetchDBVerbose, "verbose", false,
		"enable Debug-level slog output")
	rootCmd.AddCommand(fetchDBCmd)
}

func runFetchDB() error {
	slog.SetDefault(logs.New(os.Stderr, fetchDBVerbose))

	// SIGINT-aware context so Ctrl-C during the fetch tears down
	// cleanly instead of letting the HTTP client run to its timeout.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	path, upgraded, err := db.BootstrapWithOptions(ctx, db.BootstrapOptions{
		Repo:       fetchDBRepo,
		AppVersion: version,
		Force:      fetchDBForce,
	})
	if err != nil {
		return fmt.Errorf("fetch-db: %w", err)
	}
	if upgraded || fetchDBForce {
		fmt.Printf("deadzone.db upgraded to latest at %s\n", path)
	} else {
		fmt.Printf("deadzone.db already up to date at %s\n", path)
	}
	return nil
}
