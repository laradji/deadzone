// Package embed produces fixed-dimension embedding vectors from text.
//
// This package defines the Embedder interface used by both the indexer
// (cmd/scraper) and the query path (cmd/server). The current implementation
// is a deterministic, hash-based Stub intended to prove the pipeline
// end-to-end before a real embedding model (tracked in issue #2) is wired in.
package embed

import (
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// KindStub is the Kind() value reported by the Stub embedder. It is the
// only valid value for the -embedder flag in Phase 1.
const KindStub = "stub"

// stubDim is the Stub embedder's output dimension. Chosen to match upstream
// tursogo's TestVectorOperations fixture size. Not exported because callers
// must read it via Embedder.Dim() — there is no global "the dimension"
// anymore now that the binary is meant to support multiple embedders.
const stubDim = 64

// stubModelVersion identifies this revision of the stub. Bump it when the
// hashing or tokenization changes in a way that invalidates already-indexed
// vectors so existing DBs are rejected at Open time.
const stubModelVersion = "stub-v1"

// Embedder turns text into a fixed-dimension embedding vector.
// Implementations must be deterministic: the same input must always produce
// the same output, so that indexed vectors and query vectors are comparable.
//
// Kind, Dim, and ModelVersion are written into the database's meta table on
// first use and cross-checked on every subsequent open, so that a binary
// running with embedder X cannot accidentally read or write a database that
// was indexed with embedder Y.
type Embedder interface {
	// Embed returns a vector of length Dim() for the given text.
	Embed(text string) []float32

	// Kind identifies the embedder family (e.g. "stub", "real").
	// Used for meta consistency checks between scraper and server runs.
	Kind() string

	// Dim is the output vector dimension. Must be constant for the
	// lifetime of the Embedder.
	Dim() int

	// ModelVersion identifies the specific model producing the
	// embeddings (e.g. "stub-v1", "all-MiniLM-L6-v2"). Stored in the
	// DB meta table and cross-checked at open time.
	ModelVersion() string
}

// New returns an Embedder for the given kind. Phase 1 only knows "stub";
// unknown kinds return an error so cmd/scraper and cmd/server can fail
// cleanly with a useful message.
func New(kind string) (Embedder, error) {
	switch kind {
	case KindStub:
		return NewStub(), nil
	default:
		return nil, fmt.Errorf("unknown embedder kind %q (valid: %s)", kind, KindStub)
	}
}

// Stub is a deterministic bag-of-tokens embedder. Not semantically meaningful
// in any deep sense, but sufficient to exercise the schema and retrieval path
// while a real model is under development.
//
// Tokens are lowercased, split on non-alphanumeric characters and on
// camelCase boundaries, and hashed into dimensions modulo Dim(). A
// 4-character prefix of each token is also hashed with weight 0.5, which
// gives "tool" and "tools" a small amount of soft overlap. The resulting
// vector is L2-normalized so cosine distance is well-behaved.
type Stub struct{}

// NewStub returns a Stub embedder. Zero-cost — the type has no state.
func NewStub() Stub { return Stub{} }

// Kind reports the embedder family. Always "stub".
func (Stub) Kind() string { return KindStub }

// Dim reports the output dimension. Always stubDim.
func (Stub) Dim() int { return stubDim }

// ModelVersion identifies the stub revision. Bump stubModelVersion when the
// tokenization or hashing changes in a backwards-incompatible way.
func (Stub) ModelVersion() string { return stubModelVersion }

// Embed computes a deterministic embedding for text. The returned slice has
// length Dim() and is L2-normalized (unit-norm) for every input, including
// the empty string.
func (Stub) Embed(text string) []float32 {
	vec := make([]float32, stubDim)
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
	return int(h.Sum32() % stubDim)
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
