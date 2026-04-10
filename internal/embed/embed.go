// Package embed produces fixed-dimension embedding vectors from text.
//
// This package defines the Embedder interface used by both the indexer
// (cmd/scraper) and the query path (cmd/server). The current implementation
// is a deterministic, hash-based Stub intended to prove the pipeline
// end-to-end before a real embedding model (tracked in issue #2) is wired in.
package embed

import (
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// Dim is the fixed embedding dimension. Must match the F32_BLOB(N) column
// width in internal/db. Chosen to match upstream tursogo's
// TestVectorOperations fixture size.
const Dim = 64

// Embedder turns text into a fixed-dimension embedding vector.
// Implementations must be deterministic: the same input must always produce
// the same output, so that indexed vectors and query vectors are comparable.
type Embedder interface {
	Embed(text string) []float32
}

// Stub is a deterministic bag-of-tokens embedder. Not semantically meaningful
// in any deep sense, but sufficient to exercise the schema and retrieval path
// while a real model is under development.
//
// Tokens are lowercased, split on non-alphanumeric characters and on
// camelCase boundaries, and hashed into dimensions modulo Dim. A 4-character
// prefix of each token is also hashed with weight 0.5, which gives "tool"
// and "tools" a small amount of soft overlap. The resulting vector is
// L2-normalized so cosine distance is well-behaved.
type Stub struct{}

// NewStub returns a Stub embedder. Zero-cost — the type has no state.
func NewStub() Stub { return Stub{} }

// Embed computes a deterministic embedding for text. The returned slice has
// length Dim and is L2-normalized (unit-norm) for every input, including the
// empty string.
func (Stub) Embed(text string) []float32 {
	vec := make([]float32, Dim)
	for _, tok := range tokenize(text) {
		vec[hashToDim(tok)]++
		if len(tok) > 4 {
			vec[hashToDim(tok[:4])] += 0.5
		}
	}
	return normalize(vec)
}

// tokenize lowercases text and splits on non-alphanumeric runes AND on
// camelCase boundaries, so "mcp.AddTool" yields ["mcp", "add", "tool"].
// This camelCase handling is what lets a natural-language query like
// "register a tool" share the "tool" token with an identifier like
// "AddTool" in the indexed docs.
func tokenize(text string) []string {
	var (
		out []string
		cur strings.Builder
	)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	var prev rune
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && unicode.IsLower(prev) {
				flush()
			}
			cur.WriteRune(r)
		default:
			flush()
		}
		prev = r
	}
	flush()
	return out
}

func hashToDim(s string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return int(h.Sum32() % Dim)
}

func normalize(v []float32) []float32 {
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	if sumSq == 0 {
		// Empty or whitespace-only input. Return a fixed unit vector in the
		// first dimension so vector_distance_cos never receives a zero
		// vector and never returns NaN.
		v[0] = 1
		return v
	}
	inv := float32(1 / math.Sqrt(sumSq))
	for i := range v {
		v[i] *= inv
	}
	return v
}
