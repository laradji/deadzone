// Parallelism model for `deadzone scrape` (see #93). The outer loop
// over resolved libraries runs concurrently, bounded per kind by two
// independent semaphores:
//
//   - github-md and github-rst libs are pure HTTP and safe to run
//     N-wide in parallel. Both kinds share the --parallel-github-md
//     bound (default 4; env DEADZONE_SCRAPE_PARALLEL_GITHUB_MD) since
//     they have identical I/O characteristics — see #95 for the choice
//     to reuse the existing flag rather than introduce a per-kind one.
//   - scrape-via-agent libs share one LLM endpoint that is usually
//     single-threaded on consumer hardware (oMLX, Ollama). Default
//     concurrency is 1 to preserve today's behavior; raise it with
//     --parallel-scrape-via-agent or DEADZONE_SCRAPE_PARALLEL_SCRAPE_VIA_AGENT
//     when pointed at a concurrent backend (vLLM, OpenAI).
//
// Per-URL semantics (soft-skip + skipped_ceiling, see #63) are unchanged;
// only the lib-level orchestration is parallel. Errors are aggregated
// continue-on-error style: one lib's failure no longer aborts the rest,
// and the process exits 1 iff at least one lib failed.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
	"github.com/laradji/deadzone/internal/logs"
	"github.com/laradji/deadzone/internal/packs"
	"github.com/laradji/deadzone/internal/scraper"
)

// maxSkipsPerLib caps the number of per-URL failures tolerated inside a
// single lib before the scraper aborts with "too many failed URLs".
// Sized so a dead LLM endpoint or a fully unreachable documentation
// host can't grind through a thousand-URL library producing zero docs,
// while still absorbing the transient blips (one cold-start timeout,
// one 5xx) that #63's smoke test showed were killing the lib on the
// very first real run. Intentionally not configurable — tightening or
// loosening this is a product decision, not an operational knob.
const maxSkipsPerLib = 5

// Env-var knobs for per-kind parallelism, read as the default value of
// the matching --parallel-* flag. Explicit flags always win (see
// runScrape for the wiring). Naming mirrors DEADZONE_AGENT_ENDPOINT* so
// an operator configuring both ends of the pipeline sees one prefix.
const (
	EnvParallelGithubMD       = "DEADZONE_SCRAPE_PARALLEL_GITHUB_MD"
	EnvParallelScrapeViaAgent = "DEADZONE_SCRAPE_PARALLEL_SCRAPE_VIA_AGENT"
)

// Default concurrency per kind. github-md is HTTP-bound; 4 concurrent
// fetches stays polite to any one doc host while clearing a 13-lib
// backlog fast. scrape-via-agent defaults to 1 because local LLM
// runtimes (oMLX, Ollama single-model) serialize requests at the
// backend regardless of how many we fan out from here — raising this
// only helps against a concurrent backend (vLLM, OpenAI).
const (
	defaultParallelGithubMD       = 4
	defaultParallelScrapeViaAgent = 1
)

// scrape subcommand flags — package-level so cobra's Flags().XxxVar can
// bind them at init-time. The --parallel-* defaults are resolved from
// env vars at init() time, which means the env must be set before the
// binary starts; there is no per-invocation re-read. Matches the pre-
// cobra stdlib-flag behavior.
var (
	scrapeArtifactsDir           string
	scrapeEmbedderKind           string
	scrapeVerbose                bool
	scrapeConfigPath             string
	scrapeLibFilter              string
	scrapeVersionFilter          string
	scrapeParallelGithubMD       int
	scrapeParallelScrapeViaAgent int
	scrapeListOnly               bool
)

var scrapeCmd = &cobra.Command{
	Use:   "scrape",
	Short: "Index libraries from libraries_sources.yaml into ./artifacts",
	Long: `Turn libraries_sources.yaml into one artifact DB per resolved
library in ./artifacts/. Each artifact is a per-lib .db plus a state.yaml
sidecar; ` + "`deadzone consolidate`" + ` merges them into the main deadzone.db.

Parallelism is bounded per source kind — see --parallel-github-md and
--parallel-scrape-via-agent. --list emits the resolved (lib_id, version,
slug) matrix as JSON and exits, skipping the embedder entirely.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runScrape()
	},
}

func init() {
	scrapeCmd.Flags().StringVar(&scrapeArtifactsDir, "artifacts", "./artifacts",
		"directory to write per-lib artifact .db files into")
	scrapeCmd.Flags().StringVar(&scrapeEmbedderKind, "embedder", embed.KindHugot,
		"embedder to use (valid: hugot)")
	scrapeCmd.Flags().BoolVar(&scrapeVerbose, "verbose", false,
		"emit per-doc Debug log lines in addition to per-URL summaries")
	scrapeCmd.Flags().StringVar(&scrapeConfigPath, "config", "libraries_sources.yaml",
		"path to libraries_sources.yaml registry")
	scrapeCmd.Flags().StringVar(&scrapeLibFilter, "lib", "",
		"scrape only this base lib_id (e.g. /hashicorp/terraform); empty = scrape all libs in the registry")
	scrapeCmd.Flags().StringVar(&scrapeVersionFilter, "version", "",
		"scrape only this version (requires --lib); empty = all versions of the filtered lib(s)")
	scrapeCmd.Flags().IntVar(&scrapeParallelGithubMD, "parallel-github-md",
		envIntOr(EnvParallelGithubMD, defaultParallelGithubMD),
		"max concurrent github-* libs (github-md, github-rst — env: "+EnvParallelGithubMD+"; flag wins over env)")
	scrapeCmd.Flags().IntVar(&scrapeParallelScrapeViaAgent, "parallel-scrape-via-agent",
		envIntOr(EnvParallelScrapeViaAgent, defaultParallelScrapeViaAgent),
		"max concurrent scrape-via-agent libs (env: "+EnvParallelScrapeViaAgent+"; flag wins over env)")
	// --list short-circuits before embedder/agent setup and emits the
	// resolved (lib_id, version, slug) matrix to stdout as JSON. Consumed
	// by .github/workflows/scrape-pack.yml's expand-libs job (see #126);
	// intentionally the only side-effect-free flag on this subcommand so
	// a CI runner can list libs without a model cache or network.
	scrapeCmd.Flags().BoolVar(&scrapeListOnly, "list", false,
		"emit JSON array of {lib_id, version, slug} resolved from --config and exit; skips the embedder and all I/O")
	rootCmd.AddCommand(scrapeCmd)
}

// runScrape is the `deadzone scrape` entry point. The per-lib indexer
// turns libraries_sources.yaml into one artifact DB per resolved
// library in ./artifacts/.
func runScrape() error {
	if scrapeParallelGithubMD < 1 {
		return fmt.Errorf("--parallel-github-md must be >= 1, got %d", scrapeParallelGithubMD)
	}
	if scrapeParallelScrapeViaAgent < 1 {
		return fmt.Errorf("--parallel-scrape-via-agent must be >= 1, got %d", scrapeParallelScrapeViaAgent)
	}
	// --version without --lib is ambiguous — multi-version libs share
	// tags (two libs can both have a v1.0) so the filter has to be
	// anchored on a base lib_id before the version tag narrows it
	// down. Reject at parse time so the operator sees the mistake
	// before any I/O.
	if scrapeVersionFilter != "" && scrapeLibFilter == "" {
		return errors.New("--version requires --lib")
	}

	// stderr-only JSON logging keeps scrape consistent with the server
	// subcommand (which has a hard stdout-is-JSON-RPC constraint).
	slog.SetDefault(logs.New(os.Stderr, scrapeVerbose))

	cfg, err := scraper.LoadConfig(scrapeConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Resolve flattens version shorthands and applies the
	// (--lib, --version) filter pair so the scrape loop doesn't need to
	// know about either feature.
	sources := cfg.Resolve(scrapeLibFilter, scrapeVersionFilter)
	if len(sources) == 0 {
		switch {
		case scrapeLibFilter != "" && scrapeVersionFilter != "":
			return fmt.Errorf("no libraries match --lib %q --version %q in %s", scrapeLibFilter, scrapeVersionFilter, scrapeConfigPath)
		case scrapeLibFilter != "":
			return fmt.Errorf("no libraries match --lib %q in %s", scrapeLibFilter, scrapeConfigPath)
		default:
			return fmt.Errorf("no libraries to scrape in %s", scrapeConfigPath)
		}
	}

	if scrapeListOnly {
		return emitResolvedList(sources)
	}

	// One artifacts/ dir per scrape run; created on demand so the first
	// invocation on a fresh checkout doesn't require an extra `mkdir -p`
	// step in the README.
	if err := os.MkdirAll(scrapeArtifactsDir, 0o755); err != nil {
		return fmt.Errorf("create artifacts dir %s: %w", scrapeArtifactsDir, err)
	}

	e, err := embed.New(scrapeEmbedderKind)
	if err != nil {
		return fmt.Errorf("embedder: %w", err)
	}
	defer func() {
		if err := e.Close(); err != nil {
			slog.Warn("embedder close", "err", err.Error())
		}
	}()

	// scrape-via-agent sources need an OpenAI-compatible endpoint
	// resolved from env. We construct + ping the agent before any URL
	// is processed so a misconfigured endpoint surfaces as a single
	// startup error rather than a per-URL cascade midway through.
	//
	// Ordered AFTER embed.New so a missing model file or other
	// embedder failure short-circuits before we pay the agent ping
	// latency. Sources without any agent-kind entry skip this entirely
	// so scrape still works on a clean checkout with no env vars set.
	ctx := context.Background()
	agent, err := setupAgent(ctx, sources)
	if err != nil {
		return err
	}

	meta := db.Meta{
		EmbedderKind: e.Kind(),
		EmbeddingDim: e.Dim(),
		ModelVersion: e.ModelVersion(),
	}

	slog.Info("scraper.start",
		"config_path", scrapeConfigPath,
		"lib_filter", scrapeLibFilter,
		"version_filter", scrapeVersionFilter,
		"lib_count", len(sources),
		"artifacts_dir", scrapeArtifactsDir,
		"embedder_kind", e.Kind(),
		"embedding_dim", e.Dim(),
		"model_version", e.ModelVersion(),
		"parallel_github_md", scrapeParallelGithubMD,
		"parallel_scrape_via_agent", scrapeParallelScrapeViaAgent,
	)

	runStart := time.Now()
	parallelByKind := map[string]int{
		scraper.KindGithubMD:       scrapeParallelGithubMD,
		scraper.KindGithubRST:      scrapeParallelGithubMD,
		scraper.KindScrapeViaAgent: scrapeParallelScrapeViaAgent,
	}
	results := scrapeSources(ctx, http.DefaultClient, agent, e, meta, scrapeArtifactsDir, sources, parallelByKind)

	var joined error
	var okCount, failedCount, docsTotal int
	var failedIDs []string
	for _, r := range results {
		slot := r.libID
		if r.version != "" {
			slot = r.libID + "@" + r.version
		}
		if r.err != nil {
			joined = errors.Join(joined, fmt.Errorf("%s: %w", slot, r.err))
			failedCount++
			failedIDs = append(failedIDs, slot)
			slog.Error("scraper.lib_failed",
				"lib_id", r.libID,
				"version", r.version,
				"skipped_count", r.skipped,
				"err", r.err.Error(),
			)
			continue
		}
		okCount++
		docsTotal += r.docs
	}

	slog.Info("scraper.done",
		"libs_ok", okCount,
		"libs_failed", failedCount,
		"libs_failed_ids", failedIDs,
		"docs_total", docsTotal,
		"duration_ms", time.Since(runStart).Milliseconds(),
		"artifacts_dir", scrapeArtifactsDir,
	)

	return joined
}

// libResult is the per-(lib, version) outcome produced by
// scrapeSources. docs is the count of successfully indexed snippets
// (0 on failure); err is the lib-fatal error or nil. skipped carries
// the per-URL soft-fail count so the scraper.lib_failed log line can
// surface whether the slot died on a ceiling trip vs a hard error.
type libResult struct {
	libID   string
	version string
	docs    int
	skipped int
	err     error
}

// scrapeSources drives the lib-level loop with per-kind bounded
// concurrency via errgroup + a semaphore per kind. Each lib's error is
// captured into results[i].err rather than returned from the worker,
// so one lib's failure never cancels its siblings (continue-on-error
// per #93). The caller aggregates counts and decides the process exit
// code.
//
// The semaphore map is keyed by scraper.Kind* discriminators. Unknown
// kinds surface as a per-lib error so the rest of the run still makes
// progress; LoadConfig already rejects unknown kinds at parse time, so
// this branch is belt-and-braces.
func scrapeSources(
	ctx context.Context,
	client *http.Client,
	agent *scraper.Agent,
	e embed.Embedder,
	meta db.Meta,
	artifactsDir string,
	sources []scraper.ResolvedSource,
	parallelByKind map[string]int,
) []libResult {
	sems := make(map[string]chan struct{}, len(parallelByKind))
	for kind, n := range parallelByKind {
		if n < 1 {
			n = 1
		}
		sems[kind] = make(chan struct{}, n)
	}

	results := make([]libResult, len(sources))
	group, gctx := errgroup.WithContext(ctx)
	for i, src := range sources {
		i, src := i, src
		group.Go(func() error {
			sem, ok := sems[src.Kind]
			if !ok {
				results[i] = libResult{libID: src.LibID, version: src.Version, err: fmt.Errorf("unknown kind %q", src.Kind)}
				return nil
			}
			select {
			case sem <- struct{}{}:
			case <-gctx.Done():
				results[i] = libResult{libID: src.LibID, version: src.Version, err: gctx.Err()}
				return nil
			}
			defer func() { <-sem }()

			docs, skipped, err := scrapeLibToArtifact(gctx, client, agent, e, meta, artifactsDir, src)
			results[i] = libResult{libID: src.LibID, version: src.Version, docs: docs, skipped: skipped, err: err}
			// Never propagate — aggregation happens in the caller so
			// sibling libs are not cancelled by errgroup.
			return nil
		})
	}
	_ = group.Wait()
	return results
}

// scrapeLibToArtifact handles one resolved library end-to-end: it
// computes the artifact path from the lib_id, removes any pre-existing
// artifact file (and tursogo's WAL/SHM sidecars) so the new run starts
// from a clean slate, opens a fresh per-lib DB via OpenArtifact, runs
// the per-URL fetch/embed/insert loop, updates the libs catalog row,
// and closes the artifact. Returns the number of docs successfully
// indexed and the per-URL soft-skip count for the operator log.
//
// Each artifact contains exactly one lib_id by construction; the
// "delete then open" rebuild model is intentional — the per-lib
// granularity is the whole point of #28, so a partial scrape that
// leaves an artifact stale would defeat the design. If the rebuild
// fails midway the artifact file is missing on disk, not corrupted,
// and the operator can re-run the same --lib filter to retry.
func scrapeLibToArtifact(
	ctx context.Context,
	client *http.Client,
	agent *scraper.Agent,
	e embed.Embedder,
	meta db.Meta,
	artifactsDir string,
	src scraper.ResolvedSource,
) (docsTotal int, skippedThisLib int, err error) {
	artifactDir := packs.ArtifactDir(artifactsDir, src.LibID, src.Version)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return 0, 0, fmt.Errorf("create artifact dir %s: %w", artifactDir, err)
	}
	artifactPath := packs.ArtifactDBPath(artifactsDir, src.LibID, src.Version)
	statePath := packs.StatePath(artifactsDir, src.LibID, src.Version)

	// Read any pre-existing sidecar BEFORE the wipe loop so
	// `created_at` survives a re-scrape. A missing file is the first-
	// scrape case and is handled below by falling back to time.Now().
	// Any other read/parse error is logged and treated as absent —
	// the scraper is not going to abort a whole run because a sidecar
	// is corrupt; the next successful write will overwrite it.
	var existingState *packs.StateFile
	if s, err := packs.LoadState(statePath); err == nil {
		existingState = s
	} else if !os.IsNotExist(err) {
		slog.Warn("scraper.state_load_failed",
			"lib_id", src.LibID,
			"version", src.Version,
			"state_path", statePath,
			"err", err.Error(),
		)
	}

	// Wipe any prior artifact + tursogo sidecar files. The sidecars
	// are journaling state; an orphaned -wal/-shm pointing at a now-
	// deleted main file confuses the next Open. Errors from os.Remove
	// for non-existent files are ignored — the only thing we care
	// about is that nothing from a previous run survives this point.
	//
	// Note: we intentionally do NOT wipe the `.state` sidecar here —
	// it carries the `created_at` value captured above. On a mid-scrape
	// failure the `.state` is left untouched, still reflecting the last
	// successful scrape (the re-run will overwrite both on success).
	for _, p := range []string{artifactPath, artifactPath + "-wal", artifactPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return 0, 0, fmt.Errorf("remove stale artifact %s: %w", p, err)
		}
	}

	d, err := db.OpenArtifact(artifactPath, meta, src.LibID, src.Version)
	if err != nil {
		return 0, 0, fmt.Errorf("open artifact %s: %w", artifactPath, err)
	}
	defer d.Close()

	slog.Info("scraper.lib_start",
		"lib_id", src.LibID,
		"version", src.Version,
		"kind", src.Kind,
		"ref", src.Ref,
		"url_count", len(src.URLs),
		"artifact_path", artifactPath,
	)

	// Make sure the libs catalog row exists before we start indexing
	// docs. UpsertLibIfNew is idempotent per (lib_id, version) and
	// skips the embed call when the row is already present, so the
	// cost on a re-run is just one count(*); the actual doc_count is
	// filled in at the end of this function once we know the real
	// number. Each ResolvedSource covers exactly one (lib_id, version)
	// slot so a single upsert per source is the correct grain.
	if upsertErr := db.UpsertLibIfNew(d, src.LibID, src.Version, e); upsertErr != nil {
		return 0, 0, fmt.Errorf("upsert lib %q version %q: %w", src.LibID, src.Version, upsertErr)
	}

	// Deferred so libs.doc_count reflects what actually landed in the
	// docs table even if the URL loop aborts mid-way (#65). The named
	// docsTotal return captures the running total at defer time;
	// LIFO ordering puts this before defer d.Close() so the Exec still
	// runs against an open handle. Never clobbers a non-nil err from
	// the loop — the update failure is logged and only promoted to the
	// returned error when the scrape would otherwise have succeeded.
	defer func() {
		updateErr := db.UpdateLibCount(d, src.LibID, src.Version, docsTotal)
		if updateErr == nil {
			return
		}
		slog.Error("scraper.update_lib_count_failed",
			"lib_id", src.LibID,
			"version", src.Version,
			"docs_total", docsTotal,
			"err", updateErr.Error(),
		)
		if err == nil {
			err = fmt.Errorf("update lib count %q version %q: %w", src.LibID, src.Version, updateErr)
		}
	}()

	libStart := time.Now()

	for _, u := range src.URLs {
		// Per-URL fetch — split out from the embed/insert loop so the
		// "silently stalls on one URL" failure mode shows up as a
		// missing scraper.fetch event for that URL. The kind dispatch
		// is intentionally trivial: github-md is the fast HTTP→parse
		// path, scrape-via-agent runs the LLM extractor.
		fetchStart := time.Now()
		var (
			res     scraper.FetchOneResult
			err     error
			fetcher = src.Kind
		)
		switch src.Kind {
		case scraper.KindGithubMD:
			res, err = scraper.FetchOne(ctx, client, src.LibID, u)
		case scraper.KindGithubRST:
			res, err = scraper.FetchOneViaGithubRST(ctx, client, src.LibID, u)
		case scraper.KindScrapeViaAgent:
			res, err = scraper.FetchOneViaAgent(ctx, client, agent, src.LibID, u)
		default:
			return docsTotal, skippedThisLib, fmt.Errorf("unsupported kind %q for lib %q", src.Kind, src.LibID)
		}
		fetchDur := time.Since(fetchStart)
		if err != nil {
			// Classify the failure. Verification drops, transient
			// transport errors (timeouts, 5xx, reset connections),
			// and per-URL content-type rejections (PDF, unknown
			// binary) all soft-skip: log at url_skipped, count
			// against maxSkipsPerLib, keep processing the rest of
			// the lib. Anything else (auth, nil agent, insert
			// failure downstream) is fatal for the lib — the
			// alternative is a half-indexed artifact masquerading
			// as complete.
			reason, soft := classifyFetchErr(err)
			if soft {
				skippedThisLib++
				slog.Error("scraper.url_skipped",
					"lib_id", src.LibID,
					"version", src.Version,
					"url", u,
					"kind", fetcher,
					"reason", reason,
					"skipped_count", skippedThisLib,
					"skipped_ceiling", maxSkipsPerLib,
					"duration_ms", fetchDur.Milliseconds(),
					"err", err.Error(),
				)
				if skippedThisLib >= maxSkipsPerLib {
					return docsTotal, skippedThisLib, fmt.Errorf(
						"too many failed URLs in %s (%d skipped, ceiling %d): %w",
						src.LibID, skippedThisLib, maxSkipsPerLib, err)
				}
				continue
			}
			slog.Error("scraper.fetch_failed",
				"lib_id", src.LibID,
				"version", src.Version,
				"url", u,
				"kind", fetcher,
				"reason", reason,
				"duration_ms", fetchDur.Milliseconds(),
				"err", err.Error(),
			)
			return docsTotal, skippedThisLib, fmt.Errorf("fetch %s: %w", u, err)
		}
		slog.Info("scraper.fetch",
			"lib_id", src.LibID,
			"version", src.Version,
			"url", u,
			"kind", fetcher,
			"bytes", res.Bytes,
			"duration_ms", fetchDur.Milliseconds(),
			"docs_extracted", len(res.Docs),
		)

		// Embed and insert each doc, summing per-step latencies so the
		// scraper.indexed line carries the timing breakdown for the
		// URL without one log line per doc (gated on --verbose).
		//
		// Embed failures are logged and the doc is skipped rather than
		// aborting the whole run: a single bad doc shouldn't take down
		// a multi-URL scrape, but the operator needs to see the count
		// in the per-URL summary so silent doc loss is impossible.
		var embedTotal, insertTotal time.Duration
		var docsInserted, docsSkipped int
		for _, doc := range res.Docs {
			// The fetchers populate doc.LibID from the ResolvedSource's
			// base lib_id but do not know about version — stamp it in
			// here so the DB row carries the (lib_id, version) slot.
			doc.Version = src.Version

			embedStart := time.Now()
			vec, err := e.EmbedDocument(doc.Title + "\n" + doc.Content)
			embedTotal += time.Since(embedStart)
			if err != nil {
				docsSkipped++
				slog.Warn("scraper.embed_failed",
					"lib_id", doc.LibID,
					"version", doc.Version,
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
					"version", doc.Version,
					"title", doc.Title,
					"url", u,
					"err", err.Error(),
				)
				return docsTotal, skippedThisLib, fmt.Errorf("insert %q: %w", doc.Title, err)
			}
			insertTotal += time.Since(insertStart)
			docsInserted++

			slog.Debug("scraper.doc_indexed",
				"lib_id", doc.LibID,
				"version", doc.Version,
				"title", doc.Title,
				"url", u,
				"content_bytes", len(doc.Content),
			)
		}

		slog.Info("scraper.indexed",
			"lib_id", src.LibID,
			"version", src.Version,
			"url", u,
			"docs_inserted", docsInserted,
			"docs_skipped", docsSkipped,
			"embed_ms_total", embedTotal.Milliseconds(),
			"insert_ms_total", insertTotal.Milliseconds(),
		)

		docsTotal += docsInserted
	}

	// Write the `.state` sidecar once the docs table is fully populated;
	// the libs catalog row is updated by the deferred UpdateLibCount
	// installed earlier in this function (see #65), which runs just
	// before `d.Close()` on every exit path. Reaching this point means
	// the URL loop completed without a fatal error, so writing the
	// sidecar here reflects a fully successful scrape — a mid-scrape
	// abort returns above and leaves any pre-existing sidecar intact.
	// `created_at` is preserved from the sidecar we read before the
	// wipe; absent or zero on the first scrape, in which case it is
	// seeded from `now` below.
	now := time.Now().UTC()
	state := &packs.StateFile{
		LibID:         src.LibID,
		Version:       src.Version,
		SchemaVersion: db.CurrentSchemaVersion,
		Embedder: packs.EmbedderState{
			Kind:  e.Kind(),
			Model: e.ModelVersion(),
			Dim:   e.Dim(),
		},
		Ref:       src.Ref,
		CreatedAt: now,
		UpdatedAt: now,
		URLCount:  len(src.URLs),
		DocCount:  docsTotal,
	}
	if existingState != nil && !existingState.CreatedAt.IsZero() {
		state.CreatedAt = existingState.CreatedAt
	}
	if err := state.Save(statePath); err != nil {
		return docsTotal, skippedThisLib, fmt.Errorf("write state %s: %w", statePath, err)
	}

	slog.Info("scraper.lib_done",
		"lib_id", src.LibID,
		"version", src.Version,
		"ref", src.Ref,
		"docs_total", docsTotal,
		"duration_ms", time.Since(libStart).Milliseconds(),
		"artifact_path", artifactPath,
		"state_path", statePath,
	)
	return docsTotal, skippedThisLib, nil
}

// classifyFetchErr tags a fetch/extract error with a short reason and
// reports whether it's soft-failable (per-URL skip) or fatal (abort the
// lib). The tag goes straight into the scraper.url_skipped /
// scraper.fetch_failed log line so an operator can grep for one class
// of failure across a run without parsing wrapped error messages.
//
// Soft-failable errors:
//   - ErrAgentVerificationFailed — hallucinated code block, per-URL drop
//   - ErrPDFNotSupportedYet      — incidental PDF link in a doc index
//   - context.DeadlineExceeded   — cold-start reload or slow first token
//   - HTTP 5xx (via HTTPStatusError) — upstream blip
//   - transient transport errors (ECONNRESET, EPIPE, EOF, net timeouts)
//
// Anything else (auth failures, 4xx other than above, decode errors)
// is fatal for the lib.
func classifyFetchErr(err error) (reason string, soft bool) {
	switch {
	case errors.Is(err, scraper.ErrAgentVerificationFailed):
		return "verification_failed", true
	case errors.Is(err, scraper.ErrPDFNotSupportedYet):
		return "pdf_unsupported", true
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", true
	}
	var httpErr *scraper.HTTPStatusError
	if errors.As(err, &httpErr) {
		if httpErr.Status >= 500 && httpErr.Status < 600 {
			return fmt.Sprintf("http_%d", httpErr.Status), true
		}
		return fmt.Sprintf("http_%d", httpErr.Status), false
	}
	if scraper.IsTransientAgentError(err) {
		return "transport", true
	}
	return "other", false
}

// setupAgent decides whether the scrape-via-agent path is needed for
// this run and, if so, builds + health-checks the Agent before any URL
// is processed.
//
// The contract from #27 is "fail fast at startup, no silent fallback":
//   - if no source uses scrape-via-agent, return (nil, nil) so a clean
//     checkout with no env vars set still works
//   - if any source does, env must be configured AND Ping must succeed
//     before the function returns
//
// The agent value is shared across every scrape-via-agent source for
// the run; the http.Client inside it carries its own per-call timeout.
// Agent.Extract is claimed safe for concurrent use (see
// internal/scraper/agent.go's type doc) — the --parallel-scrape-via-agent
// default of 1 means we do not lean on that claim in practice until the
// operator explicitly raises the knob.
func setupAgent(ctx context.Context, sources []scraper.ResolvedSource) (*scraper.Agent, error) {
	needs := false
	for _, s := range sources {
		if s.Kind == scraper.KindScrapeViaAgent {
			needs = true
			break
		}
	}
	if !needs {
		return nil, nil
	}

	agent, err := scraper.NewAgentFromEnv()
	if err != nil {
		return nil, fmt.Errorf("scrape-via-agent source present but agent not configured: %w", err)
	}

	slog.Info("scraper.agent_configured",
		"endpoint", agent.Endpoint(),
		"model", agent.Model(),
	)

	if err := agent.Ping(ctx); err != nil {
		return nil, fmt.Errorf("agent health check failed for endpoint %s: %w", agent.Endpoint(), err)
	}
	slog.Info("scraper.agent_ping_ok",
		"endpoint", agent.Endpoint(),
		"model", agent.Model(),
	)
	return agent, nil
}

// emitResolvedList writes the resolved (lib_id, version, slug, cache_hash)
// matrix to stdout as a JSON array, one object per ResolvedSource.
// Consumed by .github/workflows/scrape-pack.yml's expand-libs step, which
// pipes the value into a `matrix:` via `fromJSON`. slug matches packs.Slug
// so the cache-key path in each scrape matrix slot is trivially
// reconstructible from the JSON alone. cache_hash is the per-entry hash
// (see entryCacheHash) — it replaces the global hashFiles(libraries_sources.yaml)
// segment of the per-lib artifact cache key so an edit to one entry no
// longer invalidates every other entry's cache (#153).
func emitResolvedList(sources []scraper.ResolvedSource) error {
	type libEntry struct {
		LibID     string `json:"lib_id"`
		Version   string `json:"version"`
		Slug      string `json:"slug"`
		CacheHash string `json:"cache_hash"`
	}
	entries := make([]libEntry, 0, len(sources))
	for _, s := range sources {
		entries = append(entries, libEntry{
			LibID:     s.LibID,
			Version:   s.Version,
			Slug:      packs.Slug(s.LibID, s.Version),
			CacheHash: entryCacheHash(s),
		})
	}
	enc := json.NewEncoder(os.Stdout)
	// Single-line output: GitHub Actions' `$GITHUB_OUTPUT` protocol
	// breaks on embedded newlines unless the multi-line heredoc form is
	// used, and the expand-libs job uses the single-line form.
	return enc.Encode(entries)
}

// entryCacheHash returns a deterministic sha256 hex digest of the inputs
// that should invalidate exactly this resolved entry's per-lib artifact
// cache (#153). Inputs are kind, the post-substitution ref, and the
// post-substitution URLs sorted lexicographically. lib_id and version
// are intentionally NOT in the hash — they already discriminate the
// cache bucket via the slug prefix in the key, so hashing them again
// would be redundant noise that turns a base lib_id rename into a
// double invalidation.
//
// URLs are sorted to make the hash insensitive to YAML reordering that
// doesn't change semantics. The struct is JSON-marshaled before hashing
// to fix the field order across Go versions (struct field order in the
// source determines marshal order, which is stable but not lexicographic).
func entryCacheHash(s scraper.ResolvedSource) string {
	urls := append([]string{}, s.URLs...)
	sort.Strings(urls)
	in := struct {
		Kind string   `json:"kind"`
		Ref  string   `json:"ref"`
		URLs []string `json:"urls"`
	}{
		Kind: s.Kind,
		Ref:  s.Ref,
		URLs: urls,
	}
	// json.Marshal cannot fail for this concrete struct (no unsupported
	// types, no NaN/Inf), so the error is impossible in practice.
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// envIntOr reads an integer from env var name, falling back to def if
// the var is unset, empty, or unparseable. Used to make the
// --parallel-* flag defaults env-overridable without needing a separate
// config file. Silent fallback on parse error is deliberate: the flag
// default is always a safe number, so a typo in the env var shouldn't
// abort the run — the flag help text tells the operator which var they
// set and `scraper.start` logs the effective parallelism.
func envIntOr(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return def
	}
	return n
}
