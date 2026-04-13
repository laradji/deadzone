package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/scraper"
)

// stubEmbedder is a deterministic, dependency-free Embedder for testing
// the cmd/scraper orchestration. Every call returns a fixed zero-vector
// of the configured dim so db.Insert and db.UpsertLibIfNew see
// embeddings of the right shape without loading any model files.
type stubEmbedder struct {
	dim   int
	calls atomic.Int64
}

func (s *stubEmbedder) EmbedQuery(string) ([]float32, error)    { return s.vec(), nil }
func (s *stubEmbedder) EmbedDocument(string) ([]float32, error) { return s.vec(), nil }
func (s *stubEmbedder) Kind() string                            { return "stub" }
func (s *stubEmbedder) Dim() int                                { return s.dim }
func (s *stubEmbedder) ModelVersion() string                    { return "stub-v0" }
func (s *stubEmbedder) Close() error                            { return nil }
func (s *stubEmbedder) vec() []float32 {
	s.calls.Add(1)
	// Non-zero so any future normalized-vector assertion still sees a
	// well-formed input; the specific values don't matter for the
	// orchestration tests.
	v := make([]float32, s.dim)
	v[0] = 1
	return v
}

func newStubMeta() (*stubEmbedder, db.Meta) {
	e := &stubEmbedder{dim: 8}
	return e, db.Meta{EmbedderKind: e.Kind(), EmbeddingDim: e.Dim(), ModelVersion: e.ModelVersion()}
}

// TestScrapeSources_ContinueOnError exercises the #93 continue-on-error
// contract: one lib fails (the first URL returns 500 five times, tripping
// skipped_ceiling), the sibling lib still completes, and the per-lib
// result vector carries both outcomes. The caller in run() turns this
// into a non-zero exit code and libs_ok=1 / libs_failed=1.
func TestScrapeSources_ContinueOnError(t *testing.T) {
	t.Parallel()

	// Failing server: every request returns a 5xx, so classifyFetchErr
	// tags each as soft-skip and the lib trips the skipped_ceiling.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	// Succeeding server: a minimal markdown page that ParseMarkdown
	// accepts into one doc. Two H2 sections → two docs.
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		fmt.Fprint(w, "# Hello\n\n## Intro\n\nBody text here.\n\n## Next\n\nMore body.\n")
	}))
	defer okSrv.Close()

	artifacts := t.TempDir()
	e, meta := newStubMeta()

	failURLs := make([]string, maxSkipsPerLib)
	for i := range failURLs {
		failURLs[i] = failSrv.URL + fmt.Sprintf("/boom-%d.md", i)
	}

	sources := []scraper.ResolvedSource{
		{LibID: "/test/fails", BaseLibID: "/test/fails", Kind: scraper.KindGithubMD, URLs: failURLs},
		{LibID: "/test/works", BaseLibID: "/test/works", Kind: scraper.KindGithubMD, URLs: []string{okSrv.URL + "/hello.md"}},
	}

	results := scrapeSources(context.Background(), http.DefaultClient, nil, e, meta, artifacts, sources,
		map[string]int{scraper.KindGithubMD: 2})

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Results are indexed by source position, so we can assert directly.
	if results[0].err == nil {
		t.Errorf("expected first (failing) lib to carry an error, got nil")
	}
	if results[1].err != nil {
		t.Errorf("expected second (succeeding) lib to succeed, got err: %v", results[1].err)
	}
	if results[1].docs == 0 {
		t.Errorf("expected second lib to index >=1 doc, got 0")
	}

	// The successful lib's artifact must be on disk; the failing lib's
	// must not (the "delete then open" rebuild wipes the file before
	// open, and the failure aborts before a successful close).
	okArtifact := filepath.Join(artifacts, "test_works.db")
	if _, err := os.Stat(okArtifact); err != nil {
		t.Errorf("expected successful artifact at %s: %v", okArtifact, err)
	}
}

// TestScrapeSources_ParallelGithubMD proves the lib-level loop runs
// kind-level-parallel: four github-md libs each blocked on a 500ms
// fixture server should finish well under 2s (serial would be ~2s,
// 4-wide parallel is ~0.5s + overhead). The threshold is loose enough
// to avoid CI flake but tight enough to fail if the loop serialized.
func TestScrapeSources_ParallelGithubMD(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping parallelism timing test in -short mode")
	}
	t.Parallel()

	const delay = 500 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "text/markdown")
		fmt.Fprint(w, "# Hi\n\n## Section\n\nContent.\n")
	}))
	defer srv.Close()

	artifacts := t.TempDir()
	e, meta := newStubMeta()

	// Four libs, each with one URL — one fetch per lib, all delayed 500ms.
	sources := make([]scraper.ResolvedSource, 4)
	for i := range sources {
		id := fmt.Sprintf("/test/lib%d", i)
		sources[i] = scraper.ResolvedSource{
			LibID:     id,
			BaseLibID: id,
			Kind:      scraper.KindGithubMD,
			URLs:      []string{srv.URL + fmt.Sprintf("/%d.md", i)},
		}
	}

	start := time.Now()
	results := scrapeSources(context.Background(), http.DefaultClient, nil, e, meta, artifacts, sources,
		map[string]int{scraper.KindGithubMD: 4})
	elapsed := time.Since(start)

	for i, r := range results {
		if r.err != nil {
			t.Errorf("lib %d failed unexpectedly: %v", i, r.err)
		}
	}

	// Serial would be ~4 * 500ms = 2s. 1.5s allows for scheduler and I/O
	// jitter while still catching a regression to sequential execution.
	if elapsed >= 1500*time.Millisecond {
		t.Errorf("expected parallel execution to finish under 1.5s, took %v", elapsed)
	}
}

// TestScrapeSources_UnknownKind ensures an unexpected kind produces a
// per-lib error rather than panicking or silently dropping the lib.
// LoadConfig already rejects unknown kinds at parse time; this is the
// belt-and-braces guard on the direct-call path that consolidate/tests
// might otherwise exercise.
func TestScrapeSources_UnknownKind(t *testing.T) {
	t.Parallel()

	artifacts := t.TempDir()
	e, meta := newStubMeta()

	sources := []scraper.ResolvedSource{
		{LibID: "/test/weird", BaseLibID: "/test/weird", Kind: "invented", URLs: []string{"http://nope.invalid/"}},
	}

	results := scrapeSources(context.Background(), http.DefaultClient, nil, e, meta, artifacts, sources,
		map[string]int{scraper.KindGithubMD: 1})

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].err == nil {
		t.Errorf("expected err for unknown kind, got nil")
	}
	if !strings.Contains(results[0].err.Error(), "unknown kind") {
		t.Errorf("expected 'unknown kind' in err, got %v", results[0].err)
	}
}

// TestEnvIntOr covers the three branches the flag defaults depend on:
// unset, bad, and good values. Silent fallback on a bad value is by
// design (see envIntOr's comment).
func TestEnvIntOr(t *testing.T) {
	const name = "DEADZONE_SCRAPE_TEST_INT"
	t.Setenv(name, "")
	if got := envIntOr(name, 7); got != 7 {
		t.Errorf("unset: got %d, want 7", got)
	}
	t.Setenv(name, "not-a-number")
	if got := envIntOr(name, 7); got != 7 {
		t.Errorf("garbage: got %d, want 7 (silent fallback)", got)
	}
	t.Setenv(name, "0")
	if got := envIntOr(name, 7); got != 7 {
		t.Errorf("zero: got %d, want 7 (n<1 treated as invalid)", got)
	}
	t.Setenv(name, "12")
	if got := envIntOr(name, 7); got != 12 {
		t.Errorf("good: got %d, want 12", got)
	}
}
