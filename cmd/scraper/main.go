package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	artifactsDir := flag.String("artifacts", "./artifacts", "directory to write per-lib artifact .db files into")
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

	// One artifacts/ dir per scraper run; created on demand so the
	// first invocation on a fresh checkout doesn't require an extra
	// `mkdir -p` step in the README.
	if err := os.MkdirAll(*artifactsDir, 0o755); err != nil {
		return fmt.Errorf("create artifacts dir %s: %w", *artifactsDir, err)
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

	meta := db.Meta{
		EmbedderKind: e.Kind(),
		EmbeddingDim: e.Dim(),
		ModelVersion: e.ModelVersion(),
	}

	slog.Info("scraper.start",
		"config_path", *configPath,
		"lib_filter", *libFilter,
		"lib_count", len(sources),
		"artifacts_dir", *artifactsDir,
		"embedder_kind", e.Kind(),
		"embedding_dim", e.Dim(),
		"model_version", e.ModelVersion(),
	)

	ctx := context.Background()
	runStart := time.Now()
	var docsTotal int

	for _, src := range sources {
		libDocs, err := scrapeLibToArtifact(ctx, http.DefaultClient, e, meta, *artifactsDir, src)
		if err != nil {
			return err
		}
		docsTotal += libDocs
	}

	slog.Info("scraper.done",
		"lib_count", len(sources),
		"docs_total", docsTotal,
		"duration_ms", time.Since(runStart).Milliseconds(),
		"artifacts_dir", *artifactsDir,
	)
	return nil
}

// scrapeLibToArtifact handles one resolved library end-to-end: it
// computes the artifact path from the lib_id, removes any pre-existing
// artifact file (and tursogo's WAL/SHM sidecars) so the new run starts
// from a clean slate, opens a fresh per-lib DB via OpenArtifact, runs
// the per-URL fetch/embed/insert loop, updates the libs catalog row,
// and closes the artifact. Returns the number of docs successfully
// indexed for the operator log.
//
// Each artifact contains exactly one lib_id by construction; the
// "delete then open" rebuild model is intentional — the per-lib
// granularity is the whole point of #28, so a partial scrape that
// leaves an artifact stale would defeat the design. If the rebuild
// fails midway the artifact file is missing on disk, not corrupted,
// and the operator can re-run the same -lib filter to retry.
func scrapeLibToArtifact(
	ctx context.Context,
	client *http.Client,
	e embed.Embedder,
	meta db.Meta,
	artifactsDir string,
	src scraper.ResolvedSource,
) (int, error) {
	artifactPath := filepath.Join(artifactsDir, artifactFilename(src.LibID))

	// Wipe any prior artifact + tursogo sidecar files. The sidecars
	// are journaling state; an orphaned -wal/-shm pointing at a now-
	// deleted main file confuses the next Open. Errors from os.Remove
	// for non-existent files are ignored — the only thing we care
	// about is that nothing from a previous run survives this point.
	for _, p := range []string{artifactPath, artifactPath + "-wal", artifactPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return 0, fmt.Errorf("remove stale artifact %s: %w", p, err)
		}
	}

	d, err := db.OpenArtifact(artifactPath, meta, src.LibID)
	if err != nil {
		return 0, fmt.Errorf("open artifact %s: %w", artifactPath, err)
	}
	defer d.Close()

	slog.Info("scraper.lib_start",
		"lib_id", src.LibID,
		"base_lib_id", src.BaseLibID,
		"version", src.Version,
		"kind", src.Kind,
		"url_count", len(src.URLs),
		"artifact_path", artifactPath,
	)

	// Make sure the libs catalog row exists before we start indexing
	// docs. UpsertLibIfNew is idempotent and skips the embed call when
	// the row is already present, so the cost on a re-run is just one
	// count(*); the actual doc_count is filled in at the end of this
	// function once we know the real number. Each ResolvedSource has
	// its own canonical lib_id (versioned libs already get distinct
	// /org/project/version values from cfg.Resolve), so a single
	// upsert per source is the correct grain.
	if err := db.UpsertLibIfNew(d, src.LibID, e); err != nil {
		return 0, fmt.Errorf("upsert lib %q: %w", src.LibID, err)
	}

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

	// Update the libs catalog with the final indexed doc count so
	// search_libraries can rank by "how well-indexed is this lib".
	// Each artifact is rebuilt from scratch, so docsTotal is the
	// absolute row count for the lib in this artifact — no append-
	// vs-replace ambiguity.
	if err := db.UpdateLibCount(d, src.LibID, docsTotal); err != nil {
		slog.Error("scraper.update_lib_count_failed",
			"lib_id", src.LibID,
			"docs_total", docsTotal,
			"err", err.Error(),
		)
		return docsTotal, fmt.Errorf("update lib count %q: %w", src.LibID, err)
	}

	slog.Info("scraper.lib_done",
		"lib_id", src.LibID,
		"docs_total", docsTotal,
		"duration_ms", time.Since(libStart).Milliseconds(),
		"artifact_path", artifactPath,
	)
	return docsTotal, nil
}

// artifactFilename derives the on-disk basename for a lib_id's
// artifact: the leading "/" is stripped and the remaining slashes
// become underscores. Example:
//
//	/modelcontextprotocol/go-sdk → modelcontextprotocol_go-sdk.db
//	/facebook/react/v18         → facebook_react_v18.db
//
// The mapping is deterministic and 1:1 with the lib_id, so an operator
// can read the file listing of artifacts/ and recover every lib by
// inspection. Hyphens and dots are preserved.
func artifactFilename(libID string) string {
	trimmed := strings.TrimPrefix(libID, "/")
	return strings.ReplaceAll(trimmed, "/", "_") + ".db"
}
