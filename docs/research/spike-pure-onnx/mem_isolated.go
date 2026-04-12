//go:build ignore

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/amikos-tech/pure-onnx/embeddings/minilm"
	"github.com/amikos-tech/pure-onnx/ort"
)

func rss() int64 {
	out, _ := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	v, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return v
}

func main() {
	variant := "fp32"
	nbuckets := 1
	if len(os.Args) > 1 {
		variant = os.Args[1]
	}
	if len(os.Args) > 2 {
		nbuckets, _ = strconv.Atoi(os.Args[2])
	}

	libPath, _ := ort.EnsureOnnxRuntimeSharedLibrary()
	ort.SetSharedLibraryPath(libPath)
	ort.InitializeEnvironment()
	defer ort.DestroyEnvironment()

	modelPath := "/tmp/spike-pure-onnx/cache/nomic/model.onnx"
	if variant == "quantized" {
		modelPath = "/tmp/spike-pure-onnx/cache/nomic/model_quantized.onnx"
	}
	tokPath := "/tmp/spike-pure-onnx/cache/nomic/tokenizer.json"

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseRSS := rss()

	allBuckets := []int{64, 128, 256, 512, 1024, 2048}
	buckets := allBuckets[:nbuckets]

	var embs []*minilm.Embedder
	for _, b := range buckets {
		e, err := minilm.NewEmbedder(modelPath, tokPath,
			minilm.WithSequenceLength(b),
			minilm.WithEmbeddingDimension(768),
			minilm.WithMeanPooling(),
			minilm.WithL2Normalization(),
		)
		if err != nil {
			log.Fatal(err)
		}
		// Trigger inference to populate ORT session cache
		e.EmbedQuery("search_query: test")
		embs = append(embs, e)
	}

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	afterRSS := rss()
	delta := afterRSS - baseRSS

	fmt.Printf("variant=%s buckets=%d base=%dKB after=%dKB delta=%dKB (%.1f MB)\n",
		variant, nbuckets, baseRSS, afterRSS, delta, float64(delta)/1024)

	for _, e := range embs {
		e.Close()
	}
}
