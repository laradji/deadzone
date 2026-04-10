package main

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// acceptanceCorpus is a small hand-crafted set of documentation snippets
// mimicking the go-sdk's surface area. One snippet is the tool-registration
// target; the rest are thematically-adjacent distractors (server lifecycle,
// resources, prompts, sampling) that a decent semantic retriever should
// still rank below tool registration for "expose-functions"-style queries.
//
// Keeping the corpus hand-crafted (not scraped) makes this test fully
// deterministic and immune to upstream go-sdk doc churn.
var acceptanceCorpus = []db.Doc{
	{
		LibID:   "/modelcontextprotocol/go-sdk",
		Title:   "Tool registration",
		Content: "Register tools on an MCP server with mcp.AddTool(server, &mcp.Tool{Name: \"myTool\", Description: \"...\"}, handlerFunc). The handler is invoked each time a client asks the server to run that tool.",
	},
	{
		LibID:   "/modelcontextprotocol/go-sdk",
		Title:   "Server setup",
		Content: "Create a new MCP server instance with mcp.NewServer and start it over stdio by calling server.Run(ctx, &mcp.StdioTransport{}). The transport handles JSON-RPC framing.",
	},
	{
		LibID:   "/modelcontextprotocol/go-sdk",
		Title:   "Resources",
		Content: "Resources model read-only data such as files or database rows. Advertise a resource with mcp.AddResource so clients can list it and fetch it by URI.",
	},
	{
		LibID:   "/modelcontextprotocol/go-sdk",
		Title:   "Prompts",
		Content: "Prompts are parameterized message templates the client can render. Call mcp.AddPrompt to publish a template alongside its argument schema.",
	},
	{
		LibID:   "/modelcontextprotocol/go-sdk",
		Title:   "Sampling",
		Content: "Sampling lets the server request a completion from the client's model. Use Session.CreateMessage from inside a handler to ask the client to generate text.",
	},
}

// acceptanceTargetTitle is the snippet each reformulation query is expected
// to rank first.
const acceptanceTargetTitle = "Tool registration"

// acceptanceQueries are the three natural-language reformulations from
// issue #20. None of them share literal tokens with "register" / "tool" /
// "AddTool", so a bag-of-words retriever would necessarily fail on all
// three. Passing this test is a positive signal that hugot + MiniLM is
// projecting queries into a useful semantic space rather than relying on
// surface-level token overlap.
var acceptanceQueries = []string{
	"how do I expose functions to the LLM",
	"let Claude call my function",
	"plug custom code into the server",
}

// TestAcceptance_SemanticRanking exercises the full search handler path
// (embedder → vector store → top-k ranking) against the hand-crafted
// corpus and asserts each reformulation query picks the tool-registration
// snippet as its top-1 result.
//
// Skipped under -short so CI can opt out on every PR (where the heavy
// model + warm-up cost isn't worth paying) and run the acceptance suite
// only on main / release pipelines. See README "Quick start" for the
// model cache layout that makes this affordable in CI.
func TestAcceptance_SemanticRanking(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping semantic acceptance test in -short mode")
	}

	d := newAcceptanceDB(t)
	handler := makeSearchHandler(d, testEmbedder)

	for _, query := range acceptanceQueries {
		query := query
		t.Run(query, func(t *testing.T) {
			_, out, err := handler(
				context.Background(),
				&mcp.CallToolRequest{},
				SearchDocsInput{Query: query},
			)
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if len(out.Snippets) == 0 {
				t.Fatalf("query %q returned zero snippets", query)
			}
			if out.Snippets[0].Title != acceptanceTargetTitle {
				t.Errorf("query %q: expected %q ranked first, got %q",
					query, acceptanceTargetTitle, out.Snippets[0].Title)
				for i, s := range out.Snippets {
					t.Logf("  #%d: [%s] %s", i+1, s.LibID, s.Title)
				}
			}
		})
	}
}

// Latency budgets from issue #20. Cold-start covers the very first Embed
// call after NewHugot (GoMLX session warm-up + first JIT compile);
// warm-path covers every subsequent call against the already-warmed
// pipeline, which is the number that matters for MCP query responsiveness.
const (
	coldStartBudget = 500 * time.Millisecond
	warmPathBudget  = 100 * time.Millisecond
	warmPathRuns    = 10
)

// TestAcceptance_EmbedLatency measures the two latencies that matter for
// the MCP request path and fails loudly with both actual timings and
// budgets logged when either is exceeded, so the next run has full
// debugging context.
//
// Uses a *fresh* Hugot (not the package-wide testEmbedder) so the
// cold-path measurement actually captures the first-call warmup cost,
// rather than whatever state the package-wide instance has accumulated
// from prior tests. The model is reused from DEADZONE_HUGOT_CACHE so only
// session / pipeline construction is on the critical path — the ~90 MB
// model download is not.
func TestAcceptance_EmbedLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping latency acceptance test in -short mode")
	}

	e, err := embed.NewHugot(embed.DefaultHugotModel, hugotTestCacheDir())
	if err != nil {
		t.Fatalf("NewHugot: %v", err)
	}
	defer e.Close()

	// Cold path: first Embed after NewHugot.
	coldStartTime := time.Now()
	_ = e.Embed("cold start latency probe")
	cold := time.Since(coldStartTime)
	t.Logf("cold path:        %v (budget %v)", cold, coldStartBudget)
	if cold > coldStartBudget {
		t.Errorf("cold-start latency %v exceeded budget %v", cold, coldStartBudget)
	}

	// Warm path: median of N runs against the already-warmed pipeline.
	// Median rather than mean because GoMLX inference is GC-sensitive and
	// a single long pause shouldn't sink an otherwise healthy run.
	warm := make([]time.Duration, warmPathRuns)
	for i := range warm {
		t0 := time.Now()
		_ = e.Embed("warm path latency probe")
		warm[i] = time.Since(t0)
	}
	sort.Slice(warm, func(i, j int) bool { return warm[i] < warm[j] })
	median := warm[len(warm)/2]

	t.Logf("warm path runs:   %v", warm)
	t.Logf("warm path median: %v (budget %v)", median, warmPathBudget)
	if median > warmPathBudget {
		t.Errorf("warm-path median latency %v exceeded budget %v", median, warmPathBudget)
	}
}

// newAcceptanceDB builds a fresh turso database, inserts the acceptance
// corpus into it, and returns it for the test to query. Each test gets its
// own file under t.TempDir so parallel runs don't step on each other.
func newAcceptanceDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "acceptance.db")
	d, err := db.Open(path, db.Meta{
		EmbedderKind: testEmbedder.Kind(),
		EmbeddingDim: testEmbedder.Dim(),
		ModelVersion: testEmbedder.ModelVersion(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	for _, doc := range acceptanceCorpus {
		vec := testEmbedder.Embed(doc.Title + "\n" + doc.Content)
		if err := db.Insert(d, doc, vec); err != nil {
			t.Fatalf("Insert %q: %v", doc.Title, err)
		}
	}
	return d
}
