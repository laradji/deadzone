package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/laradji/deadzone/internal/db"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHandleSearchDocs_ReturnsSnippets(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	docs := []db.Doc{
		{LibID: "/modelcontextprotocol/go-sdk", Title: "Tools", Content: "Use mcp.AddTool to register tools on the server."},
		{LibID: "/modelcontextprotocol/go-sdk", Title: "Server setup", Content: "Create a server with mcp.NewServer."},
		{LibID: "/other/lib", Title: "Unrelated", Content: "Something about AddTool in another library."},
	}
	for _, doc := range docs {
		if err := db.Insert(d, doc); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	handler := makeSearchHandler(d)

	t.Run("returns relevant snippets", func(t *testing.T) {
		_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchDocsInput{
			Query: "AddTool",
		})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if len(out.Snippets) == 0 {
			t.Fatal("expected snippets, got none")
		}
		for _, s := range out.Snippets {
			t.Logf("snippet: [%s] %s", s.LibID, s.Title)
		}
	})

	t.Run("filters by lib_id", func(t *testing.T) {
		_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchDocsInput{
			Query: "AddTool",
			LibID: "/modelcontextprotocol/go-sdk",
		})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		for _, s := range out.Snippets {
			if s.LibID != "/modelcontextprotocol/go-sdk" {
				t.Errorf("got lib_id=%q, want /modelcontextprotocol/go-sdk", s.LibID)
			}
		}
	})

	t.Run("empty query returns no error", func(t *testing.T) {
		_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchDocsInput{
			Query: "nonexistentxyzabc",
		})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if out.Snippets == nil {
			t.Error("snippets should be non-nil slice")
		}
	})
}
