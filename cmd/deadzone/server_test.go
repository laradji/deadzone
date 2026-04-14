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
// package. Built once in TestMain so the model download + ORT session
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
		vec, err := testEmbedder.EmbedDocument(doc.Title + "\n" + doc.Content)
		if err != nil {
			t.Fatalf("Embed %q: %v", doc.Title, err)
		}
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

// TestHandleSearchDocs_VersionRequiresLibID pins the usage-error
// guard: passing version without lib_id is rejected before any
// embed/query work.
func TestHandleSearchDocs_VersionRequiresLibID(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"), db.Meta{
		EmbedderKind: testEmbedder.Kind(),
		EmbeddingDim: testEmbedder.Dim(),
		ModelVersion: testEmbedder.ModelVersion(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	handler := makeSearchHandler(d, testEmbedder, false)

	_, _, err = handler(context.Background(), &mcp.CallToolRequest{}, SearchDocsInput{
		Query:   "anything",
		Version: "v1.14",
	})
	if err == nil {
		t.Fatal("expected error for version without lib_id, got nil")
	}
}

// TestHandleSearchDocs_VersionFiltersAndSurfaces seeds two versions of
// the same lib_id and asserts the filtered query returns only that
// version's snippets AND that each snippet carries the version in the
// Snippet.Version field the LLM sees.
func TestHandleSearchDocs_VersionFiltersAndSurfaces(t *testing.T) {
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
		{LibID: "/hashicorp/terraform", Version: "v1.14", Title: "tf v1.14 intro", Content: "v1.14 install instructions"},
		{LibID: "/hashicorp/terraform", Version: "v1.13", Title: "tf v1.13 intro", Content: "v1.13 install instructions"},
	}
	for _, doc := range docs {
		vec, err := testEmbedder.EmbedDocument(doc.Title + "\n" + doc.Content)
		if err != nil {
			t.Fatalf("Embed %q: %v", doc.Title, err)
		}
		if err := db.Insert(d, doc, vec); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	handler := makeSearchHandler(d, testEmbedder, false)
	_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchDocsInput{
		Query:   "install",
		LibID:   "/hashicorp/terraform",
		Version: "v1.14",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(out.Snippets) != 1 {
		t.Fatalf("got %d snippets, want 1", len(out.Snippets))
	}
	if out.Snippets[0].Version != "v1.14" {
		t.Errorf("snippet Version = %q, want v1.14", out.Snippets[0].Version)
	}
}

// TestHandleSearchLibraries exercises the search_libraries handler end
// to end against a hand-seeded libs catalog. The corpus is intentionally
// small but heterogeneous (different doc_counts, different topics) so
// every subtest can pin a single observable behavior without depending
// on the others. The handler is the only public surface area for
// search_libraries; if it works here, the wiring is correct.
func TestHandleSearchLibraries(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"), db.Meta{
		EmbedderKind: testEmbedder.Kind(),
		EmbeddingDim: testEmbedder.Dim(),
		ModelVersion: testEmbedder.ModelVersion(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	libs := []struct {
		id    string
		count int
	}{
		{"/hashicorp/terraform-provider-aws", 50},
		{"/facebook/react", 200},
		{"/expressjs/express", 75},
	}
	for _, l := range libs {
		if err := db.UpsertLibIfNew(d, l.id, "", testEmbedder); err != nil {
			t.Fatalf("UpsertLibIfNew %q: %v", l.id, err)
		}
		if err := db.UpdateLibCount(d, l.id, "", l.count); err != nil {
			t.Fatalf("UpdateLibCount %q: %v", l.id, err)
		}
	}

	handler := makeSearchLibrariesHandler(d, testEmbedder, false)

	t.Run("ranks semantic match first", func(t *testing.T) {
		_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchLibrariesInput{
			Name: "terraform aws",
		})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if len(out.Libraries) == 0 {
			t.Fatal("expected libraries, got none")
		}
		if out.Libraries[0].LibID != "/hashicorp/terraform-provider-aws" {
			t.Errorf("expected /hashicorp/terraform-provider-aws first, got %q", out.Libraries[0].LibID)
			for i, lib := range out.Libraries {
				t.Logf("  #%d: %s (score=%v)", i+1, lib.LibID, lib.MatchScore)
			}
		}
		// match_score is computed as 1 - distance, so it must lie in
		// the documented [-1, 1] range and the top result should be
		// strictly greater than the worst result.
		if out.Libraries[0].MatchScore < out.Libraries[len(out.Libraries)-1].MatchScore {
			t.Errorf("top match_score %v < bottom %v (rank/score inverted)",
				out.Libraries[0].MatchScore, out.Libraries[len(out.Libraries)-1].MatchScore)
		}
	})

	t.Run("empty name returns top by doc_count", func(t *testing.T) {
		_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchLibrariesInput{
			Name: "",
		})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if len(out.Libraries) != 3 {
			t.Fatalf("got %d libraries, want 3", len(out.Libraries))
		}
		// /facebook/react has the highest doc_count, so it should
		// lead the list even though no semantic match was performed.
		if out.Libraries[0].LibID != "/facebook/react" {
			t.Errorf("empty-name path: expected /facebook/react first, got %q", out.Libraries[0].LibID)
		}
		if out.Libraries[0].MatchScore != 1.0 {
			t.Errorf("empty-name path: expected match_score=1.0, got %v", out.Libraries[0].MatchScore)
		}
	})

	t.Run("whitespace-only name uses doc_count path", func(t *testing.T) {
		// "  " is logically empty for an LLM-driven query; the handler
		// should treat it as such and avoid embedding whitespace.
		_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchLibrariesInput{
			Name: "   ",
		})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if out.Libraries[0].LibID != "/facebook/react" {
			t.Errorf("expected doc_count ordering, got top=%q", out.Libraries[0].LibID)
		}
	})

	t.Run("limit is respected", func(t *testing.T) {
		_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchLibrariesInput{
			Name:  "terraform aws",
			Limit: 2,
		})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if len(out.Libraries) != 2 {
			t.Errorf("limit=2 returned %d libraries, want 2", len(out.Libraries))
		}
	})

	t.Run("limit cap is enforced", func(t *testing.T) {
		// Asking for 9999 must clamp to maxLibLimit (50). With only
		// 3 libs in the catalog the cap is invisible at the result
		// level, so this assertion is on "no error, normal output"
		// rather than length.
		_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchLibrariesInput{
			Name:  "terraform aws",
			Limit: 9999,
		})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if len(out.Libraries) > maxLibLimit {
			t.Errorf("got %d libraries, want <= %d", len(out.Libraries), maxLibLimit)
		}
	})
}

// TestHandleSearchLibraries_NoMatches pins the "no matches → empty
// non-nil slice" acceptance criterion. Empty libs catalog (no rows
// inserted) is the easiest way to force the path; the handler must
// return [] not null on the wire so MCP clients can iterate without a
// nil-guard.
func TestHandleSearchLibraries_NoMatches(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "empty.db"), db.Meta{
		EmbedderKind: testEmbedder.Kind(),
		EmbeddingDim: testEmbedder.Dim(),
		ModelVersion: testEmbedder.ModelVersion(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	handler := makeSearchLibrariesHandler(d, testEmbedder, false)

	for _, name := range []string{"anything", ""} {
		t.Run("name="+name, func(t *testing.T) {
			_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchLibrariesInput{
				Name: name,
			})
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if out.Libraries == nil {
				t.Error("Libraries should be non-nil empty slice, got nil")
			}
			if len(out.Libraries) != 0 {
				t.Errorf("expected 0 libraries on empty catalog, got %d", len(out.Libraries))
			}
		})
	}
}
