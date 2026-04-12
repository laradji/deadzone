package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amikos-tech/pure-onnx/embeddings/minilm"
	"github.com/amikos-tech/pure-onnx/ort"
)

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

func main() {
	// ─── Bootstrap ORT ───────────────────────────────────────────
	fmt.Println("=== ORT Bootstrap ===")
	start := time.Now()
	libPath, err := ort.EnsureOnnxRuntimeSharedLibrary()
	if err != nil {
		log.Fatalf("EnsureOnnxRuntimeSharedLibrary: %v", err)
	}
	fmt.Printf("ORT library path: %s (ensure took %v)\n", libPath, time.Since(start))

	ort.SetSharedLibraryPath(libPath)
	if err := ort.InitializeEnvironment(); err != nil {
		log.Fatalf("InitializeEnvironment: %v", err)
	}
	defer ort.DestroyEnvironment()
	fmt.Printf("ORT version: %s\n\n", ort.GetVersionString())

	// ─── Phase 1: MiniLM-L6-v2 ──────────────────────────────────
	fmt.Println("=== Phase 1: MiniLM-L6-v2 ===")

	cacheDir := "/tmp/spike-pure-onnx/cache"
	os.MkdirAll(cacheDir, 0o755)
	modelPath := filepath.Join(cacheDir, "model.onnx")
	tokPath := filepath.Join(cacheDir, "tokenizer.json")

	// Download MiniLM model + tokenizer if not cached
	if err := downloadIfMissing(modelPath,
		"https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/main/onnx/model.onnx"); err != nil {
		log.Fatalf("download model: %v", err)
	}
	if err := downloadIfMissing(tokPath,
		"https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/main/tokenizer.json"); err != nil {
		log.Fatalf("download tokenizer: %v", err)
	}

	embedder, err := minilm.NewEmbedder(modelPath, tokPath,
		minilm.WithMeanPooling(), minilm.WithL2Normalization(),
	)
	if err != nil {
		log.Fatalf("NewEmbedder (MiniLM): %v", err)
	}
	defer embedder.Close()
	fmt.Println("Model + tokenizer loaded: OK")

	// Short text
	start = time.Now()
	vec, err := embedder.EmbedQuery("hello world")
	shortLatency := time.Since(start)
	if err != nil {
		log.Fatalf("EmbedQuery short: %v", err)
	}
	fmt.Printf("Short text (\"hello world\"): dim=%d, latency=%v\n", len(vec), shortLatency)
	shortVec := vec

	// Batch embedding
	docs := []string{
		"FastAPI is a modern web framework for building APIs with Python.",
		"Kubernetes orchestrates containerized workloads across clusters.",
		"SQLite is a self-contained SQL database engine.",
	}
	start = time.Now()
	batchVecs, err := embedder.EmbedDocuments(docs)
	batchLatency := time.Since(start)
	if err != nil {
		log.Fatalf("EmbedDocuments batch: %v", err)
	}
	fmt.Printf("Batch (3 docs): dims=%d/%d/%d, latency=%v\n",
		len(batchVecs[0]), len(batchVecs[1]), len(batchVecs[2]), batchLatency)

	// Long text (~3000 words)
	longText := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 500)
	start = time.Now()
	vec, err = embedder.EmbedQuery(longText)
	longLatency := time.Since(start)
	if err != nil {
		log.Fatalf("EmbedQuery long: %v", err)
	}
	fmt.Printf("Long text (~3000 words): dim=%d, latency=%v (truncated, no panic)\n", len(vec), longLatency)
	longVec := vec

	sim := cosine(shortVec, longVec)
	fmt.Printf("cosine(short, long): %.4f\n\n", sim)

	// ─── Phase 2: nomic-embed-text-v1.5 ─────────────────────────
	fmt.Println("=== Phase 2: nomic-embed-text-v1.5 ===")

	nomicDir := filepath.Join(cacheDir, "nomic")
	os.MkdirAll(nomicDir, 0o755)
	nomicModelPath := filepath.Join(nomicDir, "model.onnx")
	nomicTokPath := filepath.Join(nomicDir, "tokenizer.json")

	if err := downloadIfMissing(nomicModelPath,
		"https://huggingface.co/nomic-ai/nomic-embed-text-v1.5/resolve/main/onnx/model.onnx"); err != nil {
		log.Fatalf("download nomic model: %v", err)
	}
	if err := downloadIfMissing(nomicTokPath,
		"https://huggingface.co/nomic-ai/nomic-embed-text-v1.5/resolve/main/tokenizer.json"); err != nil {
		log.Fatalf("download nomic tokenizer: %v", err)
	}

	nomicModelInfo, _ := os.Stat(nomicModelPath)
	nomicTokInfo, _ := os.Stat(nomicTokPath)
	fmt.Printf("Model file: %s (%.1f MB)\n", nomicModelPath, float64(nomicModelInfo.Size())/(1024*1024))
	fmt.Printf("Tokenizer file: %s (%.1f KB)\n", nomicTokPath, float64(nomicTokInfo.Size())/1024)

	// First try: WITH token_type_ids (the ONNX graph requires it as input)
	// The issue assumed WithoutTokenTypeIDsInput() but model errors without it.
	nomicEmb, err := minilm.NewEmbedder(nomicModelPath, nomicTokPath,
		minilm.WithSequenceLength(8192),
		minilm.WithEmbeddingDimension(768),
		// NOT using WithoutTokenTypeIDsInput() — nomic ONNX expects token_type_ids
		minilm.WithMeanPooling(),
		minilm.WithL2Normalization(),
	)
	if err != nil {
		log.Fatalf("NewEmbedder (nomic): %v", err)
	}
	defer nomicEmb.Close()
	fmt.Println("Model loads with custom options: OK")

	// Short text — middleware query
	start = time.Now()
	queryMiddleware, err := nomicEmb.EmbedQuery("search_query: how to add middleware in FastAPI")
	shortNomicLatency := time.Since(start)
	if err != nil {
		log.Fatalf("nomic EmbedQuery short: %v", err)
	}
	fmt.Printf("Short text (query_middleware): dim=%d, latency=%v\n", len(queryMiddleware), shortNomicLatency)

	// Long text (~1500 tokens) — middleware doc
	docMiddleware := "search_document: " + strings.Repeat("FastAPI middleware allows you to add custom processing logic to requests and responses. You can use middleware for authentication, logging, CORS, and rate limiting. ", 100)
	start = time.Now()
	vecDocMiddleware, err := nomicEmb.EmbedQuery(docMiddleware)
	longNomicLatency := time.Since(start)
	if err != nil {
		log.Fatalf("nomic EmbedQuery long doc_middleware: %v", err)
	}
	fmt.Printf("Long text (doc_middleware, ~1500 tokens): dim=%d, latency=%v\n", len(vecDocMiddleware), longNomicLatency)

	// Very long text (~4000 tokens)
	veryLongDoc := "search_document: " + strings.Repeat("FastAPI is a modern, fast, web framework for building APIs with Python based on standard Python type hints. It provides automatic interactive API documentation, data validation, serialization and deserialization. ", 200)
	start = time.Now()
	vecVeryLong, err := nomicEmb.EmbedQuery(veryLongDoc)
	veryLongLatency := time.Since(start)
	if err != nil {
		log.Fatalf("nomic EmbedQuery very long: %v", err)
	}
	fmt.Printf("Very long text (~4000 tokens): dim=%d, latency=%v\n", len(vecVeryLong), veryLongLatency)

	// Unrelated doc — databases
	docDatabases := "search_document: " + strings.Repeat("PostgreSQL is a powerful, open source object-relational database system that uses and extends the SQL language. It supports advanced data types, indexing, and full-text search capabilities. ", 100)
	start = time.Now()
	vecDocDatabases, err := nomicEmb.EmbedQuery(docDatabases)
	_ = time.Since(start)
	if err != nil {
		log.Fatalf("nomic EmbedQuery doc_databases: %v", err)
	}

	// Cosine similarities
	simHigh := cosine(queryMiddleware, vecDocMiddleware)
	simLow := cosine(queryMiddleware, vecDocDatabases)
	gap := simHigh - simLow
	fmt.Printf("\ncosine(query_middleware, doc_middleware): %.4f (should be high)\n", simHigh)
	fmt.Printf("cosine(query_middleware, doc_databases): %.4f (should be lower)\n", simLow)
	fmt.Printf("Discrimination gap: %.4f (should be > 0)\n", gap)

	// Also check very long vec is sane
	simVeryLong := cosine(queryMiddleware, vecVeryLong)
	fmt.Printf("cosine(query_middleware, very_long_fastapi): %.4f\n", simVeryLong)

	fmt.Println("\n=== Spike complete ===")

	// Ignore vecVeryLong to avoid unused warnings
	_ = vecVeryLong
}
