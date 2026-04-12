// Spike: model comparison — gte-base-en-v1.5 vs nomic vs snowflake vs bge on hugot ORT
// Tracking issue: #71 (parent: #50)
//
// Build:
//
//	CGO_ENABLED=1 LIBRARY_PATH=/tmp/spike-hugot-ort/lib go build -tags ORT -o spike .
//
// Run:
//
//	./spike
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/options"
	"github.com/knights-analytics/hugot/pipelines"
)

// ─── Model Definitions ─────────────────────────────────────────────────────

type modelSpec struct {
	Name         string // short name for table column
	HFRepo       string // HuggingFace repo
	ExpectedDim  int
	MaxCtx       int
	QueryPrefix  string // prepended to queries before embedding
	DocPrefix    string // prepended to documents before embedding
	LongCtxTest  bool   // run test E (long-context)?
	OnnxVariants []onnxVariant
}

type onnxVariant struct {
	Label        string // "int8" or "fp32"
	OnnxFilePath string // repo-relative path for DownloadOptions
	OnnxFilename string // local filename after download
}

var models = []modelSpec{
	{
		Name:        "nomic-v1.5",
		HFRepo:      "nomic-ai/nomic-embed-text-v1.5",
		ExpectedDim: 768,
		MaxCtx:      8192,
		QueryPrefix: "search_query: ",
		DocPrefix:   "search_document: ",
		LongCtxTest: true,
		OnnxVariants: []onnxVariant{
			{"int8", "onnx/model_quantized.onnx", "model_quantized.onnx"},
			{"fp32", "onnx/model.onnx", "model.onnx"},
		},
	},
	{
		Name:        "gte-base-v1.5",
		HFRepo:      "Alibaba-NLP/gte-base-en-v1.5",
		ExpectedDim: 768,
		MaxCtx:      8192,
		QueryPrefix: "",
		DocPrefix:   "",
		LongCtxTest: true,
		OnnxVariants: []onnxVariant{
			{"int8", "onnx/model_quantized.onnx", "model_quantized.onnx"},
			{"fp32", "onnx/model.onnx", "model.onnx"},
		},
	},
	{
		Name:        "arctic-m-v1",
		HFRepo:      "Snowflake/snowflake-arctic-embed-m",
		ExpectedDim: 768,
		MaxCtx:      512,
		QueryPrefix: "Represent this sentence for searching relevant passages: ",
		DocPrefix:   "",
		LongCtxTest: false,
		OnnxVariants: []onnxVariant{
			{"int8", "onnx/model_quantized.onnx", "model_quantized.onnx"},
			{"fp32", "onnx/model.onnx", "model.onnx"},
		},
	},
	{
		Name:        "arctic-m-v2",
		HFRepo:      "Snowflake/snowflake-arctic-embed-m-v2.0",
		ExpectedDim: 768,
		MaxCtx:      8192,
		QueryPrefix: "query: ",
		DocPrefix:   "",
		LongCtxTest: true,
		OnnxVariants: []onnxVariant{
			{"int8", "onnx/model_quantized.onnx", "model_quantized.onnx"},
			{"fp32", "onnx/model.onnx", "model.onnx"},
		},
	},
}

// ─── Shared Test Data ───────────────────────────────────────────────────────

const (
	shortQuery = "how to add middleware in FastAPI"

	docMatch = "FastAPI middleware lets you intercept and modify requests and responses. " +
		"You can add middleware using the @app.middleware decorator. Middleware " +
		"functions receive a request and a call_next function. Common use cases " +
		"include logging, CORS headers, authentication checks, and request timing. " +
		"The middleware runs for every request before it reaches the route handler."

	docNoise = "PostgreSQL is a powerful open-source relational database management system. " +
		"It supports advanced data types, full-text search, and JSON operations. " +
		"PostgreSQL uses MVCC for concurrency control and supports both SQL and " +
		"NoSQL workloads. It is widely used in web applications, data warehousing, " +
		"and geospatial applications with the PostGIS extension."
)

// longDoc generates a document of approximately targetTokens by repeating text.
// Rough heuristic: 1 token ≈ 4 characters for English.
func longDoc(targetTokens int) string {
	base := "FastAPI is a modern, fast (high-performance), web framework for building APIs " +
		"with Python based on standard Python type hints. The key features are automatic " +
		"API documentation with Swagger UI and ReDoc, data validation using Pydantic models, " +
		"dependency injection for authentication and database sessions, async/await support " +
		"for high throughput, and middleware for request/response processing. FastAPI achieves " +
		"performance comparable to NodeJS and Go by leveraging Starlette for the web parts " +
		"and Pydantic for the data parts. It is designed to be easy to use while being " +
		"production ready, with automatic OpenAPI schema generation. "
	targetChars := targetTokens * 4
	var b strings.Builder
	for b.Len() < targetChars {
		b.WriteString(base)
	}
	return b.String()[:targetChars]
}

// ─── Results ────────────────────────────────────────────────────────────────

type modelResult struct {
	Name     string
	LoadErr  string // non-empty if model failed to load
	Variant  string // "int8" or "fp32"
	ActualDim int
	NormOK   bool
	L2Norm   float64

	ShortLatency  []time.Duration
	LongLatency   []time.Duration
	VLongLatency  []time.Duration

	CosMatch float64
	CosNoise float64
	Gap      float64

	RSSMB float64

	LongCtx1500OK      bool
	LongCtx3000OK      bool
	LongCtxCosine      float64 // cosine(1500tok, 3000tok)
	LongCtx1500Latency time.Duration
	LongCtx3000Latency time.Duration
	LongCtxSkip        bool // true if ctx <= 512

	OnnxFileSize int64 // bytes
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func cosine(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func l2norm(v []float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum)
}

func getRSSMB() float64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return -1
	}
	kb, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return kb / 1024.0
}

func median(ds []time.Duration) time.Duration {
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func fmtMs(d time.Duration) string {
	return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000.0)
}

func onnxFileSize(cacheDir, hfRepo, filename string) int64 {
	p := filepath.Join(cacheDir, strings.ReplaceAll(hfRepo, "/", "_"), filename)
	fi, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return fi.Size()
}

// ─── Download ───────────────────────────────────────────────────────────────

// downloadModel tries each onnx variant in order. Returns (modelDir, variant, error).
func downloadModel(spec modelSpec, cacheDir string) (string, onnxVariant, error) {
	modelDir := filepath.Join(cacheDir, strings.ReplaceAll(spec.HFRepo, "/", "_"))

	for _, v := range spec.OnnxVariants {
		localPath := filepath.Join(modelDir, v.OnnxFilename)
		if _, err := os.Stat(localPath); err == nil {
			fmt.Printf("  [cached] %s → %s\n", v.Label, localPath)
			return modelDir, v, nil
		}

		fmt.Printf("  [download] trying %s (%s) ... ", v.Label, v.OnnxFilePath)
		opts := hugot.NewDownloadOptions()
		opts.OnnxFilePath = v.OnnxFilePath
		opts.Verbose = false
		dir, err := hugot.DownloadModel(spec.HFRepo, cacheDir, opts)
		if err != nil {
			fmt.Printf("FAIL: %v\n", err)
			// Clean up partial download before trying next variant.
			_ = os.RemoveAll(modelDir)
			continue
		}
		fmt.Printf("OK → %s\n", dir)
		return dir, v, nil
	}
	return "", onnxVariant{}, fmt.Errorf("all ONNX variants failed for %s", spec.HFRepo)
}

// ─── Per-Model Test Runner ──────────────────────────────────────────────────

func testModel(spec modelSpec, ortLibDir, cacheDir string) modelResult {
	res := modelResult{Name: spec.Name}

	// 1. Download
	modelDir, variant, err := downloadModel(spec, cacheDir)
	if err != nil {
		res.LoadErr = err.Error()
		return res
	}
	res.Variant = variant.Label
	res.OnnxFileSize = onnxFileSize(cacheDir, spec.HFRepo, variant.OnnxFilename)

	// 2. Create ORT session
	session, err := hugot.NewORTSession(
		options.WithOnnxLibraryPath(ortLibDir),
	)
	if err != nil {
		res.LoadErr = fmt.Sprintf("ORT session: %v", err)
		return res
	}
	defer func() {
		if dErr := session.Destroy(); dErr != nil {
			fmt.Printf("  [warn] session.Destroy: %v\n", dErr)
		}
	}()

	// 3. Create pipeline
	cfg := hugot.FeatureExtractionConfig{
		ModelPath:    modelDir,
		Name:         spec.Name,
		OnnxFilename: variant.OnnxFilename,
		Options: []hugot.FeatureExtractionOption{
			pipelines.WithNormalization(),
		},
	}
	pipe, err := hugot.NewPipeline(session, cfg)
	if err != nil {
		res.LoadErr = fmt.Sprintf("pipeline: %v", err)
		return res
	}

	embed := func(text string) ([]float32, error) {
		out, runErr := pipe.RunPipeline([]string{text})
		if runErr != nil {
			return nil, runErr
		}
		if out == nil || len(out.Embeddings) == 0 {
			return nil, errors.New("no embeddings returned")
		}
		return out.Embeddings[0], nil
	}

	// ─── Test A: Basic load + inference ─────────────────────────
	vec, err := embed("hello world")
	if err != nil {
		res.LoadErr = fmt.Sprintf("embed hello world: %v", err)
		return res
	}
	res.ActualDim = len(vec)
	res.L2Norm = l2norm(vec)
	res.NormOK = math.Abs(res.L2Norm-1.0) < 0.01

	// ─── Test B: Latency ────────────────────────────────────────
	shortText := spec.QueryPrefix + shortQuery
	longText := spec.DocPrefix + longDoc(800)
	vlongText := spec.DocPrefix + longDoc(2000)

	benchEmbed := func(text string, n int) []time.Duration {
		// Warm-up (discard first call).
		_, _ = embed(text)
		var durations []time.Duration
		for i := 0; i < n; i++ {
			start := time.Now()
			_, _ = embed(text)
			durations = append(durations, time.Since(start))
		}
		return durations
	}

	res.ShortLatency = benchEmbed(shortText, 3)
	res.LongLatency = benchEmbed(longText, 3)
	res.VLongLatency = benchEmbed(vlongText, 3)

	// ─── Test C: Semantic discrimination ────────────────────────
	qVec, _ := embed(spec.QueryPrefix + shortQuery)
	mVec, _ := embed(spec.DocPrefix + docMatch)
	nVec, _ := embed(spec.DocPrefix + docNoise)
	res.CosMatch = cosine(qVec, mVec)
	res.CosNoise = cosine(qVec, nVec)
	res.Gap = res.CosMatch - res.CosNoise

	// ─── Test D: Memory RSS ─────────────────────────────────────
	runtime.GC()
	res.RSSMB = getRSSMB()

	// ─── Test E: Long-context ───────────────────────────────────
	if !spec.LongCtxTest {
		res.LongCtxSkip = true
	} else {
		doc1500 := spec.DocPrefix + longDoc(1500)
		doc3000 := spec.DocPrefix + longDoc(3000)

		start := time.Now()
		v1500, err1 := embed(doc1500)
		res.LongCtx1500Latency = time.Since(start)
		res.LongCtx1500OK = err1 == nil && len(v1500) == spec.ExpectedDim

		start = time.Now()
		v3000, err2 := embed(doc3000)
		res.LongCtx3000Latency = time.Since(start)
		res.LongCtx3000OK = err2 == nil && len(v3000) == spec.ExpectedDim

		if err1 == nil && err2 == nil {
			res.LongCtxCosine = cosine(v1500, v3000)
		}
	}

	return res
}

// ─── Output ─────────────────────────────────────────────────────────────────

func printResults(results []modelResult) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════╗")
	fmt.Println("║               MODEL COMPARISON RESULTS (hugot ORT)              ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Header row for model names.
	hdr := fmt.Sprintf("%-30s", "Metric")
	for _, r := range results {
		hdr += fmt.Sprintf("│ %-18s", r.Name)
	}
	sep := strings.Repeat("─", 30)
	for range results {
		sep += "┼" + strings.Repeat("─", 19)
	}

	fmt.Println(hdr)
	fmt.Println(sep)

	row := func(label string, vals []string) {
		line := fmt.Sprintf("%-30s", label)
		for _, v := range vals {
			line += fmt.Sprintf("│ %-18s", v)
		}
		fmt.Println(line)
	}

	// Model loads
	vals := make([]string, len(results))
	for i, r := range results {
		if r.LoadErr != "" {
			vals[i] = "FAIL"
		} else {
			vals[i] = "YES"
		}
	}
	row("Model loads", vals)

	// ONNX variant used
	for i, r := range results {
		if r.LoadErr != "" {
			vals[i] = "N/A"
		} else {
			vals[i] = r.Variant
		}
	}
	row("ONNX variant used", vals)

	// Output dim
	for i, r := range results {
		if r.LoadErr != "" {
			vals[i] = "N/A"
		} else {
			vals[i] = strconv.Itoa(r.ActualDim)
		}
	}
	row("Output dim", vals)

	// L2 norm
	for i, r := range results {
		if r.LoadErr != "" {
			vals[i] = "N/A"
		} else {
			check := "NO"
			if r.NormOK {
				check = "YES"
			}
			vals[i] = fmt.Sprintf("%s (%.4f)", check, r.L2Norm)
		}
	}
	row("L2 norm = 1.0", vals)

	fmt.Println(sep)

	// Latency
	for i, r := range results {
		if r.LoadErr != "" || len(r.ShortLatency) == 0 {
			vals[i] = "N/A"
		} else {
			vals[i] = fmtMs(median(r.ShortLatency))
		}
	}
	row("Short query latency (warm)", vals)

	for i, r := range results {
		if r.LoadErr != "" || len(r.LongLatency) == 0 {
			vals[i] = "N/A"
		} else {
			vals[i] = fmtMs(median(r.LongLatency))
		}
	}
	row("Long doc ~800tok latency", vals)

	for i, r := range results {
		if r.LoadErr != "" || len(r.VLongLatency) == 0 {
			vals[i] = "N/A"
		} else {
			vals[i] = fmtMs(median(r.VLongLatency))
		}
	}
	row("Very long ~2000tok latency", vals)

	fmt.Println(sep)

	// Semantic discrimination
	for i, r := range results {
		if r.LoadErr != "" {
			vals[i] = "N/A"
		} else {
			vals[i] = fmt.Sprintf("%.4f", r.CosMatch)
		}
	}
	row("cosine(query, doc_match)", vals)

	for i, r := range results {
		if r.LoadErr != "" {
			vals[i] = "N/A"
		} else {
			vals[i] = fmt.Sprintf("%.4f", r.CosNoise)
		}
	}
	row("cosine(query, doc_noise)", vals)

	for i, r := range results {
		if r.LoadErr != "" {
			vals[i] = "N/A"
		} else {
			vals[i] = fmt.Sprintf("%.4f", r.Gap)
		}
	}
	row("Discrimination gap", vals)

	fmt.Println(sep)

	// Long context
	for i, r := range results {
		if r.LoadErr != "" {
			vals[i] = "N/A"
		} else if r.LongCtxSkip {
			vals[i] = "N/A (512 ctx)"
		} else if r.LongCtx1500OK {
			vals[i] = fmt.Sprintf("YES (%s)", fmtMs(r.LongCtx1500Latency))
		} else {
			vals[i] = "FAIL"
		}
	}
	row("Long ctx: 1500tok works?", vals)

	for i, r := range results {
		if r.LoadErr != "" {
			vals[i] = "N/A"
		} else if r.LongCtxSkip {
			vals[i] = "N/A (512 ctx)"
		} else if r.LongCtx3000OK {
			vals[i] = fmt.Sprintf("YES (%s)", fmtMs(r.LongCtx3000Latency))
		} else {
			vals[i] = "FAIL"
		}
	}
	row("Long ctx: 3000tok works?", vals)

	for i, r := range results {
		if r.LoadErr != "" {
			vals[i] = "N/A"
		} else if r.LongCtxSkip {
			vals[i] = "N/A"
		} else {
			vals[i] = fmt.Sprintf("%.4f", r.LongCtxCosine)
		}
	}
	row("Long ctx: cos(1500,3000)<1?", vals)

	fmt.Println(sep)

	// Memory + ONNX file size
	for i, r := range results {
		if r.LoadErr != "" {
			vals[i] = "N/A"
		} else {
			vals[i] = fmt.Sprintf("%.0f MB", r.RSSMB)
		}
	}
	row("Memory RSS", vals)

	for i, r := range results {
		if r.LoadErr != "" || r.OnnxFileSize < 0 {
			vals[i] = "N/A"
		} else {
			vals[i] = fmt.Sprintf("%.0f MB", float64(r.OnnxFileSize)/(1024*1024))
		}
	}
	row("ONNX file size", vals)

	fmt.Println()

	// Load errors
	for _, r := range results {
		if r.LoadErr != "" {
			fmt.Printf("!! %s FAILED: %s\n", r.Name, r.LoadErr)
		}
	}
}

// ─── Main ───────────────────────────────────────────────────────────────────

func main() {
	ortLibDir := "/tmp/spike-hugot-ort/lib/onnxruntime-osx-arm64-1.24.4/lib"
	if v := os.Getenv("ORT_LIB_DIR"); v != "" {
		ortLibDir = v
	}
	cacheDir := "/tmp/spike-model-comparison/models"
	if v := os.Getenv("MODEL_CACHE_DIR"); v != "" {
		cacheDir = v
	}

	// Verify ORT library exists.
	if _, err := os.Stat(filepath.Join(ortLibDir, "libonnxruntime.dylib")); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "ORT library not found at %s/libonnxruntime.dylib\n", ortLibDir)
		fmt.Fprintf(os.Stderr, "Set ORT_LIB_DIR to the directory containing libonnxruntime.dylib\n")
		os.Exit(1)
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create cache dir: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("ORT lib dir:   %s\n", ortLibDir)
	fmt.Printf("Model cache:   %s\n", cacheDir)
	fmt.Printf("Platform:      %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println()

	var results []modelResult
	for i, spec := range models {
		fmt.Printf("━━━ [%d/%d] %s (%s) ━━━\n", i+1, len(models), spec.Name, spec.HFRepo)
		res := testModel(spec, ortLibDir, cacheDir)
		results = append(results, res)
		if res.LoadErr == "" {
			fmt.Printf("  dim=%d  norm=%.4f  gap=%.4f  RSS=%.0fMB\n",
				res.ActualDim, res.L2Norm, res.Gap, res.RSSMB)
		} else {
			fmt.Printf("  FAILED: %s\n", res.LoadErr)
		}
		fmt.Println()
	}

	printResults(results)
}
