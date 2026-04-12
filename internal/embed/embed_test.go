package embed_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/laradji/deadzone/internal/embed"
)

// testEmbedder is a single Hugot instance shared by every test in the
// package. NewHugot is expensive (downloads + loads the model + warms up
// the ORT session) so amortizing it across the whole package via TestMain
// brings per-test overhead down to roughly the cost of one pipeline run.
var testEmbedder *embed.Hugot

func TestMain(m *testing.M) {
	// Use the production cache dir resolver so the model is reused
	// across runs (and across packages, and across the production
	// embed.New() path used by TestNew below) instead of being
	// re-downloaded into a fresh temp dir on every `go test` invocation.
	// embed.DefaultCacheDir honors DEADZONE_HUGOT_CACHE for CI.
	e, err := embed.NewHugot(embed.DefaultHugotModel, embed.DefaultCacheDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: NewHugot: %v\n", err)
		os.Exit(1)
	}
	testEmbedder = e
	code := m.Run()
	_ = e.Close()
	os.Exit(code)
}

// cosineDistance mirrors what vector_distance_cos computes on the DB side:
// 1 - (a·b) / (||a|| * ||b||). Lower values mean closer vectors.
func cosineDistance(a, b []float32) float32 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return float32(1 - dot/(math.Sqrt(na)*math.Sqrt(nb)))
}

func floatsToBytes(v []float32) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, v)
	return buf.Bytes()
}

func TestHugot_Deterministic(t *testing.T) {
	inputs := []string{
		"hello world",
		"Register tools using mcp.AddTool",
		"Créer un serveur MCP",
	}
	for _, in := range inputs {
		a, err := testEmbedder.EmbedQuery(in)
		if err != nil {
			t.Fatalf("EmbedQuery(%q): %v", in, err)
		}
		b, err := testEmbedder.EmbedQuery(in)
		if err != nil {
			t.Fatalf("EmbedQuery(%q): %v", in, err)
		}
		if !bytes.Equal(floatsToBytes(a), floatsToBytes(b)) {
			t.Errorf("EmbedQuery(%q) not deterministic across calls", in)
		}
	}
}

func TestHugot_Dim(t *testing.T) {
	cases := []string{"x", "hello world", "Register tools using mcp.AddTool"}
	for _, c := range cases {
		v, err := testEmbedder.EmbedDocument(c)
		if err != nil {
			t.Fatalf("EmbedDocument(%q): %v", c, err)
		}
		if len(v) != testEmbedder.Dim() {
			t.Errorf("EmbedDocument(%q) len = %d, want %d", c, len(v), testEmbedder.Dim())
		}
	}
}

func TestHugot_Metadata(t *testing.T) {
	if got := testEmbedder.Kind(); got != "hugot" {
		t.Errorf("Kind() = %q, want %q", got, "hugot")
	}
	// 768 is the nomic hidden size. Hard-coding it catches a silently
	// swapped model file (e.g. someone pointing OnnxFilename at the
	// unquantized variant or a different checkpoint with the same repo).
	if got := testEmbedder.Dim(); got != 768 {
		t.Errorf("Dim() = %d, want 768", got)
	}
	if got := testEmbedder.ModelVersion(); got != embed.DefaultHugotModel {
		t.Errorf("ModelVersion() = %q, want %q", got, embed.DefaultHugotModel)
	}
}

func TestHugot_UnitNorm(t *testing.T) {
	cases := []string{"a", "hello world", "Register tools using mcp.AddTool"}
	for _, c := range cases {
		v, err := testEmbedder.EmbedDocument(c)
		if err != nil {
			t.Fatalf("EmbedDocument(%q): %v", c, err)
		}
		var sumSq float64
		for _, x := range v {
			sumSq += float64(x) * float64(x)
		}
		// nomic through hugot's WithNormalization() option produces
		// L2-normalized embeddings; allow a slightly looser epsilon
		// than the stub since we're rounding through int8 quantized
		// ONNX inference rather than computing the norm in pure Go.
		if math.Abs(sumSq-1) > 1e-4 {
			t.Errorf("EmbedDocument(%q) ||v||^2 = %v, want ~1", c, sumSq)
		}
	}
}

// TestHugot_SemanticOverlap is the real-embedder version of the stub's
// token-overlap probe: a natural-language query should be semantically
// closer to the relevant identifier-heavy doc than to an unrelated one.
// With a 768-dim nomic-embed-text-v1.5 model this is the actual semantic
// retrieval property we care about, not a hash-collision artifact.
//
// The query uses EmbedQuery and the corpus uses EmbedDocument; skipping
// the split (embedding both sides as queries, or both as documents)
// compresses the usable dynamic range of cosine scores noticeably, so
// the assertion implicitly also guards against the caller losing the
// query/document distinction.
func TestHugot_SemanticOverlap(t *testing.T) {
	query, err := testEmbedder.EmbedQuery("register a tool")
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	relevant, err := testEmbedder.EmbedDocument("Register tools using mcp.AddTool")
	if err != nil {
		t.Fatalf("EmbedDocument relevant: %v", err)
	}
	unrelated, err := testEmbedder.EmbedDocument("Open a database with sql.Open")
	if err != nil {
		t.Fatalf("EmbedDocument unrelated: %v", err)
	}

	distRelevant := cosineDistance(query, relevant)
	distUnrelated := cosineDistance(query, unrelated)

	if distRelevant >= distUnrelated {
		t.Errorf("expected query to be closer to relevant doc: dist(query, relevant)=%v vs dist(query, unrelated)=%v",
			distRelevant, distUnrelated)
	}
}

// TestHugot_LongInputNoPanic pins the core motivation for this swap:
// all-MiniLM-L6-v2 panicked on inputs above its 512-token limit
// (issue #62), which took down the scraper mid-run. nomic-embed-text-v1.5
// advertises an 8192-token context, so a ~2000-token block of text
// should pass through the pipeline without erroring — and definitely
// without panicking.
func TestHugot_LongInputNoPanic(t *testing.T) {
	// ~2000 English-ish tokens worth of text. A paragraph repeated
	// enough times to comfortably exceed the old 512-token ceiling
	// while staying well inside the new 8192 limit.
	para := "The feature extraction pipeline tokenizes the input, runs it through the transformer, mean-pools the hidden states across the sequence dimension, and finally L2-normalizes the result. "
	long := strings.Repeat(para, 80)
	v, err := testEmbedder.EmbedDocument(long)
	if err != nil {
		t.Fatalf("EmbedDocument(long): %v", err)
	}
	if len(v) != testEmbedder.Dim() {
		t.Errorf("EmbedDocument(long) len = %d, want %d", len(v), testEmbedder.Dim())
	}
}

func TestNew(t *testing.T) {
	// The happy-path "hugot" subtest is intentionally absent. hugot's ORT
	// backend enforces a single active onnxruntime session per process
	// (see knights-analytics/hugot hugot_ort.go), and the package-level
	// testEmbedder is already holding one — spinning up a second via
	// embed.New here would fail with "another session is currently
	// active". The dispatch through New is exercised every time TestMain
	// runs, since embed.New(KindHugot) calls NewHugot(DefaultHugotModel,
	// DefaultCacheDir()) with the same arguments TestMain uses directly.

	t.Run("unknown kind", func(t *testing.T) {
		if _, err := embed.New("does-not-exist"); err == nil {
			t.Fatal("expected error for unknown kind, got nil")
		}
	})

	t.Run("stub kind no longer accepted", func(t *testing.T) {
		// Phase 2 retired the stub. A user with stale config that
		// still passes "stub" should get a clear error rather than
		// a silent fallback.
		if _, err := embed.New("stub"); err == nil {
			t.Fatal("expected error for retired 'stub' kind, got nil")
		}
	})
}
