package embed_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/laradji/deadzone/internal/embed"
)

// testEmbedder is a single Hugot instance shared by every test in the
// package. NewHugot is expensive (downloads + loads the model + warms up
// the GoMLX session) so amortizing it across the whole package via
// TestMain brings per-test overhead down to roughly the cost of one
// pipeline run.
var testEmbedder *embed.Hugot

func TestMain(m *testing.M) {
	// Use the production cache dir so the model is reused across runs
	// (and across packages) instead of being re-downloaded into a fresh
	// temp dir on every `go test` invocation.
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
// DEADZONE_HUGOT_CACHE so CI can pin the cache to a workspace-local path
// that gets restored from a Github Actions cache, falling back to the
// system default otherwise.
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
		a := testEmbedder.Embed(in)
		b := testEmbedder.Embed(in)
		if !bytes.Equal(floatsToBytes(a), floatsToBytes(b)) {
			t.Errorf("Embed(%q) not deterministic across calls", in)
		}
	}
}

func TestHugot_Dim(t *testing.T) {
	cases := []string{"x", "hello world", "Register tools using mcp.AddTool"}
	for _, c := range cases {
		v := testEmbedder.Embed(c)
		if len(v) != testEmbedder.Dim() {
			t.Errorf("Embed(%q) len = %d, want %d", c, len(v), testEmbedder.Dim())
		}
	}
}

func TestHugot_Metadata(t *testing.T) {
	if got := testEmbedder.Kind(); got != "hugot" {
		t.Errorf("Kind() = %q, want %q", got, "hugot")
	}
	if got := testEmbedder.Dim(); got <= 0 {
		t.Errorf("Dim() = %d, want > 0", got)
	}
	if got := testEmbedder.ModelVersion(); got == "" {
		t.Error("ModelVersion() returned empty string")
	}
	if got := testEmbedder.ModelVersion(); got != embed.DefaultHugotModel {
		t.Errorf("ModelVersion() = %q, want %q", got, embed.DefaultHugotModel)
	}
}

func TestHugot_UnitNorm(t *testing.T) {
	cases := []string{"a", "hello world", "Register tools using mcp.AddTool"}
	for _, c := range cases {
		v := testEmbedder.Embed(c)
		var sumSq float64
		for _, x := range v {
			sumSq += float64(x) * float64(x)
		}
		// MiniLM through hugot's WithNormalization() option produces
		// L2-normalized embeddings; allow a slightly looser epsilon
		// than the stub since we're rounding through float32 ONNX
		// inference rather than computing the norm in pure Go.
		if math.Abs(sumSq-1) > 1e-4 {
			t.Errorf("Embed(%q) ||v||^2 = %v, want ~1", c, sumSq)
		}
	}
}

// TestHugot_SemanticOverlap is the real-embedder version of the stub's
// token-overlap probe: a natural-language query should be semantically
// closer to the relevant identifier-heavy doc than to an unrelated one.
// With a 384-dim sentence-transformers model this is the actual semantic
// retrieval property we care about, not a hash-collision artifact.
func TestHugot_SemanticOverlap(t *testing.T) {
	query := testEmbedder.Embed("register a tool")
	relevant := testEmbedder.Embed("Register tools using mcp.AddTool")
	unrelated := testEmbedder.Embed("Open a database with sql.Open")

	distRelevant := cosineDistance(query, relevant)
	distUnrelated := cosineDistance(query, unrelated)

	if distRelevant >= distUnrelated {
		t.Errorf("expected query to be closer to relevant doc: dist(query, relevant)=%v vs dist(query, unrelated)=%v",
			distRelevant, distUnrelated)
	}
}

func TestNew(t *testing.T) {
	t.Run("hugot", func(t *testing.T) {
		e, err := embed.New(embed.KindHugot)
		if err != nil {
			t.Fatalf("New(hugot): %v", err)
		}
		// New returns a fresh Hugot — close it to release the
		// session it just allocated. The package-level testEmbedder
		// is unaffected.
		defer func() {
			if c, ok := e.(interface{ Close() error }); ok {
				_ = c.Close()
			}
		}()
		if e.Kind() != "hugot" {
			t.Errorf("Kind() = %q, want %q", e.Kind(), "hugot")
		}
		if e.Dim() <= 0 {
			t.Errorf("Dim() = %d, want > 0", e.Dim())
		}
	})

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
