package embed

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
)

// KindHugot is the Kind() value reported by the Hugot embedder, and the only
// kind currently accepted by New.
const KindHugot = "hugot"

// DefaultHugotModel is the sentence-transformers MiniLM checkpoint used by
// default. 384-dim, English-leaning, well-suited to short documentation
// snippets and natural-language queries. Bumping this constant invalidates
// every existing database via db.Meta cross-check on next Open.
const DefaultHugotModel = "sentence-transformers/all-MiniLM-L6-v2"

// Hugot wraps a hugot Session + FeatureExtractionPipeline running on the
// pure-Go GoMLX (simplego) backend. One Hugot is meant to live for the
// lifetime of a process: NewHugot is expensive (downloads + loads the model)
// but each Embed call is cheap.
//
// Concurrency: hugot's pipelines are not documented as goroutine-safe.
// internal/db serializes its single connection, and cmd/server handles one
// MCP request at a time, so a single shared *Hugot is fine for the current
// workload. If parallelism is added later, wrap Embed in a mutex or pool
// pipelines per worker.
type Hugot struct {
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
	model    string
	dim      int
}

// NewHugot constructs a Hugot embedder backed by modelName, downloading the
// model into cacheDir if it isn't already present. Pass an empty modelName to
// use DefaultHugotModel.
//
// First-run cost is dominated by the model download (a few MB for MiniLM)
// and the GoMLX session warm-up. Subsequent runs reuse the on-disk model.
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
	if _, err := os.Stat(filepath.Join(modelDir, "model.onnx")); errors.Is(err, fs.ErrNotExist) {
		opts := hugot.NewDownloadOptions()
		// sentence-transformers repos ship multiple ONNX variants under
		// onnx/. Picking one explicitly avoids hugot's "ambiguous .onnx
		// file" validation error. The downloader copies the file to
		// modelDir/model.onnx regardless of its source path.
		opts.OnnxFilePath = "onnx/model.onnx"
		if _, err := hugot.DownloadModel(modelName, cacheDir, opts); err != nil {
			return nil, fmt.Errorf("hugot: download %s: %w", modelName, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("hugot: stat model file: %w", err)
	}

	session, err := hugot.NewGoSession()
	if err != nil {
		return nil, fmt.Errorf("hugot: new session: %w", err)
	}

	cfg := hugot.FeatureExtractionConfig{
		ModelPath:    modelDir,
		Name:         "deadzone",
		OnnxFilename: "model.onnx",
		Options: []hugot.FeatureExtractionOption{
			// L2-normalize sentence embeddings so vector_distance_cos
			// behaves as a true cosine distance. MiniLM's training
			// objective expects normalized outputs.
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

// Embed runs text through the FeatureExtractionPipeline and returns the
// resulting unit-norm vector. Errors are propagated so callers can decide
// what to do — at index time the scraper logs and skips the doc, at query
// time the server returns the error to the MCP client. Returning a
// deterministic placeholder vector here used to silently pollute the
// cosine index, since every fallback collapsed to the same point in vector
// space and formed a synthetic attractor for any query aligned with that
// dimension.
func (h *Hugot) Embed(text string) ([]float32, error) {
	out, err := h.pipeline.RunPipeline([]string{text})
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
// construction time. 384 for the default MiniLM checkpoint.
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
