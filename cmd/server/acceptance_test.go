package main

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/laradji/deadzone/internal/db"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// acceptanceCorpus is the hand-crafted document set used by the semantic
// acceptance test. The first entry is the only tool-registration snippet
// in the corpus; the rest are deliberate distractors that touch the same
// general topic (server, MCP, code) without being about exposing
// functions to the LLM. The corpus is hand-crafted (not scraped) so the
// test stays self-contained, deterministic, and is not affected by
// upstream go-sdk doc changes.
//
// None of the queries in semanticAcceptanceQueries share literal tokens
// with the target's title or content (no "register", "tool", "AddTool",
// "function"), so a bag-of-words ranker would not place the target first
// on any of them. Only an embedder that projects the queries into a
// semantic space close to the target can satisfy the assertion.
var acceptanceCorpus = []db.Doc{
	{
		LibID:   "/modelcontextprotocol/go-sdk",
		Title:   "Tool registration",
		Content: "Use mcp.AddTool to register a tool on the MCP server. This is the canonical way to make your Go code callable from a client — each tool wraps a typed Go handler, and the model invokes it whenever it decides the tool is useful for the conversation.",
	},
	{
		LibID:   "/modelcontextprotocol/go-sdk",
		Title:   "Resource listing",
		Content: "Expose static or dynamic content via mcp.AddResource. Resources are addressed by URI and read by clients in response to user actions; they cannot run code.",
	},
	{
		LibID:   "/modelcontextprotocol/go-sdk",
		Title:   "Prompt templates",
		Content: "Declare reusable prompt fragments with mcp.AddPrompt. Prompts let users invoke a parameterized text template that the client renders into a chat message.",
	},
	{
		LibID:   "/other/db",
		Title:   "Opening a SQLite file",
		Content: "Open a SQLite database with sql.Open. The driver is registered under the name sqlite3 and accepts a path to an on-disk file.",
	},
	{
		LibID:   "/other/http",
		Title:   "Static file serving",
		Content: "Serve a directory of static assets with http.FileServer. The handler maps URL paths to filesystem entries under the given root.",
	},
}

// acceptanceTarget is the title of the only doc in acceptanceCorpus that
// describes how to register an MCP tool. Every query in
// semanticAcceptanceQueries must rank this snippet first.
const acceptanceTarget = "Tool registration"

// semanticAcceptanceQueries are three natural-language reformulations of
// "register an MCP tool" that deliberately share no literal tokens with
// the target snippet. They are the headline experiment from issue #20:
// if hugot+MiniLM is doing real semantic projection, it should put the
// target first for every one of them.
var semanticAcceptanceQueries = []string{
	"how do I expose functions to the LLM",
	"let Claude call my function",
	"plug custom code into the server",
}

// TestSemanticAcceptance is Phase 3's headline test: the hugot+nomic
// embedder, exercised through the full MCP search handler, must rank the
// tool-registration snippet first for every query in
// semanticAcceptanceQueries. Skipped under -short so CI can opt out
// without paying the model download + inference cost on every PR.
func TestSemanticAcceptance(t *testing.T) {
	if testing.Short() {
		t.Skip("acceptance test skipped under -short (model download + inference cost)")
	}

	d, err := db.Open(filepath.Join(t.TempDir(), "acceptance.db"), db.Meta{
		EmbedderKind: testEmbedder.Kind(),
		EmbeddingDim: testEmbedder.Dim(),
		ModelVersion: testEmbedder.ModelVersion(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	for _, doc := range acceptanceCorpus {
		vec, err := testEmbedder.EmbedDocument(doc.Title + "\n" + doc.Content)
		if err != nil {
			t.Fatalf("Embed %q: %v", doc.Title, err)
		}
		if err := db.Insert(d, doc, vec); err != nil {
			t.Fatalf("Insert %q: %v", doc.Title, err)
		}
	}

	handler := makeSearchHandler(d, testEmbedder, false)

	for _, query := range semanticAcceptanceQueries {
		t.Run(query, func(t *testing.T) {
			_, out, err := handler(context.Background(), &mcp.CallToolRequest{}, SearchDocsInput{
				Query: query,
			})
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if len(out.Snippets) == 0 {
				t.Fatal("expected snippets, got none")
			}
			if out.Snippets[0].Title != acceptanceTarget {
				t.Errorf("expected %q ranked first for query %q, got %q",
					acceptanceTarget, query, out.Snippets[0].Title)
				for i, s := range out.Snippets {
					t.Logf("  #%d: [%s] %s", i+1, s.LibID, s.Title)
				}
			}
		})
	}
}

// warmEmbedBudget bounds steady-state EmbedQuery calls. This is the number
// that ultimately bounds MCP query responsiveness, since every search_docs
// call invokes the embedder once on the user's query. The spike in #67
// measured ~3.8 ms for nomic-embed-text-v1.5 int8 through ORT, so 100 ms
// leaves generous headroom for slower CI hardware.
const (
	warmEmbedBudget = 100 * time.Millisecond

	// warmRuns is the sample size for warmEmbedBudget. Median is used
	// instead of mean so a single GC pause or scheduler hiccup does not
	// fail an otherwise healthy run.
	warmRuns = 10
)

// TestEmbedLatencyBudget enforces the warm-path latency budget from issue
// #20. Skipped under -short alongside the semantic acceptance test so the
// same CI flag toggles both.
//
// The cold-path subtest was removed when the embedder moved from hugot's
// GoMLX backend to the ORT one: ORT enforces a single active onnxruntime
// session per process, so creating a second NewHugot while the
// package-level testEmbedder is alive fails with "another session is
// currently active". A cold measurement would need a separate process,
// which belongs in a standalone benchmark rather than the test suite.
func TestEmbedLatencyBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("latency budget test skipped under -short")
	}

	// Prime the shared embedder so the first sample is warm too —
	// avoids contaminating the median with a one-off cache miss
	// from whatever ran before this test.
	if _, err := testEmbedder.EmbedQuery("warmup"); err != nil {
		t.Fatalf("warmup EmbedQuery: %v", err)
	}

	samples := make([]time.Duration, warmRuns)
	for i := range samples {
		start := time.Now()
		_, err := testEmbedder.EmbedQuery("how do I expose functions to the LLM")
		samples[i] = time.Since(start)
		if err != nil {
			t.Fatalf("warm sample %d EmbedQuery: %v", i, err)
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	median := samples[len(samples)/2]

	if median > warmEmbedBudget {
		t.Errorf("warm EmbedQuery median %v exceeds budget %v; samples=%v",
			median, warmEmbedBudget, samples)
	}
	t.Logf("warm EmbedQuery median: %v (budget %v, samples=%v)",
		median, warmEmbedBudget, samples)
}
