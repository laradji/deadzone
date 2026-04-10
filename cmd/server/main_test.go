package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testEmbedder is the package-level Hugot shared by every test in this
// package. Built once in TestMain so the model download + GoMLX session
// warm-up cost only happens once per `go test ./cmd/server/...`.
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

func TestHandleSearchDocs_ReturnsSnippets(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"), db.Meta{
		EmbedderKind: testEmbedder.Kind(),
		EmbeddingDim: testEmbedder.Dim(),
		ModelVersion: testEmbedder.ModelVersion(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	docs := []db.Doc{
		{LibID: "/modelcontextprotocol/go-sdk", Title: "Tool registration", Content: "Use mcp.AddTool to register tools on the server."},
		{LibID: "/modelcontextprotocol/go-sdk", Title: "Server setup", Content: "Create a server with mcp.NewServer."},
		{LibID: "/other/lib", Title: "Unrelated", Content: "Something about databases and queries."},
	}
	for _, doc := range docs {
		vec := testEmbedder.Embed(doc.Title + "\n" + doc.Content)
		if err := db.Insert(d, doc, vec); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	handler := makeSearchHandler(d, testEmbedder, false)

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
		// hugot embedder + vector search can satisfy natural-language queries
		// via real semantic similarity.
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
