// Package embed produces fixed-dimension embedding vectors from text.
//
// This package defines the Embedder interface used by both the indexer
// (cmd/scraper) and the query path (cmd/server). The current and only
// implementation is Hugot, a feature extraction pipeline running on
// hugot's ORT (onnxruntime) backend (see hugot.go).
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
// The interface is split into EmbedQuery and EmbedDocument because
// retrieval-trained models (nomic-embed-text, BGE, e5, …) require
// task-specific prefixes and degrade silently without them. The call site
// knows whether a given text is a query or a document; the embedder picks
// the right prefix accordingly. Implementations that do not need a prefix
// can forward both methods to a single internal path.
//
// Kind, Dim, and ModelVersion are written into the database's meta table on
// first use and cross-checked on every subsequent open, so that a binary
// running with embedder X cannot accidentally read or write a database that
// was indexed with embedder Y.
type Embedder interface {
	// EmbedQuery returns a vector of length Dim() for a retrieval query —
	// text that will be compared against a corpus. Implementations must
	// surface inference errors instead of returning a placeholder vector:
	// a silently corrupted embedding pollutes the cosine index permanently
	// and is impossible to detect post-hoc.
	EmbedQuery(text string) ([]float32, error)

	// EmbedDocument returns a vector of length Dim() for a corpus
	// document — text that will live in the index (a scraped snippet, a
	// lib_id row, …). Same error-propagation contract as EmbedQuery.
	EmbedDocument(text string) ([]float32, error)

	// Kind identifies the embedder family (e.g. "hugot").
	// Used for meta consistency checks between scraper and server runs.
	Kind() string

	// Dim is the output vector dimension. Must be constant for the
	// lifetime of the Embedder.
	Dim() int

	// ModelVersion identifies the specific model producing the
	// embeddings (e.g. "nomic-ai/nomic-embed-text-v1.5"). Stored in the
	// DB meta table and cross-checked at open time.
	ModelVersion() string

	// Close releases any resources held by the embedder (model session,
	// tokenizer, etc.). Safe to call once at process shutdown via defer.
	Close() error
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
		return NewHugot(DefaultHugotModel, DefaultCacheDir())
	default:
		return nil, fmt.Errorf("unknown embedder kind %q (valid: %s)", kind, KindHugot)
	}
}

// Signature returns the deterministic vector-space signature for the
// given embedder kind WITHOUT instantiating it (no model load, no
// network). Mirrors New(kind) — same valid-kinds set, same error
// shape on unknown kinds. Used by `deadzone cache-signals` to feed
// the CI per-lib artifact cache key, so a constant edit in hugot.go
// (model swap, quantization variant change, prefix tweak) invalidates
// caches in lockstep without forcing a Hugot init at workflow time.
func Signature(kind string) (string, error) {
	switch kind {
	case KindHugot:
		return HugotSignature(), nil
	default:
		return "", fmt.Errorf("unknown embedder kind %q (valid: %s)", kind, KindHugot)
	}
}
