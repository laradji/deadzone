package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
	"github.com/laradji/deadzone/internal/logs"
	"github.com/laradji/deadzone/internal/scraper"
)

func main() {
	if err := run(); err != nil {
		slog.Error("scraper fatal", "err", err.Error())
		os.Exit(1)
	}
}

func run() error {
	dbPath := flag.String("db", "deadzone.db", "path to turso database file")
	embedderKind := flag.String("embedder", embed.KindHugot, "embedder to use (valid: hugot)")
	verbose := flag.Bool("verbose", false, "emit per-doc Debug log lines in addition to per-URL summaries")
	configPath := flag.String("config", "libraries_sources.yaml", "path to libraries_sources.yaml registry")
	libFilter := flag.String("lib", "", "scrape only this lib_id (matches base or /base/version); empty = scrape all")
	flag.Parse()

	// stderr-only JSON logging keeps the scraper consistent with
	// cmd/server (which has a hard stdout-is-JSON-RPC constraint).
	slog.SetDefault(logs.New(os.Stderr, *verbose))

	cfg, err := scraper.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Resolve flattens version shorthands and applies the -lib filter so
	// the scrape loop doesn't need to know about either feature.
	sources := cfg.Resolve(*libFilter)
	if len(sources) == 0 {
		if *libFilter != "" {
			return fmt.Errorf("no libraries match -lib %q in %s", *libFilter, *configPath)
		}
		return fmt.Errorf("no libraries to scrape in %s", *configPath)
	}

	e, err := embed.New(*embedderKind)
	if err != nil {
		return fmt.Errorf("embedder: %w", err)
	}
	defer func() {
		if err := e.Close(); err != nil {
			slog.Warn("embedder close", "err", err.Error())
		}
	}()

	// db.Open enforces meta consistency: if the database already exists
	// and was indexed with a different embedder, it returns
	// db.ErrEmbedderMismatch and we abort without touching the data.
	d, err := db.Open(*dbPath, db.Meta{
		EmbedderKind: e.Kind(),
		EmbeddingDim: e.Dim(),
		ModelVersion: e.ModelVersion(),
	})
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	slog.Info("scraper.start",
		"config_path", *configPath,
		"lib_filter", *libFilter,
		"lib_count", len(sources),
		"db_path", *dbPath,
		"embedder_kind", e.Kind(),
		"embedding_dim", e.Dim(),
		"model_version", e.ModelVersion(),
	)

	ctx := context.Background()
	runStart := time.Now()
	var docsTotal int

	for _, src := range sources {
		libDocs, err := scrapeLib(ctx, http.DefaultClient, e, d, src)
		if err != nil {
			return err
		}
		docsTotal += libDocs
	}

	slog.Info("scraper.done",
		"lib_count", len(sources),
		"docs_total", docsTotal,
		"duration_ms", time.Since(runStart).Milliseconds(),
		"db_path", *dbPath,
	)
	return nil
}

// scrapeLib runs the per-URL fetch / embed / insert loop for one resolved
// library and returns the number of docs successfully indexed. It is
// extracted from run() so the multi-library loop stays readable while the
// per-URL bookkeeping (timings, fetch_failed events, embed/insert
// summaries) keeps its single-lib structure.
func scrapeLib(
	ctx context.Context,
	client *http.Client,
	e embed.Embedder,
	d *db.DB,
	src scraper.ResolvedSource,
) (int, error) {
	slog.Info("scraper.lib_start",
		"lib_id", src.LibID,
		"base_lib_id", src.BaseLibID,
		"version", src.Version,
		"kind", src.Kind,
		"url_count", len(src.URLs),
	)

	libStart := time.Now()
	var docsTotal int

	for _, u := range src.URLs {
		// Per-URL fetch — split out from the embed/insert loop so the
		// "silently stalls on one URL" failure mode shows up as a
		// missing scraper.fetch event for that URL.
		fetchStart := time.Now()
		res, err := scraper.FetchOne(ctx, client, src.LibID, u)
		fetchDur := time.Since(fetchStart)
		if err != nil {
			slog.Error("scraper.fetch_failed",
				"lib_id", src.LibID,
				"url", u,
				"duration_ms", fetchDur.Milliseconds(),
				"err", err.Error(),
			)
			return docsTotal, fmt.Errorf("fetch %s: %w", u, err)
		}
		slog.Info("scraper.fetch",
			"lib_id", src.LibID,
			"url", u,
			"bytes", res.Bytes,
			"duration_ms", fetchDur.Milliseconds(),
			"docs_extracted", len(res.Docs),
		)

		// Embed and insert each doc, summing per-step latencies so the
		// scraper.indexed line carries the timing breakdown for the
		// URL without one log line per doc (gated on -verbose).
		//
		// Embed failures are logged and the doc is skipped rather than
		// aborting the whole run: a single bad doc shouldn't take down
		// a multi-URL scrape, but the operator needs to see the count
		// in the per-URL summary so silent doc loss is impossible.
		var embedTotal, insertTotal time.Duration
		var docsInserted, docsSkipped int
		for _, doc := range res.Docs {
			embedStart := time.Now()
			vec, err := e.Embed(doc.Title + "\n" + doc.Content)
			embedTotal += time.Since(embedStart)
			if err != nil {
				docsSkipped++
				slog.Warn("scraper.embed_failed",
					"lib_id", doc.LibID,
					"title", doc.Title,
					"url", u,
					"err", err.Error(),
				)
				continue
			}

			insertStart := time.Now()
			if err := db.Insert(d, doc, vec); err != nil {
				slog.Error("scraper.insert_failed",
					"lib_id", doc.LibID,
					"title", doc.Title,
					"url", u,
					"err", err.Error(),
				)
				return docsTotal, fmt.Errorf("insert %q: %w", doc.Title, err)
			}
			insertTotal += time.Since(insertStart)
			docsInserted++

			slog.Debug("scraper.doc_indexed",
				"lib_id", doc.LibID,
				"title", doc.Title,
				"url", u,
				"content_bytes", len(doc.Content),
			)
		}

		slog.Info("scraper.indexed",
			"lib_id", src.LibID,
			"url", u,
			"docs_inserted", docsInserted,
			"docs_skipped", docsSkipped,
			"embed_ms_total", embedTotal.Milliseconds(),
			"insert_ms_total", insertTotal.Milliseconds(),
		)

		docsTotal += docsInserted
	}

	slog.Info("scraper.lib_done",
		"lib_id", src.LibID,
		"docs_total", docsTotal,
		"duration_ms", time.Since(libStart).Milliseconds(),
	)
	return docsTotal, nil
}
