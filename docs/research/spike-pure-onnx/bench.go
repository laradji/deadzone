//go:build ignore

package main

import (
	"fmt"
	"log"
	"time"

	"github.com/amikos-tech/pure-onnx/embeddings/minilm"
	"github.com/amikos-tech/pure-onnx/ort"
)

func main() {
	libPath, _ := ort.EnsureOnnxRuntimeSharedLibrary()
	ort.SetSharedLibraryPath(libPath)
	ort.InitializeEnvironment()
	defer ort.DestroyEnvironment()

	seqLengths := []int{256, 512, 1024, 2048, 8192}
	query := "search_query: how to add middleware in FastAPI"

	for _, sl := range seqLengths {
		emb, err := minilm.NewEmbedder(
			"/tmp/spike-pure-onnx/cache/nomic/model.onnx",
			"/tmp/spike-pure-onnx/cache/nomic/tokenizer.json",
			minilm.WithSequenceLength(sl),
			minilm.WithEmbeddingDimension(768),
			minilm.WithMeanPooling(),
			minilm.WithL2Normalization(),
		)
		if err != nil {
			log.Fatalf("seq=%d: %v", sl, err)
		}

		// Warmup
		emb.EmbedQuery(query)

		start := time.Now()
		vec, err := emb.EmbedQuery(query)
		elapsed := time.Since(start)
		if err != nil {
			log.Fatalf("seq=%d embed: %v", sl, err)
		}
		fmt.Printf("seq_length=%5d → dim=%d, latency=%v\n", sl, len(vec), elapsed)
		emb.Close()
	}
}
