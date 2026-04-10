package db_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
)

// embedText is a small convenience that mirrors what the scraper and server
// do in real life: embed "Title\nContent" into a single vector.
func embedText(e embed.Embedder, d db.Doc) []float32 {
	return e.Embed(d.Title + "\n" + d.Content)
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

func openStub(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path, metaFor(embed.NewStub()))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpen_CreatesDocsTable(t *testing.T) {
	d := openStub(t)

	// Verify the table exists by inserting through the real Insert path.
	e := embed.NewStub()
	doc := db.Doc{LibID: "testlib", Title: "Hello World", Content: "some content"}
	if err := db.Insert(d, doc, embedText(e, doc)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

func TestInsert(t *testing.T) {
	d := openStub(t)

	e := embed.NewStub()
	docs := []db.Doc{
		{LibID: "go-sdk", Title: "Server setup", Content: "Create a new MCP server with mcp.NewServer"},
		{LibID: "go-sdk", Title: "Tool registration", Content: "Register tools using mcp.AddTool"},
		{LibID: "libsql", Title: "Getting started", Content: "Open a database with sql.Open"},
	}

	for _, doc := range docs {
		if err := db.Insert(d, doc, embedText(e, doc)); err != nil {
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
	d := openStub(t)

	err := db.Insert(d, db.Doc{LibID: "x", Title: "t", Content: "c"}, []float32{0.1, 0.2})
	if err == nil {
		t.Fatal("expected error for wrong-dimension embedding, got nil")
	}
}

func TestSearchByEmbedding_RanksRelevantFirst(t *testing.T) {
	d := openStub(t)

	e := embed.NewStub()
	docs := []db.Doc{
		{LibID: "go-sdk", Title: "Server setup", Content: "Create a new MCP server with mcp.NewServer"},
		{LibID: "go-sdk", Title: "Tool registration", Content: "Register tools using mcp.AddTool"},
		{LibID: "libsql", Title: "Getting started", Content: "Open a database with sql.Open"},
	}
	for _, doc := range docs {
		if err := db.Insert(d, doc, embedText(e, doc)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	results, err := db.SearchByEmbedding(d, e.Embed("create a server"), "", 10)
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
	d := openStub(t)

	e := embed.NewStub()
	docs := []db.Doc{
		{LibID: "go-sdk", Title: "Server setup", Content: "Create a new MCP server"},
		{LibID: "libsql", Title: "SQL server", Content: "Connect to a database server"},
	}
	for _, doc := range docs {
		if err := db.Insert(d, doc, embedText(e, doc)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	results, err := db.SearchByEmbedding(d, e.Embed("server"), "go-sdk", 10)
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
// semantic overlap (camelCase split + token bag), even though the query
// uses natural language and the target snippet uses an identifier.
func TestSearchByEmbedding_Acceptance(t *testing.T) {
	d := openStub(t)

	e := embed.NewStub()
	docs := []db.Doc{
		{LibID: "go-sdk", Title: "Server setup", Content: "Create a new MCP server with mcp.NewServer"},
		{LibID: "go-sdk", Title: "Tool registration", Content: "Register tools using mcp.AddTool"},
		{LibID: "libsql", Title: "Getting started", Content: "Open a database with sql.Open"},
	}
	for _, doc := range docs {
		if err := db.Insert(d, doc, embedText(e, doc)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	results, err := db.SearchByEmbedding(d, e.Embed("register a tool"), "", 10)
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

	stub := embed.NewStub()
	d, err := db.Open(path, metaFor(stub))
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// Indexing one doc to confirm we are exercising a real, populated DB
	// and not a degenerate empty file.
	doc := db.Doc{LibID: "x", Title: "t", Content: "c"}
	if err := db.Insert(d, doc, embedText(stub, doc)); err != nil {
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
			meta: db.Meta{EmbedderKind: "fake", EmbeddingDim: stub.Dim(), ModelVersion: stub.ModelVersion()},
		},
		{
			name: "different dim",
			meta: db.Meta{EmbedderKind: stub.Kind(), EmbeddingDim: stub.Dim() + 1, ModelVersion: stub.ModelVersion()},
		},
		{
			name: "different model version",
			meta: db.Meta{EmbedderKind: stub.Kind(), EmbeddingDim: stub.Dim(), ModelVersion: "stub-v2"},
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
	stub := embed.NewStub()
	want := metaFor(stub)

	d, err := db.Open(path, want)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if d.Meta != want {
		t.Errorf("first open Meta = %+v, want %+v", d.Meta, want)
	}
	doc := db.Doc{LibID: "lib", Title: "Title", Content: "Content"}
	if err := db.Insert(d, doc, embedText(stub, doc)); err != nil {
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

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
