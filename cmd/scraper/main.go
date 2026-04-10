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

const goSDKLibID = "/modelcontextprotocol/go-sdk"

const rawBase = "https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/main/"

var goSDKURLs = []string{
	rawBase + "README.md",
	rawBase + "docs/README.md",
	rawBase + "docs/quick_start.md",
	rawBase + "docs/server.md",
	rawBase + "docs/client.md",
	rawBase + "docs/protocol.md",
	rawBase + "docs/mcpgodebug.md",
	rawBase + "docs/troubleshooting.md",
	rawBase + "docs/rough_edges.md",
}

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
	flag.Parse()

	// stderr-only JSON logging keeps the scraper consistent with
	// cmd/server (which has a hard stdout-is-JSON-RPC constraint).
	slog.SetDefault(logs.New(os.Stderr, *verbose))

	e, err := embed.New(*embedderKind)
	if err != nil {
		return fmt.Errorf("embedder: %w", err)
	}
	if c, ok := e.(interface{ Close() error }); ok {
		defer func() {
			if err := c.Close(); err != nil {
				slog.Warn("embedder close", "err", err.Error())
			}
		}()
	}

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

	src := scraper.Source{
		LibID: goSDKLibID,
		URLs:  goSDKURLs,
	}

	slog.Info("scraper.start",
		"lib_id", src.LibID,
		"url_count", len(src.URLs),
		"db_path", *dbPath,
		"embedder_kind", e.Kind(),
		"embedding_dim", e.Dim(),
		"model_version", e.ModelVersion(),
	)

	ctx := context.Background()
	runStart := time.Now()
	var docsTotal int

	for _, u := range src.URLs {
		// Per-URL fetch — split out from the embed/insert loop so the
		// "silently stalls on one URL" failure mode shows up as a
		// missing scraper.fetch event for that URL.
		fetchStart := time.Now()
		res, err := scraper.FetchOne(ctx, http.DefaultClient, src.LibID, u)
		fetchDur := time.Since(fetchStart)
		if err != nil {
			slog.Error("scraper.fetch_failed",
				"lib_id", src.LibID,
				"url", u,
				"duration_ms", fetchDur.Milliseconds(),
				"err", err.Error(),
			)
			return fmt.Errorf("fetch %s: %w", u, err)
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
		var embedTotal, insertTotal time.Duration
		for _, doc := range res.Docs {
			embedStart := time.Now()
			vec := e.Embed(doc.Title + "\n" + doc.Content)
			embedTotal += time.Since(embedStart)

			insertStart := time.Now()
			if err := db.Insert(d, doc, vec); err != nil {
				slog.Error("scraper.insert_failed",
					"lib_id", doc.LibID,
					"title", doc.Title,
					"url", u,
					"err", err.Error(),
				)
				return fmt.Errorf("insert %q: %w", doc.Title, err)
			}
			insertTotal += time.Since(insertStart)

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
			"docs_inserted", len(res.Docs),
			"embed_ms_total", embedTotal.Milliseconds(),
			"insert_ms_total", insertTotal.Milliseconds(),
		)

		docsTotal += len(res.Docs)
	}

	slog.Info("scraper.done",
		"lib_id", src.LibID,
		"docs_total", docsTotal,
		"duration_ms", time.Since(runStart).Milliseconds(),
		"db_path", *dbPath,
	)
	return nil
}
