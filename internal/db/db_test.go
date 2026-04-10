package db_test

import (
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

func TestOpen_CreatesDocsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// Verify the table exists by inserting through the real Insert path.
	e := embed.NewStub()
	doc := db.Doc{LibID: "testlib", Title: "Hello World", Content: "some content"}
	if err := db.Insert(d, doc, embedText(e, doc)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

func TestInsert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

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
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	err = db.Insert(d, db.Doc{LibID: "x", Title: "t", Content: "c"}, []float32{0.1, 0.2})
	if err == nil {
		t.Fatal("expected error for wrong-dimension embedding, got nil")
	}
}

func TestSearchByEmbedding_RanksRelevantFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

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
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

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
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

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

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
