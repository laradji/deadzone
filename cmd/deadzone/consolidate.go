package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
	"github.com/laradji/deadzone/internal/logs"
)

// runConsolidate is the `deadzone consolidate` entry point. It merges
// every per-lib artifact .db file in -artifacts into the main -db,
// replacing any rows that share a lib_id. Run after scraping (or after
// pulling fresh artifacts via `deadzone packs download`) to refresh
// the database the MCP server reads.
//
// Intentionally explicit: the server does not auto-consolidate at
// startup, and there is no "are you sure?" prompt here. Re-running is
// idempotent and safe — it deletes per-lib slices in main and re-
// inserts from each artifact within a single transaction, so a partial
// failure leaves main byte-identical to its pre-call state.
func runConsolidate(args []string) error {
	fs := flag.NewFlagSet("consolidate", flag.ExitOnError)
	dbPath := fs.String("db", "deadzone.db", "path to main deadzone database (created if missing)")
	artifactsDir := fs.String("artifacts", "./artifacts", "directory containing per-lib artifact .db files")
	embedderKind := fs.String("embedder", embed.KindHugot, "embedder to use (valid: hugot)")
	verbose := fs.Bool("verbose", false, "verbose logging")
	if err := fs.Parse(args); err != nil {
		return err
	}

	slog.SetDefault(logs.New(os.Stderr, *verbose))

	// The embedder is loaded purely so we can hand its meta to
	// db.Open and to the validation pass — consolidate doesn't itself
	// embed any text. The model is downloaded on first use exactly
	// like the scraper, so a fresh checkout that runs `consolidate`
	// before `scrape` still works (it just downloads ~90MB of model
	// weights to no immediate purpose).
	e, err := embed.New(*embedderKind)
	if err != nil {
		return fmt.Errorf("embedder: %w", err)
	}
	defer func() {
		if err := e.Close(); err != nil {
			slog.Warn("embedder close", "err", err.Error())
		}
	}()

	meta := db.Meta{
		EmbedderKind: e.Kind(),
		EmbeddingDim: e.Dim(),
		ModelVersion: e.ModelVersion(),
	}

	d, err := db.Open(*dbPath, meta)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	slog.Info("consolidate.start",
		"db_path", *dbPath,
		"artifacts_dir", *artifactsDir,
		"embedder_kind", e.Kind(),
		"embedding_dim", e.Dim(),
		"model_version", e.ModelVersion(),
	)

	start := time.Now()
	result, err := db.Consolidate(d, *artifactsDir)
	if err != nil {
		return fmt.Errorf("consolidate: %w", err)
	}

	slog.Info("consolidate.done",
		"db_path", *dbPath,
		"artifacts_dir", *artifactsDir,
		"artifacts", result.Artifacts,
		"docs_merged", result.DocsMerged,
		"libs_merged", result.LibsMerged,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}
