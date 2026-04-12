//go:build ignore

package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/amikos-tech/pure-onnx/embeddings/minilm"
	"github.com/amikos-tech/pure-onnx/ort"
)

func cos(a, b []float32) float64 {
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

func getRSS() int64 {
	pid := os.Getpid()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return -1
	}
	val, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return val // in KB
}

func main() {
	libPath, _ := ort.EnsureOnnxRuntimeSharedLibrary()
	ort.SetSharedLibraryPath(libPath)
	ort.InitializeEnvironment()
	defer ort.DestroyEnvironment()
	fmt.Printf("ORT version: %s\n\n", ort.GetVersionString())

	nomicDir := "/tmp/spike-pure-onnx/cache/nomic"
	fp32Path := nomicDir + "/model.onnx"
	quantPath := nomicDir + "/model_quantized.onnx"
	tokPath := nomicDir + "/tokenizer.json"

	// Download quantized model if not cached
	if _, err := os.Stat(quantPath); err != nil {
		fmt.Println("Downloading quantized model...")
		if err := downloadIfMissing(quantPath,
			"https://huggingface.co/nomic-ai/nomic-embed-text-v1.5/resolve/main/onnx/model_quantized.onnx"); err != nil {
			log.Fatalf("download quantized: %v", err)
		}
	} else {
		info, _ := os.Stat(quantPath)
		fmt.Printf("Quantized model cached: %s (%.1f MB)\n", quantPath, float64(info.Size())/(1024*1024))
	}

	queryMiddleware := "search_query: how to add middleware in FastAPI"
	docMiddleware := "search_document: " + strings.Repeat("FastAPI middleware allows you to add custom processing logic to requests and responses. You can use middleware for authentication, logging, CORS, and rate limiting. ", 100)
	docDatabases := "search_document: " + strings.Repeat("PostgreSQL is a powerful, open source object-relational database system that uses and extends the SQL language. It supports advanced data types, indexing, and full-text search capabilities. ", 100)

	// ──────────────────────────────────────────────────────────
	// Test quantized model with Option B2 (bucketed pool, reuse)
	// ──────────────────────────────────────────────────────────
	fmt.Println("\n=== Quantized Model (int8) — Option B2 Bucketed ===")

	// Bucket=64 (short text)
	fmt.Println("\n--- Bucket=64 (short text) ---")
	qEmb64, err := minilm.NewEmbedder(quantPath, tokPath,
		minilm.WithSequenceLength(64),
		minilm.WithEmbeddingDimension(768),
		minilm.WithMeanPooling(),
		minilm.WithL2Normalization(),
	)
	if err != nil {
		log.Fatalf("quantized NewEmbedder seq=64: %v", err)
	}

	// Warmup
	qEmb64.EmbedQuery(queryMiddleware)

	// Cached
	start := time.Now()
	qVecQuery, _ := qEmb64.EmbedQuery(queryMiddleware)
	shortLatencyQ := time.Since(start)
	fmt.Printf("Short text (cached): dim=%d, latency=%v\n", len(qVecQuery), shortLatencyQ)

	start = time.Now()
	qVecQuery2, _ := qEmb64.EmbedQuery(queryMiddleware)
	fmt.Printf("Short text (cached, 2nd): dim=%d, latency=%v\n", len(qVecQuery2), time.Since(start))

	// Bucket=2048 (long text)
	fmt.Println("\n--- Bucket=2048 (long text) ---")
	qEmb2048, err := minilm.NewEmbedder(quantPath, tokPath,
		minilm.WithSequenceLength(2048),
		minilm.WithEmbeddingDimension(768),
		minilm.WithMeanPooling(),
		minilm.WithL2Normalization(),
	)
	if err != nil {
		log.Fatalf("quantized NewEmbedder seq=2048: %v", err)
	}

	// Warmup
	qEmb2048.EmbedQuery(docMiddleware)

	// Cached
	start = time.Now()
	qVecDocMw, _ := qEmb2048.EmbedQuery(docMiddleware)
	longLatencyQ := time.Since(start)
	fmt.Printf("Long text doc_mw (cached): dim=%d, latency=%v\n", len(qVecDocMw), longLatencyQ)

	start = time.Now()
	qVecDocDb, _ := qEmb2048.EmbedQuery(docDatabases)
	fmt.Printf("Long text doc_db (cached): dim=%d, latency=%v\n", len(qVecDocDb), time.Since(start))

	// Discrimination
	simHighQ := cos(qVecQuery, qVecDocMw)
	simLowQ := cos(qVecQuery, qVecDocDb)
	gapQ := simHighQ - simLowQ
	fmt.Printf("\ncosine(query_mw, doc_mw) = %.6f\n", simHighQ)
	fmt.Printf("cosine(query_mw, doc_db) = %.6f\n", simLowQ)
	fmt.Printf("Discrimination gap       = %.6f (fp32 was 0.314)\n", gapQ)

	// ──────────────────────────────────────────────────────────
	// Cross-model consistency (fp32 vs quantized)
	// ──────────────────────────────────────────────────────────
	fmt.Println("\n=== Cross-model Consistency (fp32 vs quantized) ===")
	fp32Emb64, _ := minilm.NewEmbedder(fp32Path, tokPath,
		minilm.WithSequenceLength(64),
		minilm.WithEmbeddingDimension(768),
		minilm.WithMeanPooling(),
		minilm.WithL2Normalization(),
	)
	fp32VecQuery, _ := fp32Emb64.EmbedQuery(queryMiddleware)
	simCross := cos(fp32VecQuery, qVecQuery)
	fmt.Printf("cosine(fp32_query, quant_query) = %.6f\n", simCross)

	fp32Emb2048, _ := minilm.NewEmbedder(fp32Path, tokPath,
		minilm.WithSequenceLength(2048),
		minilm.WithEmbeddingDimension(768),
		minilm.WithMeanPooling(),
		minilm.WithL2Normalization(),
	)
	fp32VecDocMw, _ := fp32Emb2048.EmbedQuery(docMiddleware)
	simCrossDoc := cos(fp32VecDocMw, qVecDocMw)
	fmt.Printf("cosine(fp32_doc_mw, quant_doc_mw) = %.6f\n", simCrossDoc)

	// ──────────────────────────────────────────────────────────
	// Memory footprint
	// ──────────────────────────────────────────────────────────
	fmt.Println("\n=== Memory Footprint ===")

	// Close everything first
	qEmb64.Close()
	qEmb2048.Close()
	fp32Emb64.Close()
	fp32Emb2048.Close()
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baselineRSS := getRSS()
	fmt.Printf("Baseline (no embedders): RSS=%d KB (%.1f MB)\n", baselineRSS, float64(baselineRSS)/1024)

	// fp32, 1 bucket
	fp32_1, _ := minilm.NewEmbedder(fp32Path, tokPath,
		minilm.WithSequenceLength(64),
		minilm.WithEmbeddingDimension(768),
		minilm.WithMeanPooling(),
		minilm.WithL2Normalization(),
	)
	fp32_1.EmbedQuery(queryMiddleware) // trigger inference
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	fp32_1_RSS := getRSS()
	fmt.Printf("fp32, 1 bucket (seq=64): RSS=%d KB (%.1f MB), delta=+%.1f MB\n",
		fp32_1_RSS, float64(fp32_1_RSS)/1024, float64(fp32_1_RSS-baselineRSS)/1024)
	fp32_1.Close()

	// fp32, all buckets (64, 128, 256, 512, 1024, 2048)
	buckets := []int{64, 128, 256, 512, 1024, 2048}
	var fp32Embs []*minilm.Embedder
	for _, b := range buckets {
		e, _ := minilm.NewEmbedder(fp32Path, tokPath,
			minilm.WithSequenceLength(b),
			minilm.WithEmbeddingDimension(768),
			minilm.WithMeanPooling(),
			minilm.WithL2Normalization(),
		)
		e.EmbedQuery(queryMiddleware) // trigger inference for each
		fp32Embs = append(fp32Embs, e)
	}
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	fp32AllRSS := getRSS()
	fmt.Printf("fp32, %d buckets: RSS=%d KB (%.1f MB), delta=+%.1f MB\n",
		len(buckets), fp32AllRSS, float64(fp32AllRSS)/1024, float64(fp32AllRSS-baselineRSS)/1024)
	for _, e := range fp32Embs {
		e.Close()
	}
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	// quantized, 1 bucket
	q_1, _ := minilm.NewEmbedder(quantPath, tokPath,
		minilm.WithSequenceLength(64),
		minilm.WithEmbeddingDimension(768),
		minilm.WithMeanPooling(),
		minilm.WithL2Normalization(),
	)
	q_1.EmbedQuery(queryMiddleware)
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	q1RSS := getRSS()
	fmt.Printf("quantized, 1 bucket (seq=64): RSS=%d KB (%.1f MB), delta=+%.1f MB\n",
		q1RSS, float64(q1RSS)/1024, float64(q1RSS-baselineRSS)/1024)
	q_1.Close()

	// quantized, all buckets
	var qEmbs []*minilm.Embedder
	for _, b := range buckets {
		e, _ := minilm.NewEmbedder(quantPath, tokPath,
			minilm.WithSequenceLength(b),
			minilm.WithEmbeddingDimension(768),
			minilm.WithMeanPooling(),
			minilm.WithL2Normalization(),
		)
		e.EmbedQuery(queryMiddleware)
		qEmbs = append(qEmbs, e)
	}
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	qAllRSS := getRSS()
	fmt.Printf("quantized, %d buckets: RSS=%d KB (%.1f MB), delta=+%.1f MB\n",
		len(buckets), qAllRSS, float64(qAllRSS)/1024, float64(qAllRSS-baselineRSS)/1024)
	for _, e := range qEmbs {
		e.Close()
	}

	fmt.Println("\n=== Phase 4 complete ===")
}
