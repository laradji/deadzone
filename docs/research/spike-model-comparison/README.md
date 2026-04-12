# Spike: Model Comparison on hugot ORT

Compares 4 embedding models head-to-head using [hugot](https://github.com/knights-analytics/hugot)
with ONNX Runtime backend on macOS arm64 CPU.

**Tracking issue:** #71 (parent: #50)

## Models

| Model | Dim | Ctx | License |
|---|---|---|---|
| `nomic-ai/nomic-embed-text-v1.5` | 768 | 8192 | Apache-2.0 |
| `Alibaba-NLP/gte-base-en-v1.5` | 768 | 8192 | Apache-2.0 |
| `Snowflake/snowflake-arctic-embed-m` | 768 | 512 | Apache-2.0 |
| `Snowflake/snowflake-arctic-embed-m-v2.0` | 768 | 8192 | Apache-2.0 |

## Prerequisites

Reuse the spike #67 setup:

```
/tmp/spike-hugot-ort/lib/
├── libtokenizers.a
└── onnxruntime-osx-arm64-1.24.4/
    └── lib/
        └── libonnxruntime.dylib
```

## Build

```bash
cd docs/research/spike-model-comparison
go mod tidy

CGO_ENABLED=1 \
LIBRARY_PATH=/tmp/spike-hugot-ort/lib \
go build -tags ORT -o spike .
```

## Run

```bash
./spike
```

Environment variables (optional):

| Variable | Default | Description |
|---|---|---|
| `ORT_LIB_DIR` | `/tmp/spike-hugot-ort/lib/onnxruntime-osx-arm64-1.24.4/lib` | Directory containing `libonnxruntime.dylib` |
| `MODEL_CACHE_DIR` | `/tmp/spike-model-comparison/models` | Directory for downloaded model files |

## Tests per model

- **A.** Basic load + inference (dim, L2 norm)
- **B.** Latency: short query, long doc (~800 tok), very long doc (~2000 tok)
- **C.** Semantic discrimination: cosine similarity gap on fixed test texts
- **D.** Memory RSS after load + inference
- **E.** Long-context (8192-ctx models only): 1500 tok + 3000 tok documents
