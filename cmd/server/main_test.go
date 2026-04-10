package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHandleSearchDocs_ReturnsSnippets(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	e := embed.NewStub()
	docs := []db.Doc{
		{LibID: "/modelcontextprotocol/go-sdk", Title: "Tool registration", Content: "Use mcp.AddTool to register tools on the server."},
		{LibID: "/modelcontextprotocol/go-sdk", Title: "Server setup", Content: "Create a server with mcp.NewServer."},
		{LibID: "/other/lib", Title: "Unrelated", Content: "Something about databases and queries."},
	}
	for _, doc := range docs {
		vec := e.Embed(doc.Title + "\n" + doc.Content)
		if err := db.Insert(d, doc, vec); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	handler := makeSearchHandler(d, e)

	t.Run("returns relevant snippets for semantic query", func(t *testing.T) {
		_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchDocsInput{
			Query: "register a tool",
		})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if len(out.Snippets) == 0 {
			t.Fatal("expected snippets, got none")
		}
		// Top snippet should be the Tool registration doc, proving that the
		// stub embedder + vector search can satisfy natural-language queries
		// via camelCase-split token overlap.
		if out.Snippets[0].Title != "Tool registration" {
			t.Errorf("expected 'Tool registration' ranked first, got %q", out.Snippets[0].Title)
			for i, s := range out.Snippets {
				t.Logf("  #%d: [%s] %s", i+1, s.LibID, s.Title)
			}
		}
	})

	t.Run("filters by lib_id", func(t *testing.T) {
		_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchDocsInput{
			Query: "register a tool",
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

	t.Run("nonsense query returns non-nil slice", func(t *testing.T) {
		_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchDocsInput{
			Query: "zzz nonsense xyzabc",
		})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if out.Snippets == nil {
			t.Error("snippets should be non-nil slice")
		}
	})
}
