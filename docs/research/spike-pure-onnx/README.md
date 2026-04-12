# Spike: pure-onnx (purego ORT + tokenizer)

Evaluates [amikos-tech/pure-onnx](https://github.com/amikos-tech/pure-onnx) v0.0.1
as a replacement for hugot in deadzone's embedding pipeline.

**Tracking issue:** #69 (parent: #50)

## Setup

```bash
cd /tmp/spike-pure-onnx
go mod init spike-pure-onnx
go get github.com/amikos-tech/pure-onnx@v0.0.1

# Build (no CGO)
CGO_ENABLED=0 go build -o spike .

# Run all phases
./spike

# Sequence-length latency sweep
go run bench.go

# Quantized model + memory
go run phase4.go download.go

# Isolated memory measurement
go build -o mem_isolated_bin mem_isolated.go download.go
./mem_isolated_bin fp32 1       # fp32, 1 bucket
./mem_isolated_bin quantized 6  # quantized, 6 buckets
```

## Files

| File | Purpose |
|---|---|
| `main.go` | Phase 1 (MiniLM) + Phase 2 (nomic fp32) |
| `download.go` | HuggingFace model downloader helper |
| `bench.go` | Sequence-length latency sweep |
| `phase4.go` | Quantized model + memory footprint |
| `mem_isolated.go` | Clean isolated memory measurement |

## Key Findings

### Verdict: strong candidate

pure-onnx is the recommended replacement for hugot. No CGO, purego ORT,
auto-managed dylib download, clean API.

### Recommended configuration for deadzone

- **Model:** nomic-embed-text-v1.5 quantized (int8, 131 MB)
- **Sequence length:** 512 (fixed, no bucketed pool)
- **Embedding dim:** 768
- **Pooling:** mean + L2 normalization
- **Do NOT use** `WithoutTokenTypeIDsInput()` — nomic ONNX requires token_type_ids

### Performance (macOS arm64, CPU)

| Metric | Value |
|---|---|
| Short query latency (cached, seq=512) | ~100ms |
| Memory (single session, seq=512) | ~300 MB |
| Discrimination gap | 0.281 |
| Model download | 131 MB (auto-cached by ORT) |

### Gotchas

1. `WithoutTokenTypeIDsInput()` fails for nomic — the ONNX graph requires `token_type_ids`
2. seq=8192 is impractical on CPU (~8s/query); seq=512 is the sweet spot
3. Default seq_len is 256, must set explicitly
4. Cannot mix fp32 and quantized vectors in the same index (cosine drift ~0.93)
5. Multiple ORT sessions multiply memory linearly (each loads model weights)
6. Requires Go 1.24+
