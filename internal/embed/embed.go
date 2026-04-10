// Package embed produces fixed-dimension embedding vectors from text.
//
// This package defines the Embedder interface used by both the indexer
// (cmd/scraper) and the query path (cmd/server). The current and only
// implementation is Hugot, a sentence-transformers feature extraction
// pipeline running on hugot's pure-Go GoMLX backend (see hugot.go).
//
// The interface keeps room for future embedders behind the same factory:
// adding a second model family means adding a Kind constant and a New case,
// not changing every call site.
package embed

import "fmt"

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

	// Kind identifies the embedder family (e.g. "hugot").
	// Used for meta consistency checks between scraper and server runs.
	Kind() string

	// Dim is the output vector dimension. Must be constant for the
	// lifetime of the Embedder.
	Dim() int

	// ModelVersion identifies the specific model producing the
	// embeddings (e.g. "sentence-transformers/all-MiniLM-L6-v2"). Stored
	// in the DB meta table and cross-checked at open time.
	ModelVersion() string
}

// New returns an Embedder for the given kind. Currently only KindHugot is
// supported; unknown kinds return an error so cmd/scraper and cmd/server can
// fail cleanly with a useful message.
//
// The Hugot embedder uses DefaultHugotModel and the platform default cache
// directory. Callers needing a non-default model or cache location should
// call NewHugot directly.
func New(kind string) (Embedder, error) {
	switch kind {
	case KindHugot:
		return NewHugot(DefaultHugotModel, defaultCacheDir())
	default:
		return nil, fmt.Errorf("unknown embedder kind %q (valid: %s)", kind, KindHugot)
	}
}
