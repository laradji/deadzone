package embed

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/options"
	"github.com/knights-analytics/hugot/pipelines"

	"github.com/laradji/deadzone/internal/ort"
)

// KindHugot is the Kind() value reported by the Hugot embedder, and the only
// kind currently accepted by New.
const KindHugot = "hugot"

// DefaultHugotModel is nomic-embed-text-v1.5 — 768-dim, 8192-token context,
// Apache-2.0. Replaces the earlier all-MiniLM-L6-v2 checkpoint that panicked
// on inputs above 512 tokens and only produced 384-dim embeddings. Bumping
// this constant invalidates every existing database via db.Meta cross-check
// on next Open.
const DefaultHugotModel = "nomic-ai/nomic-embed-text-v1.5"

// onnxFilename is the specific ONNX variant we download and load. The
// unquantized model.onnx is ~549 MB and ships with an external-data
// sidecar; the int8 quantized variant is a self-contained ~131 MB file
// and is what the spike in #67 measured end-to-end.
const onnxFilename = "model_quantized.onnx"

// nomic was trained with task-specific prefixes. The tokenizer sees them
// as regular text, but the model only maps queries and documents into the
// same space when the prefix is present — skipping them silently degrades
// retrieval quality. The Embedder interface is split into EmbedQuery /
// EmbedDocument so the call site chooses up front which prefix gets
// prepended, rather than passing a mode flag at every call.
const (
	queryPrefix    = "search_query: "
	documentPrefix = "search_document: "
)

// Hugot wraps a hugot Session + FeatureExtractionPipeline running on the
// ORT (onnxruntime) backend. One Hugot is meant to live for the lifetime
// of a process: NewHugot is expensive (downloads + loads the model + spins
// up the ORT environment) but each EmbedQuery / EmbedDocument call is
// cheap.
//
// Concurrency: hugot's pipelines are not documented as goroutine-safe.
// internal/db serializes its single connection, and cmd/server handles one
// MCP request at a time, so a single shared *Hugot is fine for the current
// workload. If parallelism is added later, wrap the embed calls in a mutex
// or pool pipelines per worker.
type Hugot struct {
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
	model    string
	dim      int
}

// NewHugot constructs a Hugot embedder backed by modelName, downloading the
// model into cacheDir if it isn't already present. Pass an empty modelName
// to use DefaultHugotModel.
//
// First-run cost is dominated by the ONNX model download (~131 MB for the
// int8 nomic quantized variant) and the ORT session warm-up. Subsequent
// runs reuse the on-disk model.
//
// The ORT shared library is resolved via internal/ort.Bootstrap: the
// pinned release is downloaded + SHA256-verified + cached on first use
// and re-used on every subsequent run. Set DEADZONE_ORT_LIB_PATH to
// bypass the download and point at a hand-positioned library (air-gapped
// installs, pinned mirrors). Building with `-tags ORT` and CGO_ENABLED=1
// is required — without the tag, hugot.NewORTSession below compiles as a
// stub that returns a clear error.
func NewHugot(modelName, cacheDir string) (*Hugot, error) {
	if modelName == "" {
		modelName = DefaultHugotModel
	}
	if cacheDir == "" {
		return nil, errors.New("hugot: cacheDir must not be empty")
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("hugot: create cache dir %q: %w", cacheDir, err)
	}

	// hugot's downloader names the on-disk directory after the model, with
	// '/' replaced by '_'. We mirror that to detect a cached model and
	// avoid re-downloading on every start.
	modelDir := filepath.Join(cacheDir, strings.ReplaceAll(modelName, "/", "_"))
	if _, err := os.Stat(filepath.Join(modelDir, onnxFilename)); errors.Is(err, fs.ErrNotExist) {
		opts := hugot.NewDownloadOptions()
		// nomic's repo ships multiple ONNX variants under onnx/. Picking
		// the int8 quantized one explicitly avoids hugot's "ambiguous
		// .onnx file" validation error. The downloader copies the file
		// to modelDir/<basename> regardless of its source path.
		opts.OnnxFilePath = "onnx/" + onnxFilename
		if _, err := hugot.DownloadModel(context.Background(), modelName, cacheDir, opts); err != nil {
			return nil, fmt.Errorf("hugot: download %s: %w", modelName, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("hugot: stat model file: %w", err)
	}

	libDir, err := ort.Bootstrap("")
	if err != nil {
		return nil, fmt.Errorf("hugot: bootstrap onnxruntime: %w", err)
	}
	session, err := hugot.NewORTSession(context.Background(), options.WithOnnxLibraryPath(libDir))
	if err != nil {
		return nil, fmt.Errorf("hugot: new ORT session: %w", err)
	}

	cfg := hugot.FeatureExtractionConfig{
		ModelPath:    modelDir,
		Name:         "deadzone",
		OnnxFilename: onnxFilename,
		Options: []hugot.FeatureExtractionOption{
			// L2-normalize sentence embeddings so vector_distance_cos
			// behaves as a true cosine distance. nomic's training
			// objective expects normalized outputs on the consumer side.
			pipelines.WithNormalization(),
		},
	}
	pipe, err := hugot.NewPipeline(session, cfg)
	if err != nil {
		_ = session.Destroy()
		return nil, fmt.Errorf("hugot: new pipeline: %w", err)
	}

	// Hidden size is the last element of the output tensor shape; for text
	// encoders this is always the embedding dimension regardless of how
	// many leading dynamic axes the model declares.
	dims := pipe.Output.Dimensions
	if len(dims) == 0 || dims[len(dims)-1] <= 0 {
		_ = session.Destroy()
		return nil, fmt.Errorf("hugot: cannot determine embedding dimension from output shape %v", dims)
	}

	return &Hugot{
		session:  session,
		pipeline: pipe,
		model:    modelName,
		dim:      int(dims[len(dims)-1]),
	}, nil
}

// EmbedQuery prepends the "search_query: " prefix that nomic was trained
// with for retrieval queries and returns the resulting unit-norm vector.
// Use this for text that will be compared against a corpus (e.g. a user
// query handed to search_docs or search_libraries).
func (h *Hugot) EmbedQuery(text string) ([]float32, error) {
	return h.embed(queryPrefix + text)
}

// EmbedDocument prepends the "search_document: " prefix that nomic was
// trained with for indexed passages and returns the resulting unit-norm
// vector. Use this when embedding content that will live in the corpus
// (e.g. a scraped doc, or a lib_id written into the libs table).
func (h *Hugot) EmbedDocument(text string) ([]float32, error) {
	return h.embed(documentPrefix + text)
}

// embed runs text through the FeatureExtractionPipeline and returns the
// resulting unit-norm vector. Errors are propagated so callers can decide
// what to do — at index time the scraper logs and skips the doc, at query
// time the server returns the error to the MCP client. Returning a
// deterministic placeholder vector here used to silently pollute the
// cosine index, since every fallback collapsed to the same point in
// vector space and formed a synthetic attractor for any query aligned
// with that dimension.
func (h *Hugot) embed(text string) ([]float32, error) {
	out, err := h.pipeline.RunPipeline(context.Background(), []string{text})
	if err != nil {
		return nil, fmt.Errorf("hugot: run pipeline (text len=%d): %w", len(text), err)
	}
	if out == nil || len(out.Embeddings) == 0 {
		return nil, fmt.Errorf("hugot: pipeline returned no embeddings (text len=%d)", len(text))
	}
	return out.Embeddings[0], nil
}

// Kind reports the embedder family. Always KindHugot.
func (h *Hugot) Kind() string { return KindHugot }

// Dim is the output vector dimension, discovered from the loaded model at
// construction time. 768 for the default nomic checkpoint.
func (h *Hugot) Dim() int { return h.dim }

// ModelVersion returns the Hugging Face model name. Used by db.Meta to
// invalidate databases when the user switches models — bumping the model
// triggers ErrEmbedderMismatch on next Open against an old database.
func (h *Hugot) ModelVersion() string { return h.model }

// Close releases the underlying hugot Session and every pipeline owned by
// it. Safe to call once at process shutdown via defer.
func (h *Hugot) Close() error {
	if h.session == nil {
		return nil
	}
	err := h.session.Destroy()
	h.session = nil
	h.pipeline = nil
	return err
}

// DefaultCacheDir returns the cache directory used by NewHugot when the
// caller passes an empty cacheDir, and by embed.New for the same purpose.
//
// Resolution order:
//
//  1. $DEADZONE_HUGOT_CACHE if set (used by CI to pin the cache to a
//     workspace-local path that persists across runs, and by users who
//     want to share the model cache across processes or place it on a
//     specific disk).
//  2. os.UserCacheDir() + /deadzone/models — the platform default:
//     - Linux:   $XDG_CACHE_HOME/deadzone/models  (or ~/.cache/deadzone/models)
//     - macOS:   ~/Library/Caches/deadzone/models
//     - Windows: %LOCALAPPDATA%\deadzone\models
//  3. ./.deadzone-cache/models as a last-resort fallback when
//     UserCacheDir fails so the constructor can still proceed.
//
// Exported (capitalized) so tests in this and other packages can call
// the same resolution logic instead of duplicating the env-var check.
func DefaultCacheDir() string {
	if dir := os.Getenv("DEADZONE_HUGOT_CACHE"); dir != "" {
		return dir
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(".deadzone-cache", "models")
	}
	return filepath.Join(base, "deadzone", "models")
}
