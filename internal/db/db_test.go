package db_test

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
	_ "turso.tech/database/tursogo"
)

// testEmbedder is the package-level Hugot shared by every test in this
// package. Built once in TestMain so the model download + ORT session
// warm-up cost is amortized over the whole test run.
var testEmbedder *embed.Hugot

func TestMain(m *testing.M) {
	// Multi-process helper short-circuit: when the parent test re-execs
	// us with this env var set, run the DB-holder loop and exit without
	// touching the Hugot embedder or calling m.Run(). See
	// TestOpenReader_MultiProcess for the parent side.
	if path := os.Getenv("DEADZONE_TEST_HOLD_DB_PATH"); path != "" {
		runHoldDBHelper(path)
		return
	}

	e, err := embed.NewHugot(embed.DefaultHugotModel, hugotTestCacheDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: NewHugot: %v\n", err)
		os.Exit(1)
	}
	testEmbedder = e
	code := m.Run()
	_ = e.Close()
	os.Exit(code)
}

// runHoldDBHelper opens path through the same code path OpenReader
// uses (sql.Open + PRAGMA query_only on a pinned connection),
// announces readiness on stdout, and blocks until the parent closes
// stdin. Direct sql.Open is on purpose — calling db.OpenReader would
// require the embedder meta to validate, which would force this child
// process to load Hugot (multi-second startup). The lock acquired by
// PRAGMA query_only is byte-identical to what OpenReader takes, so
// the parent's lock-conflict assertion still pins the same code path.
func runHoldDBHelper(path string) {
	d, err := sql.Open("turso", path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hold helper: sql.Open: %v\n", err)
		os.Exit(1)
	}
	d.SetMaxOpenConns(1)
	d.SetMaxIdleConns(1)
	d.SetConnMaxLifetime(0)
	d.SetConnMaxIdleTime(0)
	if _, err := d.Exec(`PRAGMA query_only = 1`); err != nil {
		fmt.Fprintf(os.Stderr, "hold helper: PRAGMA query_only: %v\n", err)
		os.Exit(1)
	}
	if _, err := fmt.Println("ready"); err != nil {
		os.Exit(1)
	}
	// Block on stdin EOF — parent closes its end of the pipe to signal
	// "you can release the lock and exit". Deliberately avoids signals
	// so a Ctrl-C in the parent test runner naturally tears the child
	// down via SIGPIPE on its next write attempt.
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = d.Close()
	os.Exit(0)
}

// hugotTestCacheDir picks a cache directory for tests. Honors
// DEADZONE_HUGOT_CACHE so CI can pin the cache to a workspace-local path,
// otherwise uses the system default so the model is reused across runs.
func hugotTestCacheDir() string {
	if dir := os.Getenv("DEADZONE_HUGOT_CACHE"); dir != "" {
		return dir
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return ".deadzone-cache/models"
	}
	return cache + "/deadzone/models"
}

// embedText is a small convenience that mirrors what the scraper does in
// real life: embed "Title\nContent" as a corpus document. Tests call it
// with t.Helper-style fatality so an embedder failure aborts the test
// rather than silently passing nil through to db.Insert.
func embedText(t *testing.T, e embed.Embedder, d db.Doc) []float32 {
	t.Helper()
	v, err := e.EmbedDocument(d.Title + "\n" + d.Content)
	if err != nil {
		t.Fatalf("EmbedDocument %q: %v", d.Title, err)
	}
	return v
}

// metaFor extracts a db.Meta from an embed.Embedder. The same little
// adapter is needed in cmd/scraper and cmd/server too; defining it as a
// helper here keeps the tests readable.
func metaFor(e embed.Embedder) db.Meta {
	return db.Meta{
		EmbedderKind: e.Kind(),
		EmbeddingDim: e.Dim(),
		ModelVersion: e.ModelVersion(),
	}
}

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpen_CreatesDocsTable(t *testing.T) {
	d := openTestDB(t)

	// Verify the table exists by inserting through the real Insert path.
	doc := db.Doc{LibID: "testlib", Title: "Hello World", Content: "some content"}
	if err := db.Insert(d, doc, embedText(t, testEmbedder, doc)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

func TestInsert(t *testing.T) {
	d := openTestDB(t)

	docs := []db.Doc{
		{LibID: "go-sdk", Title: "Server setup", Content: "Create a new MCP server with mcp.NewServer"},
		{LibID: "go-sdk", Title: "Tool registration", Content: "Register tools using mcp.AddTool"},
		{LibID: "libsql", Title: "Getting started", Content: "Open a database with sql.Open"},
	}

	for _, doc := range docs {
		if err := db.Insert(d, doc, embedText(t, testEmbedder, doc)); err != nil {
			t.Fatalf("Insert %q: %v", doc.Title, err)
		}
	}

	var count int
	if err := d.QueryRow(`SELECT count(*) FROM docs`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 rows, got %d", count)
	}
}

func TestInsert_RejectsWrongDim(t *testing.T) {
	d := openTestDB(t)

	err := db.Insert(d, db.Doc{LibID: "x", Title: "t", Content: "c"}, []float32{0.1, 0.2})
	if err == nil {
		t.Fatal("expected error for wrong-dimension embedding, got nil")
	}
}

func TestSearchByEmbedding_RanksRelevantFirst(t *testing.T) {
	d := openTestDB(t)

	docs := []db.Doc{
		{LibID: "go-sdk", Title: "Server setup", Content: "Create a new MCP server with mcp.NewServer"},
		{LibID: "go-sdk", Title: "Tool registration", Content: "Register tools using mcp.AddTool"},
		{LibID: "libsql", Title: "Getting started", Content: "Open a database with sql.Open"},
	}
	for _, doc := range docs {
		if err := db.Insert(d, doc, embedText(t, testEmbedder, doc)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	qv, err := testEmbedder.EmbedQuery("create a server")
	if err != nil {
		t.Fatalf("Embed query: %v", err)
	}
	results, err := db.SearchByEmbedding(d, qv, "", "", 10)
	if err != nil {
		t.Fatalf("SearchByEmbedding: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result, got none")
	}
	if results[0].Title != "Server setup" {
		t.Errorf("expected 'Server setup' ranked first, got %q", results[0].Title)
		for i, r := range results {
			t.Logf("  #%d: [%s] %s", i+1, r.LibID, r.Title)
		}
	}
}

func TestSearchByEmbedding_FiltersByLib(t *testing.T) {
	d := openTestDB(t)

	docs := []db.Doc{
		{LibID: "go-sdk", Title: "Server setup", Content: "Create a new MCP server"},
		{LibID: "libsql", Title: "SQL server", Content: "Connect to a database server"},
	}
	for _, doc := range docs {
		if err := db.Insert(d, doc, embedText(t, testEmbedder, doc)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	qv, err := testEmbedder.EmbedQuery("server")
	if err != nil {
		t.Fatalf("Embed query: %v", err)
	}
	results, err := db.SearchByEmbedding(d, qv, "go-sdk", "", 10)
	if err != nil {
		t.Fatalf("SearchByEmbedding: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result, got none")
	}
	for _, r := range results {
		if r.LibID != "go-sdk" {
			t.Errorf("filter failed: got lib_id=%q, expected go-sdk", r.LibID)
		}
	}
}

// TestSearchByEmbedding_Acceptance is the unit-test version of the issue's
// acceptance criterion: "register a tool" finds the mcp.AddTool snippet via
// real semantic similarity, even though the query uses natural language and
// the target snippet uses an identifier.
func TestSearchByEmbedding_Acceptance(t *testing.T) {
	d := openTestDB(t)

	docs := []db.Doc{
		{LibID: "go-sdk", Title: "Server setup", Content: "Create a new MCP server with mcp.NewServer"},
		{LibID: "go-sdk", Title: "Tool registration", Content: "Register tools using mcp.AddTool"},
		{LibID: "libsql", Title: "Getting started", Content: "Open a database with sql.Open"},
	}
	for _, doc := range docs {
		if err := db.Insert(d, doc, embedText(t, testEmbedder, doc)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	qv, err := testEmbedder.EmbedQuery("register a tool")
	if err != nil {
		t.Fatalf("Embed query: %v", err)
	}
	results, err := db.SearchByEmbedding(d, qv, "", "", 10)
	if err != nil {
		t.Fatalf("SearchByEmbedding: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result, got none")
	}
	if results[0].Title != "Tool registration" {
		t.Errorf("expected 'Tool registration' ranked first, got %q", results[0].Title)
		for i, r := range results {
			t.Logf("  #%d: [%s] %s", i+1, r.LibID, r.Title)
		}
	}
}

// TestSearchByEmbedding_FiltersByLibAllVersions seeds one lib_id
// with two versions and asserts that the lib-scoped search
// (version == "") surfaces both versions' rows.
func TestSearchByEmbedding_FiltersByLibAllVersions(t *testing.T) {
	d := openTestDB(t)

	docs := []db.Doc{
		{LibID: "/foo/tf", Version: "v1.14", Title: "tf install", Content: "install in v1.14"},
		{LibID: "/foo/tf", Version: "v1.13", Title: "tf install", Content: "install in v1.13"},
		{LibID: "/other/lib", Version: "", Title: "unrelated", Content: "noise"},
	}
	for _, doc := range docs {
		if err := db.Insert(d, doc, embedText(t, testEmbedder, doc)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	qv, err := testEmbedder.EmbedQuery("install")
	if err != nil {
		t.Fatalf("Embed query: %v", err)
	}
	results, err := db.SearchByEmbedding(d, qv, "/foo/tf", "", 10)
	if err != nil {
		t.Fatalf("SearchByEmbedding: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	seen := map[string]bool{}
	for _, r := range results {
		if r.LibID != "/foo/tf" {
			t.Errorf("filter leaked: LibID = %q", r.LibID)
		}
		seen[r.Version] = true
	}
	if !seen["v1.14"] || !seen["v1.13"] {
		t.Errorf("expected both versions, got %v", seen)
	}
}

// TestSearchByEmbedding_FiltersByLibAndVersion pins the two-arg
// filter path: when both lib and version are supplied, only the
// matching (lib_id, version) rows come back.
func TestSearchByEmbedding_FiltersByLibAndVersion(t *testing.T) {
	d := openTestDB(t)

	docs := []db.Doc{
		{LibID: "/foo/tf", Version: "v1.14", Title: "tf install", Content: "install in v1.14"},
		{LibID: "/foo/tf", Version: "v1.13", Title: "tf install", Content: "install in v1.13"},
	}
	for _, doc := range docs {
		if err := db.Insert(d, doc, embedText(t, testEmbedder, doc)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	qv, err := testEmbedder.EmbedQuery("install")
	if err != nil {
		t.Fatalf("Embed query: %v", err)
	}
	results, err := db.SearchByEmbedding(d, qv, "/foo/tf", "v1.14", 10)
	if err != nil {
		t.Fatalf("SearchByEmbedding: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (v1.14 only)", len(results))
	}
	if results[0].Version != "v1.14" {
		t.Errorf("Version = %q, want v1.14", results[0].Version)
	}
}

// TestDB_RejectsEmbedderMismatch is the meta-enforcement test from the
// issue: a DB created with one embedder kind must refuse to be reopened
// with a different one. The check fires at Open time so callers cannot
// even reach an Insert with mismatched vectors.
func TestDB_RejectsEmbedderMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	d, err := db.Open(path, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// Indexing one doc to confirm we are exercising a real, populated DB
	// and not a degenerate empty file.
	doc := db.Doc{LibID: "x", Title: "t", Content: "c"}
	if err := db.Insert(d, doc, embedText(t, testEmbedder, doc)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cases := []struct {
		name string
		meta db.Meta
	}{
		{
			name: "different kind",
			meta: db.Meta{EmbedderKind: "fake", EmbeddingDim: testEmbedder.Dim(), ModelVersion: testEmbedder.ModelVersion()},
		},
		{
			name: "different dim",
			meta: db.Meta{EmbedderKind: testEmbedder.Kind(), EmbeddingDim: testEmbedder.Dim() + 1, ModelVersion: testEmbedder.ModelVersion()},
		},
		{
			name: "different model version",
			meta: db.Meta{EmbedderKind: testEmbedder.Kind(), EmbeddingDim: testEmbedder.Dim(), ModelVersion: "made-up-model-v9"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reopened, err := db.Open(path, tc.meta)
			if err == nil {
				reopened.Close()
				t.Fatal("expected ErrEmbedderMismatch, got nil")
			}
			if !errors.Is(err, db.ErrEmbedderMismatch) {
				t.Errorf("expected ErrEmbedderMismatch, got %v", err)
			}
		})
	}
}

// TestDB_RoundtripsMeta verifies that the meta the embedder reports is
// what gets persisted, and that reopening the DB exposes the same Meta
// via the wrapper struct without the caller having to re-read the table.
func TestDB_RoundtripsMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	want := metaFor(testEmbedder)

	d, err := db.Open(path, want)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if d.Meta != want {
		t.Errorf("first open Meta = %+v, want %+v", d.Meta, want)
	}
	doc := db.Doc{LibID: "lib", Title: "Title", Content: "Content"}
	if err := db.Insert(d, doc, embedText(t, testEmbedder, doc)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := db.Open(path, want)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Meta != want {
		t.Errorf("reopened Meta = %+v, want %+v", reopened.Meta, want)
	}
}

// countingEmbedder wraps an embed.Embedder and counts how many times
// Embed has been called. Used by TestUpsertLibIfNew_Idempotent to prove
// that the second upsert for the same lib_id is a literal no-op at the
// embedder level (the issue's "at most one Embed call per lib in the
// lifetime of the database" guarantee).
type countingEmbedder struct {
	inner embed.Embedder
	calls int
}

func (c *countingEmbedder) EmbedDocument(text string) ([]float32, error) {
	c.calls++
	return c.inner.EmbedDocument(text)
}

// TestUpsertLibIfNew_Idempotent is the load-bearing assertion behind the
// design's "embedding never changes once computed" claim: re-running
// the scraper for an existing lib must NOT call Embed a second time. We
// verify it by wrapping testEmbedder in a counter and inspecting the
// counter after two consecutive UpsertLibIfNew calls.
func TestUpsertLibIfNew_Idempotent(t *testing.T) {
	d := openTestDB(t)
	c := &countingEmbedder{inner: testEmbedder}

	if err := db.UpsertLibIfNew(d, "/facebook/react", "", c); err != nil {
		t.Fatalf("first UpsertLibIfNew: %v", err)
	}
	if c.calls != 1 {
		t.Fatalf("after first upsert: Embed called %d time(s), want 1", c.calls)
	}

	if err := db.UpsertLibIfNew(d, "/facebook/react", "", c); err != nil {
		t.Fatalf("second UpsertLibIfNew: %v", err)
	}
	if c.calls != 1 {
		t.Errorf("after second upsert: Embed called %d time(s), want 1 (re-upsert must not re-embed)", c.calls)
	}

	// And a sanity check: a *different* lib_id does trigger a fresh Embed
	// call. This catches the failure mode where UpsertLibIfNew gets
	// over-eager and short-circuits on any non-empty libs table.
	if err := db.UpsertLibIfNew(d, "/vercel/next.js", "", c); err != nil {
		t.Fatalf("upsert second lib: %v", err)
	}
	if c.calls != 2 {
		t.Errorf("after upserting a second lib: Embed called %d time(s), want 2", c.calls)
	}
}

// TestUpsertLibIfNew_AllowsSameLibDifferentVersion pins the #113
// "(lib_id, version) is the primary key" promise: two rows with the
// same lib_id and different versions must coexist, and each pair
// gets exactly one EmbedDocument call.
func TestUpsertLibIfNew_AllowsSameLibDifferentVersion(t *testing.T) {
	d := openTestDB(t)
	c := &countingEmbedder{inner: testEmbedder}

	if err := db.UpsertLibIfNew(d, "/hashicorp/terraform", "v1.14", c); err != nil {
		t.Fatalf("upsert v1.14: %v", err)
	}
	if err := db.UpsertLibIfNew(d, "/hashicorp/terraform", "v1.13", c); err != nil {
		t.Fatalf("upsert v1.13: %v", err)
	}
	// Two distinct (lib_id, version) pairs → two embed calls.
	if c.calls != 2 {
		t.Errorf("Embed called %d time(s), want 2 (one per version)", c.calls)
	}

	var count int
	if err := d.QueryRow(`SELECT count(*) FROM libs WHERE lib_id = ?`, "/hashicorp/terraform").Scan(&count); err != nil {
		t.Fatalf("count libs: %v", err)
	}
	if count != 2 {
		t.Errorf("libs rows for /hashicorp/terraform = %d, want 2", count)
	}

	// And the re-upsert of an existing pair is still idempotent (no
	// extra embed call).
	if err := db.UpsertLibIfNew(d, "/hashicorp/terraform", "v1.14", c); err != nil {
		t.Fatalf("re-upsert v1.14: %v", err)
	}
	if c.calls != 2 {
		t.Errorf("after re-upsert: Embed called %d time(s), want 2", c.calls)
	}
}

// TestUpdateLibCount_UpdatesRightRow verifies that UpdateLibCount only
// touches the row keyed by libID. The "untouched lib stays at 0" half
// of the assertion catches an obvious WHERE-clause bug; the "matching
// lib gets the new value" half catches an obvious set-vs-add bug.
func TestUpdateLibCount_UpdatesRightRow(t *testing.T) {
	d := openTestDB(t)

	for _, libID := range []string{"/a/one", "/b/two"} {
		if err := db.UpsertLibIfNew(d, libID, "", testEmbedder); err != nil {
			t.Fatalf("UpsertLibIfNew %q: %v", libID, err)
		}
	}

	if err := db.UpdateLibCount(d, "/a/one", "", 42); err != nil {
		t.Fatalf("UpdateLibCount: %v", err)
	}

	got := readLibCount(t, d, "/a/one")
	if got != 42 {
		t.Errorf("/a/one doc_count = %d, want 42", got)
	}
	other := readLibCount(t, d, "/b/two")
	if other != 0 {
		t.Errorf("/b/two doc_count = %d, want 0 (UpdateLibCount touched the wrong row)", other)
	}
}

func readLibCount(t *testing.T, d *db.DB, libID string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT doc_count FROM libs WHERE lib_id = ?`, libID).Scan(&n); err != nil {
		t.Fatalf("read doc_count for %q: %v", libID, err)
	}
	return n
}

// TestSearchLibsByEmbedding_RanksRelevantFirst is the headline test for
// the search_libraries handler's hot path: a free-text query must rank
// the semantically-closest lib first via lib_id-text-only embeddings.
// "terraform aws" should resolve to /hashicorp/terraform-provider-aws
// over the React and Express decoys; this is the exact "useful at
// Context7 scale" property the issue is asking for.
func TestSearchLibsByEmbedding_RanksRelevantFirst(t *testing.T) {
	d := openTestDB(t)

	libs := []string{
		"/hashicorp/terraform-provider-aws",
		"/facebook/react",
		"/expressjs/express",
	}
	for _, libID := range libs {
		if err := db.UpsertLibIfNew(d, libID, "", testEmbedder); err != nil {
			t.Fatalf("UpsertLibIfNew %q: %v", libID, err)
		}
	}

	qv, err := testEmbedder.EmbedQuery("terraform aws")
	if err != nil {
		t.Fatalf("Embed query: %v", err)
	}
	results, err := db.SearchLibsByEmbedding(d, qv, 10)
	if err != nil {
		t.Fatalf("SearchLibsByEmbedding: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result, got none")
	}
	if results[0].LibID != "/hashicorp/terraform-provider-aws" {
		t.Errorf("expected '/hashicorp/terraform-provider-aws' first, got %q", results[0].LibID)
		for i, r := range results {
			t.Logf("  #%d: %s (dist=%v)", i+1, r.LibID, r.Distance)
		}
	}
}

// TestSearchLibsByEmbedding_HonoursLimit pins down the cap-on-result-set
// guarantee from the tool's spec: even with three matching libs, asking
// for two must return exactly two. The order assertion is incidental;
// the count assertion is the load-bearing one.
func TestSearchLibsByEmbedding_HonoursLimit(t *testing.T) {
	d := openTestDB(t)

	for _, libID := range []string{"/a/one", "/b/two", "/c/three"} {
		if err := db.UpsertLibIfNew(d, libID, "", testEmbedder); err != nil {
			t.Fatalf("UpsertLibIfNew %q: %v", libID, err)
		}
	}

	qv, err := testEmbedder.EmbedQuery("anything")
	if err != nil {
		t.Fatalf("Embed query: %v", err)
	}
	results, err := db.SearchLibsByEmbedding(d, qv, 2)
	if err != nil {
		t.Fatalf("SearchLibsByEmbedding: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("limit=2 returned %d results, want 2", len(results))
	}
}

// TestTopLibsByDocCount_OrdersDescending pins the empty-name path of
// the search_libraries handler. doc_count desc is what gives the LLM a
// useful corpus-summary answer when it doesn't know what to ask for
// yet, and it has to be deterministic so the tool's behavior is
// reproducible across runs.
func TestTopLibsByDocCount_OrdersDescending(t *testing.T) {
	d := openTestDB(t)

	libs := []struct {
		id    string
		count int
	}{
		{"/small/lib", 3},
		{"/big/lib", 100},
		{"/medium/lib", 25},
	}
	for _, l := range libs {
		if err := db.UpsertLibIfNew(d, l.id, "", testEmbedder); err != nil {
			t.Fatalf("UpsertLibIfNew %q: %v", l.id, err)
		}
		if err := db.UpdateLibCount(d, l.id, "", l.count); err != nil {
			t.Fatalf("UpdateLibCount %q: %v", l.id, err)
		}
	}

	results, err := db.TopLibsByDocCount(d, 10)
	if err != nil {
		t.Fatalf("TopLibsByDocCount: %v", err)
	}
	want := []string{"/big/lib", "/medium/lib", "/small/lib"}
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d", len(results), len(want))
	}
	for i, w := range want {
		if results[i].LibID != w {
			t.Errorf("position %d: got %q, want %q", i, results[i].LibID, w)
		}
	}
}

// seedMainDB writes a fully-valid consolidated DB at path via the
// mutator path, inserts a handful of docs, and closes it. Used by the
// OpenReader tests to get a realistic on-disk file without repeating
// the Open/Insert boilerplate in every case.
func seedMainDB(t *testing.T, path string) {
	t.Helper()
	d, err := db.Open(path, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("seed: Open: %v", err)
	}
	docs := []db.Doc{
		{LibID: "/a/one", Title: "one", Content: "first doc"},
		{LibID: "/b/two", Title: "two", Content: "second doc"},
	}
	for _, doc := range docs {
		if err := db.Insert(d, doc, embedText(t, testEmbedder, doc)); err != nil {
			t.Fatalf("seed: Insert %q: %v", doc.Title, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("seed: Close: %v", err)
	}
}

// TestOpenReader_ExistingDB checks the happy path: a DB seeded via the
// mutator Open path can be reopened via OpenReader and answer SELECTs
// with the same row count. Covers AC bullet "TestOpenReader_existingDB".
func TestOpenReader_ExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reader.db")
	seedMainDB(t, path)

	d, err := db.OpenReader(path, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer d.Close()

	var count int
	if err := d.QueryRow(`SELECT count(*) FROM docs`).Scan(&count); err != nil {
		t.Fatalf("SELECT count(*): %v", err)
	}
	if count != 2 {
		t.Errorf("doc count = %d, want 2", count)
	}
	if d.Meta != metaFor(testEmbedder) {
		t.Errorf("Meta = %+v, want %+v", d.Meta, metaFor(testEmbedder))
	}
}

// TestOpenReader_RejectsWrite is the core invariant: once a DB is
// opened via OpenReader, the connection must refuse any write. Both an
// INSERT and a CREATE TABLE are attempted so the test catches a
// half-finished implementation that only guards one shape of mutation.
func TestOpenReader_RejectsWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reader.db")
	seedMainDB(t, path)

	d, err := db.OpenReader(path, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer d.Close()

	// INSERT must fail — the stored vector width matches the
	// embedder's dim, so if query_only didn't fire we'd silently
	// succeed and the test would pass for the wrong reason.
	insertVec := embedText(t, testEmbedder, db.Doc{Title: "t", Content: "c"})
	if err := db.Insert(d, db.Doc{LibID: "x", Title: "t", Content: "c"}, insertVec); err == nil {
		t.Error("Insert on reader: expected error, got nil")
	}

	// CREATE TABLE must also fail — the issue's acceptance criterion
	// explicitly names it because the mutator Open path itself issues
	// CREATE TABLE IF NOT EXISTS at boot, and a reader that still
	// allows DDL would re-introduce the exact lock contention we are
	// trying to eliminate.
	if _, err := d.Exec(`CREATE TABLE canary (id INTEGER PRIMARY KEY)`); err == nil {
		t.Error("CREATE TABLE on reader: expected error, got nil")
	}

	// UPDATE and DELETE round out the mutation surface. query_only
	// covers all three in one pragma, so a failure in any one branch
	// points at a regression in the pragma-lifetime handling rather
	// than a missing case in OpenReader.
	if _, err := d.Exec(`UPDATE docs SET title = 'x' WHERE lib_id = '/a/one'`); err == nil {
		t.Error("UPDATE on reader: expected error, got nil")
	}
	if _, err := d.Exec(`DELETE FROM docs WHERE lib_id = '/a/one'`); err == nil {
		t.Error("DELETE on reader: expected error, got nil")
	}
}

// TestOpenReader_MissingFile pins the "readers never spawn empty DBs"
// contract. If the path does not exist, OpenReader must return an
// os.ErrNotExist-wrapping error and must NOT create a stub file on
// disk — otherwise a typo in -db on the server CLI would produce a
// nonsense empty database that silently answers zero results.
func TestOpenReader_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-created.db")

	d, err := db.OpenReader(path, metaFor(testEmbedder))
	if err == nil {
		d.Close()
		t.Fatal("OpenReader on missing file: expected error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Errorf("OpenReader created a stub file at %s; it must not", path)
	}
}

// TestOpenReader_SchemaMismatch rebuilds a v1 (pre-libs) database by
// hand and asserts that OpenReader rejects it with ErrSchemaMismatch.
// Parity with TestDB_RejectsPreLibsSchema for the mutator path — the
// reader must surface the same sentinel so callers using errors.Is can
// treat the two paths interchangeably.
func TestOpenReader_SchemaMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create meta: %v", err)
	}
	for _, kv := range [][2]string{
		{"embedder_kind", testEmbedder.Kind()},
		{"embedding_dim", fmt.Sprintf("%d", testEmbedder.Dim())},
		{"model_version", testEmbedder.ModelVersion()},
	} {
		if _, err := raw.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
			t.Fatalf("insert meta %s: %v", kv[0], err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	d, err := db.OpenReader(path, metaFor(testEmbedder))
	if err == nil {
		d.Close()
		t.Fatal("expected ErrSchemaMismatch, got nil")
	}
	if !errors.Is(err, db.ErrSchemaMismatch) {
		t.Errorf("expected ErrSchemaMismatch, got %v", err)
	}
}

// TestOpenReader_EmbedderMismatch seeds a DB with the real embedder
// meta, then tries to OpenReader it with three different shapes of
// mismatched meta (kind, dim, model version). Mirrors
// TestDB_RejectsEmbedderMismatch so the reader and mutator paths share
// identical meta-enforcement semantics.
func TestOpenReader_EmbedderMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reader.db")
	seedMainDB(t, path)

	cases := []struct {
		name string
		meta db.Meta
	}{
		{
			name: "different kind",
			meta: db.Meta{EmbedderKind: "fake", EmbeddingDim: testEmbedder.Dim(), ModelVersion: testEmbedder.ModelVersion()},
		},
		{
			name: "different dim",
			meta: db.Meta{EmbedderKind: testEmbedder.Kind(), EmbeddingDim: testEmbedder.Dim() + 1, ModelVersion: testEmbedder.ModelVersion()},
		},
		{
			name: "different model version",
			meta: db.Meta{EmbedderKind: testEmbedder.Kind(), EmbeddingDim: testEmbedder.Dim(), ModelVersion: "made-up-model-v9"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.OpenReader(path, tc.meta)
			if err == nil {
				d.Close()
				t.Fatal("expected ErrEmbedderMismatch, got nil")
			}
			if !errors.Is(err, db.ErrEmbedderMismatch) {
				t.Errorf("expected ErrEmbedderMismatch, got %v", err)
			}
		})
	}
}

// listTables returns the names of every user-defined table in the
// database at path, in lexicographic order. Used by
// TestOpenReader_DoesNotIssueDDL to snapshot the schema around an
// OpenReader call.
func listTables(t *testing.T, path string) []string {
	t.Helper()
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatalf("listTables: open: %v", err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatalf("listTables: query: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("listTables: scan: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("listTables: rows: %v", err)
	}
	sort.Strings(names)
	return names
}

// TestOpenReader_DoesNotIssueDDL is the direct structural test for
// the #131 root-cause fix: OpenReader must not create, alter, or drop
// any table on the file it opens. Before the reader/mutator split,
// `deadzone server`'s boot would run `CREATE TABLE IF NOT EXISTS meta`
// unconditionally (db.go:137, pre-refactor), taking a SQLite
// write-intent lock on every start and racing other boots on the
// same file. This test snapshots the full sqlite_master table list
// before and after OpenReader and fails if the two differ — a much
// stronger assertion than "N concurrent readers don't time out",
// because it pins down WHY concurrent readers work rather than
// verifying the symptom.
func TestOpenReader_DoesNotIssueDDL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reader.db")
	seedMainDB(t, path)

	before := listTables(t, path)

	d, err := db.OpenReader(path, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	// Issue a representative SELECT so any lazy DDL the driver might
	// defer until first query has a chance to fire before we snapshot.
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM docs`).Scan(&n); err != nil {
		d.Close()
		t.Fatalf("SELECT: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after := listTables(t, path)
	if len(before) != len(after) {
		t.Fatalf("OpenReader changed table count: before=%v after=%v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("OpenReader altered schema: before=%v after=%v", before, after)
			break
		}
	}
}

// TestOpenReader_CoexistsInProcess holds a write-intent lock on the
// main DB via a mutator-opened `BEGIN IMMEDIATE`, then spawns
// multiple OpenReader calls against the same file. Asserts every
// reader opens and answers its SELECT without SQLITE_BUSY, proving
// the reader path is safe against an active in-process writer (#131
// boot-race). Combined with TestOpenReader_DoesNotIssueDDL.
//
// SCOPE — single process only: every goroutine shares the same
// tursogo driver instance and the same fcntl FD. The cross-process
// contract is pinned by TestOpenReader_MultiProcess (#172).
func TestOpenReader_CoexistsInProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reader.db")
	seedMainDB(t, path)

	mutator, err := db.Open(path, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("mutator Open: %v", err)
	}
	defer mutator.Close()

	// Pin a connection and hold a reserved lock via BEGIN IMMEDIATE.
	// SetMaxOpenConns(1) on the mutator side means there is a single
	// conn; Conn() returns it, and the BEGIN IMMEDIATE sticks on that
	// conn for the life of the test. Rollback on defer so we never
	// leak a half-open tx into the pool.
	ctx := context.Background()
	writerConn, err := mutator.Conn(ctx)
	if err != nil {
		t.Fatalf("mutator Conn: %v", err)
	}
	defer writerConn.Close()

	if _, err := writerConn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	defer func() {
		if _, err := writerConn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			t.Logf("ROLLBACK: %v", err)
		}
	}()

	// Force real acquisition of the reserved lock by issuing a write
	// on the pinned tx. BEGIN IMMEDIATE alone is intended to grab it,
	// but writing a row removes any ambiguity about driver behaviour.
	if _, err := writerConn.ExecContext(ctx,
		`UPDATE docs SET title = 'held-' || title WHERE lib_id = '/a/one'`); err != nil {
		t.Fatalf("writer UPDATE: %v", err)
	}

	// Now spawn N OpenReader calls. None of them should block
	// indefinitely or fail with SQLITE_BUSY. A 5-second implicit
	// timeout via the test runner's default would catch a hang; we
	// assert success inline.
	const readers = 3
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		counts []int
		errs   []error
	)
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			d, err := db.OpenReader(path, metaFor(testEmbedder))
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("OpenReader: %w", err))
				mu.Unlock()
				return
			}
			defer d.Close()
			var n int
			if err := d.QueryRow(`SELECT count(*) FROM docs`).Scan(&n); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("SELECT: %w", err))
				mu.Unlock()
				return
			}
			mu.Lock()
			counts = append(counts, n)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("reader vs held writer: %v", e)
		}
		t.FailNow()
	}
	if len(counts) != readers {
		t.Fatalf("got %d reader results, want %d", len(counts), readers)
	}
	// Sanity: readers all see the pre-writer state (writer's tx is
	// uncommitted, so its UPDATE is invisible). Seed inserts 2 rows.
	for i, n := range counts {
		if n != 2 {
			t.Errorf("reader %d: count=%d, want 2 (uncommitted writer must be invisible)", i, n)
		}
	}
}

// TestOpenReader_MultiProcess pins the user-visible cross-process
// behavior of issue #172. Two sub-tests cover the two paths the
// production code can take:
//
//   - CoexistsWithEnvVar — the primary path: cmd/deadzone/server.go
//     sets LIMBO_DISABLE_FILE_LOCK=1 so tursogo skips the OS-level
//     fcntl lock on the database file. With both processes running
//     under that env var, db.OpenReader succeeds in both — restoring
//     the #131 contract that N concurrent `deadzone server` processes
//     can share the same deadzone.db.
//
//   - FallbackErrReaderBusyWithoutEnvVar — defense-in-depth: if a
//     wrapper strips the env var (sandbox, env scrubber, future
//     tursogo bump renaming the var), the second process must still
//     fail with db.ErrReaderBusy and a message naming the file —
//     never the raw tursogo "Locking error: Failed locking file"
//     string the user originally reported.
//
// Mechanics: each sub-test re-execs its own test binary in a "hold"
// mode (TestMain short-circuits on the DEADZONE_TEST_HOLD_DB_PATH env
// var, runs runHoldDBHelper, and exits without loading Hugot). The
// helper takes the lock via the same sql.Open + PRAGMA query_only
// call sequence OpenReader uses, prints "ready" on stdout, and
// blocks on stdin EOF. Re-execing the test binary instead of the
// real `deadzone` binary keeps `go test` self-contained — the OS
// lock is what matters and the helper acquires it through
// byte-identical driver calls.
//
// Not gated on `-short`: the contract this validates is the user-
// visible failure mode and CI must catch a regression unconditionally.
func TestOpenReader_MultiProcess(t *testing.T) {
	t.Run("CoexistsWithEnvVar", func(t *testing.T) {
		// Children inherit os.Environ(); t.Setenv restores the prior
		// state on exit so sibling tests are not affected.
		t.Setenv(db.EnvDisableFileLock, "1")

		path := filepath.Join(t.TempDir(), "reader.db")
		seedMainDB(t, path)
		startHoldHelper(t, path)

		d, err := db.OpenReader(path, metaFor(testEmbedder))
		if err != nil {
			t.Fatalf("OpenReader against held file: got %v, want success (env-var bypass should let processes coexist)", err)
		}
		defer d.Close()

		var n int
		if err := d.QueryRow(`SELECT count(*) FROM docs`).Scan(&n); err != nil {
			t.Fatalf("parent SELECT count(*): %v", err)
		}
		if n < 1 {
			t.Errorf("docs count = %d, want >= 1 (seedMainDB should populate)", n)
		}
	})

	t.Run("FallbackErrReaderBusyWithoutEnvVar", func(t *testing.T) {
		// Register restore BEFORE mutating the env so a panic between
		// the two lines cannot leak state to sibling tests.
		old, present := os.LookupEnv(db.EnvDisableFileLock)
		t.Cleanup(func() {
			if present {
				os.Setenv(db.EnvDisableFileLock, old)
			} else {
				os.Unsetenv(db.EnvDisableFileLock)
			}
		})
		os.Unsetenv(db.EnvDisableFileLock)

		path := filepath.Join(t.TempDir(), "reader.db")
		seedMainDB(t, path)
		startHoldHelper(t, path)

		d, err := db.OpenReader(path, metaFor(testEmbedder))
		if err == nil {
			_ = d.Close()
			t.Fatal("OpenReader unexpectedly succeeded against a held file with the env var unset; tursogo should have rejected the open")
		}
		if !errors.Is(err, db.ErrReaderBusy) {
			t.Fatalf("OpenReader error = %v; want errors.Is(_, db.ErrReaderBusy)", err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("OpenReader error = %q; want it to mention the DB path %q", err.Error(), path)
		}
	})
}

// startHoldHelper re-execs the test binary in DB-holder mode against
// path and registers its own cleanup so the child cannot be orphaned
// even if the caller t.Fatals before its own cleanup line. The
// helper inherits the parent's environment, so each sub-test
// controls the LIMBO_DISABLE_FILE_LOCK policy by setting it before
// the call.
func startHoldHelper(t *testing.T, path string) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run", "^$")
	cmd.Env = append(os.Environ(), "DEADZONE_TEST_HOLD_DB_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hold helper: %v", err)
	}
	// Register cleanup the moment the child is alive — Kill + Wait
	// regardless of how the readiness check below resolves.
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	readyCh := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			if scanner.Text() != "ready" {
				readyCh <- fmt.Errorf("hold helper: unexpected line %q", scanner.Text())
				return
			}
			readyCh <- nil
			return
		}
		if err := scanner.Err(); err != nil {
			readyCh <- err
			return
		}
		readyCh <- io.EOF
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			t.Fatalf("hold helper never became ready: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("hold helper readiness timeout")
	}
}

// TestOpenReader_PinsSingleConnection freezes the pool-posture
// invariant that the read-only contract relies on. PRAGMA query_only
// is per-connection, not per-DB — if a future change raises
// SetMaxOpenConns past 1 (e.g. to parallelize reads under #45), the
// database/sql pool can spawn a second, un-pragma'd connection and
// silently accept writes on it. Pinning the cap at 1 at Stats() level
// catches that regression before it can hit prod.
func TestOpenReader_PinsSingleConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reader.db")
	seedMainDB(t, path)

	d, err := db.OpenReader(path, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer d.Close()

	stats := d.Stats()
	if stats.MaxOpenConnections != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1; raising the cap requires re-establishing PRAGMA query_only per-connection (see #131 — query_only is not a DB-level pragma)",
			stats.MaxOpenConnections)
	}
}

// TestOpenReader_RejectsForeignSqliteFile covers the case where the
// path points at a SQLite file the mutator path never touched — e.g.
// a user pointed -db at somebody else's database by mistake, or a
// half-failed bootstrap produced a file with an unrelated schema.
// A readable file with no meta table must surface as
// ErrReaderNotInitialized (actionable: "run consolidate") rather
// than as a raw "no such table: meta" driver error (which looks like
// corruption). A fresh deadzone.db created by a mutator always has
// the meta table, so this branch does not fire on in-tree workflows
// — it guards against pointing OpenReader at foreign files.
func TestOpenReader_RejectsForeignSqliteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")

	// Create the file via the driver without any schema. This is what
	// a stray `touch empty.db` or a half-failed bootstrap looks like.
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	// Force the file to actually exist on disk by issuing a trivial
	// query; tursogo defers the create until the first operation.
	if _, err := raw.Exec(`CREATE TABLE canary (x INTEGER)`); err != nil {
		t.Fatalf("raw exec: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	d, err := db.OpenReader(path, metaFor(testEmbedder))
	if err == nil {
		d.Close()
		t.Fatal("expected ErrReaderNotInitialized, got nil")
	}
	if !errors.Is(err, db.ErrReaderNotInitialized) {
		t.Errorf("expected ErrReaderNotInitialized, got %v", err)
	}
}

// TestDB_RejectsPreLibsSchema simulates an old (pre-libs) database file
// by hand-crafting a meta table without a schema_version key. Opening
// such a file with the current build must fail with ErrSchemaMismatch
// — the migration story for issue #44 is "drop & re-scrape", and this
// test guarantees the old DB can't silently slip through and produce
// nonsense search_libraries responses (empty libs table, no rows).
func TestDB_RejectsPreLibsSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Open the file directly through the driver and write the v1 meta
	// layout: just the three embedder keys, no schema_version. This is
	// what every database created before issue #44 looks like on disk.
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create meta: %v", err)
	}
	for _, kv := range [][2]string{
		{"embedder_kind", testEmbedder.Kind()},
		{"embedding_dim", fmt.Sprintf("%d", testEmbedder.Dim())},
		{"model_version", testEmbedder.ModelVersion()},
	} {
		if _, err := raw.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
			t.Fatalf("insert meta %s: %v", kv[0], err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	reopened, err := db.Open(path, metaFor(testEmbedder))
	if err == nil {
		reopened.Close()
		t.Fatal("expected ErrSchemaMismatch on pre-libs DB, got nil")
	}
	if !errors.Is(err, db.ErrSchemaMismatch) {
		t.Errorf("expected ErrSchemaMismatch, got %v", err)
	}
}
