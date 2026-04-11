package db_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
	_ "turso.tech/database/tursogo"
)

// testEmbedder is the package-level Hugot shared by every test in this
// package. Built once in TestMain so the model download + GoMLX session
// warm-up cost is amortized over the whole test run.
var testEmbedder *embed.Hugot

func TestMain(m *testing.M) {
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

// embedText is a small convenience that mirrors what the scraper and server
// do in real life: embed "Title\nContent" into a single vector. Tests call
// it with t.Helper-style fatality so an embedder failure aborts the test
// rather than silently passing nil through to db.Insert.
func embedText(t *testing.T, e embed.Embedder, d db.Doc) []float32 {
	t.Helper()
	v, err := e.Embed(d.Title + "\n" + d.Content)
	if err != nil {
		t.Fatalf("Embed %q: %v", d.Title, err)
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

	qv, err := testEmbedder.Embed("create a server")
	if err != nil {
		t.Fatalf("Embed query: %v", err)
	}
	results, err := db.SearchByEmbedding(d, qv, "", 10)
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

	qv, err := testEmbedder.Embed("server")
	if err != nil {
		t.Fatalf("Embed query: %v", err)
	}
	results, err := db.SearchByEmbedding(d, qv, "go-sdk", 10)
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

	qv, err := testEmbedder.Embed("register a tool")
	if err != nil {
		t.Fatalf("Embed query: %v", err)
	}
	results, err := db.SearchByEmbedding(d, qv, "", 10)
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

func (c *countingEmbedder) Embed(text string) ([]float32, error) {
	c.calls++
	return c.inner.Embed(text)
}

// TestUpsertLibIfNew_Idempotent is the load-bearing assertion behind the
// design's "embedding never changes once computed" claim: re-running
// the scraper for an existing lib must NOT call Embed a second time. We
// verify it by wrapping testEmbedder in a counter and inspecting the
// counter after two consecutive UpsertLibIfNew calls.
func TestUpsertLibIfNew_Idempotent(t *testing.T) {
	d := openTestDB(t)
	c := &countingEmbedder{inner: testEmbedder}

	if err := db.UpsertLibIfNew(d, "/facebook/react", c); err != nil {
		t.Fatalf("first UpsertLibIfNew: %v", err)
	}
	if c.calls != 1 {
		t.Fatalf("after first upsert: Embed called %d time(s), want 1", c.calls)
	}

	if err := db.UpsertLibIfNew(d, "/facebook/react", c); err != nil {
		t.Fatalf("second UpsertLibIfNew: %v", err)
	}
	if c.calls != 1 {
		t.Errorf("after second upsert: Embed called %d time(s), want 1 (re-upsert must not re-embed)", c.calls)
	}

	// And a sanity check: a *different* lib_id does trigger a fresh Embed
	// call. This catches the failure mode where UpsertLibIfNew gets
	// over-eager and short-circuits on any non-empty libs table.
	if err := db.UpsertLibIfNew(d, "/vercel/next.js", c); err != nil {
		t.Fatalf("upsert second lib: %v", err)
	}
	if c.calls != 2 {
		t.Errorf("after upserting a second lib: Embed called %d time(s), want 2", c.calls)
	}
}

// TestUpdateLibCount_UpdatesRightRow verifies that UpdateLibCount only
// touches the row keyed by libID. The "untouched lib stays at 0" half
// of the assertion catches an obvious WHERE-clause bug; the "matching
// lib gets the new value" half catches an obvious set-vs-add bug.
func TestUpdateLibCount_UpdatesRightRow(t *testing.T) {
	d := openTestDB(t)

	for _, libID := range []string{"/a/one", "/b/two"} {
		if err := db.UpsertLibIfNew(d, libID, testEmbedder); err != nil {
			t.Fatalf("UpsertLibIfNew %q: %v", libID, err)
		}
	}

	if err := db.UpdateLibCount(d, "/a/one", 42); err != nil {
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
		if err := db.UpsertLibIfNew(d, libID, testEmbedder); err != nil {
			t.Fatalf("UpsertLibIfNew %q: %v", libID, err)
		}
	}

	qv, err := testEmbedder.Embed("terraform aws")
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
		if err := db.UpsertLibIfNew(d, libID, testEmbedder); err != nil {
			t.Fatalf("UpsertLibIfNew %q: %v", libID, err)
		}
	}

	qv, err := testEmbedder.Embed("anything")
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
		if err := db.UpsertLibIfNew(d, l.id, testEmbedder); err != nil {
			t.Fatalf("UpsertLibIfNew %q: %v", l.id, err)
		}
		if err := db.UpdateLibCount(d, l.id, l.count); err != nil {
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
