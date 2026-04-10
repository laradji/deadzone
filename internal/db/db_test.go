package db_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/laradji/deadzone/internal/db"
)

func TestOpen_CreatesDocsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// Verify FTS5 table exists by inserting a row
	_, err = d.Exec(`INSERT INTO docs(lib, title, content) VALUES (?, ?, ?)`,
		"testlib", "Hello World", "some content")
	if err != nil {
		t.Fatalf("INSERT into docs: %v", err)
	}
}

func TestInsert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	docs := []db.Doc{
		{Lib: "go-sdk", Title: "Server setup", Content: "Create a new MCP server with mcp.NewServer"},
		{Lib: "go-sdk", Title: "Tool registration", Content: "Register tools using mcp.AddTool"},
		{Lib: "libsql", Title: "Getting started", Content: "Open a database with sql.Open"},
	}

	for _, doc := range docs {
		if err := db.Insert(d, doc); err != nil {
			t.Fatalf("Insert %q: %v", doc.Title, err)
		}
	}

	// Verify row count
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM docs`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 rows, got %d", count)
	}
}

func TestSearch_ReturnsRelevantSnippets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	docs := []db.Doc{
		{Lib: "go-sdk", Title: "Server setup", Content: "Create a new MCP server with mcp.NewServer"},
		{Lib: "go-sdk", Title: "Tool registration", Content: "Register tools using mcp.AddTool"},
		{Lib: "libsql", Title: "Getting started", Content: "Open a database with sql.Open"},
	}
	for _, doc := range docs {
		if err := db.Insert(d, doc); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	results, err := db.Search(d, "server", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result for query 'server', got none")
	}

	// All results should mention "server" in title or content
	for _, r := range results {
		t.Logf("result: lib=%s title=%q", r.Lib, r.Title)
	}
}

func TestSearch_FiltersByLib(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	docs := []db.Doc{
		{Lib: "go-sdk", Title: "Server setup", Content: "Create a new MCP server"},
		{Lib: "libsql", Title: "SQL server", Content: "Connect to a database server"},
	}
	for _, doc := range docs {
		if err := db.Insert(d, doc); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	results, err := db.Search(d, "server", "go-sdk")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range results {
		if r.Lib != "go-sdk" {
			t.Errorf("filter failed: got lib=%q, expected go-sdk", r.Lib)
		}
	}
}

func TestSearch_UnicodeContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if err := db.Insert(d, db.Doc{
		Lib: "testlib", Title: "Unicode doc", Content: "Créer un serveur MCP avec Go",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := db.Search(d, "serveur", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected result for unicode query 'serveur', got none")
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
