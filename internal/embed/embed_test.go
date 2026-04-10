package embed_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/laradji/deadzone/internal/embed"
)

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

func TestStub_Deterministic(t *testing.T) {
	e := embed.NewStub()
	inputs := []string{
		"",
		"hello world",
		"Register tools using mcp.AddTool",
		"Créer un serveur MCP",
	}
	for _, in := range inputs {
		a := e.Embed(in)
		b := e.Embed(in)
		if !bytes.Equal(floatsToBytes(a), floatsToBytes(b)) {
			t.Errorf("Embed(%q) not deterministic across calls", in)
		}
	}
}

func TestStub_Dim(t *testing.T) {
	e := embed.NewStub()
	cases := []string{"", "x", "hello world", "Register tools using mcp.AddTool"}
	for _, c := range cases {
		v := e.Embed(c)
		if len(v) != embed.Dim {
			t.Errorf("Embed(%q) len = %d, want %d", c, len(v), embed.Dim)
		}
	}
}

func TestStub_UnitNorm(t *testing.T) {
	e := embed.NewStub()
	cases := []string{"", "a", "hello world", "Register tools using mcp.AddTool", "!!!"}
	for _, c := range cases {
		v := e.Embed(c)
		var sumSq float64
		for _, x := range v {
			sumSq += float64(x) * float64(x)
		}
		if math.Abs(sumSq-1) > 1e-6 {
			t.Errorf("Embed(%q) ||v||^2 = %v, want ~1", c, sumSq)
		}
	}
}

func TestStub_EmptyString(t *testing.T) {
	e := embed.NewStub()
	v := e.Embed("")
	if v == nil {
		t.Fatal("Embed(\"\") returned nil")
	}
	if len(v) != embed.Dim {
		t.Fatalf("Embed(\"\") len = %d, want %d", len(v), embed.Dim)
	}
	// All components must be finite (no NaN / Inf).
	for i, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			t.Errorf("Embed(\"\")[%d] = %v, want finite", i, x)
		}
	}
	// Must be unit-norm so cosine distance is well-defined.
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	if math.Abs(sumSq-1) > 1e-6 {
		t.Errorf("Embed(\"\") ||v||^2 = %v, want ~1", sumSq)
	}
}

func TestStub_TokenOverlap(t *testing.T) {
	e := embed.NewStub()

	query := e.Embed("register a tool")
	relevant := e.Embed("Register tools using mcp.AddTool")
	unrelated := e.Embed("Open a database with sql.Open")

	distRelevant := cosineDistance(query, relevant)
	distUnrelated := cosineDistance(query, unrelated)

	if distRelevant >= distUnrelated {
		t.Errorf("expected query to be closer to relevant doc: dist(query, relevant)=%v vs dist(query, unrelated)=%v",
			distRelevant, distUnrelated)
	}
}

func TestStub_CamelCaseSplit(t *testing.T) {
	e := embed.NewStub()

	camel := e.Embed("AddTool")
	spaced := e.Embed("add tool")
	unrelated := e.Embed("database connection")

	if cosineDistance(camel, spaced) >= cosineDistance(camel, unrelated) {
		t.Errorf("camelCase split failed: 'AddTool' should be closer to 'add tool' than to 'database connection'")
	}
}
