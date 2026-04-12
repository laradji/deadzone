# Embedder Choice — Research Notes

> **Status: research complete, 2026-04-12. Decision shipped in
> [#72](https://github.com/laradji/deadzone/pull/82).** Original question
> ("is MiniLM-L6-v2 the right embedder?") was tracked in
> [#50](https://github.com/laradji/deadzone/issues/50), forced onto the
> critical path by [#62](https://github.com/laradji/deadzone/issues/62)
> (hugot panic on inputs >512 tokens, 47% data loss on real corpora), and
> resolved empirically via three parallel spikes
> ([#67](https://github.com/laradji/deadzone/issues/67) hugot-ORT,
> [#68](https://github.com/laradji/deadzone/issues/68) onnxer,
> [#69](https://github.com/laradji/deadzone/issues/69) pure-onnx). Issue
> #50 closed as superseded by the landed implementation.

---

## TL;DR — decision

**Switch from `sentence-transformers/all-MiniLM-L6-v2` (384-dim, 512-token, pure-Go GoMLX) to `nomic-ai/nomic-embed-text-v1.5` int8 quantized (768-dim, 8192-token, hugot ORT backend via CGO).**

Trade-off accepted: drop the "CGO-free end-to-end" property for the embedder stage. `tursogo` stays CGO-free via `purego`, so the overall binary still has zero mandatory system deps at install time — `libonnxruntime` is auto-fetched + SHA256-verified at first run via `internal/ort.Bootstrap`, and `libtokenizers.a` is statically linked into the binary at build time. End users still download one tarball, extract, and run.

This unlocks ~16× longer context (512 → 8192 tokens), 2× embedding capacity (384 → 768 dim), and retires the #62 panic without byte-level truncation hacks.

---

## The original question

The MiniLM-L6 pick in the Phase 2 hugot migration was "good enough to ship", not "optimal". Five concerns surfaced in the #50 body:

1. **English-only.** Multilingual variants exist (`paraphrase-multilingual-MiniLM-L12-v2`, `multilingual-e5-small`). A non-trivial fraction of indexed docs are non-English (French AWS guides, Spanish Django tutorials, Chinese Vue community docs) — currently embedded into a space that represents them poorly.
2. **Trained on prose, not code.** Code-heavy doc snippets embed badly: `mcp.AddTool` ends up close to `mcp.AddPrompt` because they share tokens, not because they're semantically similar in usage.
3. **256/512-token cap with silent truncation.** H2 splitting in some docs produces chunks longer than this — the second half is silently dropped from the embedding (the content is still stored, but the query match is based on a partial signal).
4. **384 dimensions is small.** MTEB sweet spot is 768+ for technical retrieval.
5. **The model is from 2021.** The field has moved — BGE, E5, GTE, nomic, mxbai all post-date MiniLM and lead the leaderboard in the same size class.

**What made this urgent:** #62 turned concern (3) from "quality degradation" into a **hard crash**. hugot's FeatureExtractionPipeline panics (rather than truncates) when inputs exceed the model's max sequence length. On real documentation corpora the tail distribution of chunk lengths puts ~47% of chunks over 512 tokens, so the scraper would abort mid-library on almost any non-trivial source.

---

## The blocker: GoMLX can't run modern embedders

Deep-dive into `hugot@v0.7.0` (`backends/model_gomlx.go`) revealed three hard constraints on the pure-Go GoMLX backend:

1. **Fixed sequence buckets `[32, 128, 512]`**, tunable via `WithGoMLXSequenceBuckets()` but that doesn't touch the position encoding problem below.
2. **Absolute position embeddings only** (`model_gomlx.go:242-248`) — position IDs are sequential `[1, 2, 3, …, seq_len]` (BERT-style). No RoPE. No ALiBi.
3. **README explicitly scopes the Go backend** to "simpler workloads… for smaller models such as all-MiniLM-L6-v2".

**Every embedding model with >512-token context uses RoPE or ALiBi.** BERT's absolute position table is fixed at init time — any model that wants longer context must use a different position scheme, which GoMLX cannot execute.

### Candidate evaluation at the backend level

| Model | Ctx | Dim | ONNX (q8) | License | Pos. encoding | GoMLX |
|---|---|---|---|---|---|---|
| nomic-embed-text-v1.5 | 8192 | 768 | 131 MB | Apache-2.0 | RoPE + SwiGLU | **NO** |
| jina-embeddings-v2-small-en | 8192 | 512 | 130 MB | Apache-2.0 | ALiBi | **NO** |
| gte-base-en-v1.5 | 8192 | 768 | 147 MB | Apache-2.0 | RoPE + GLU | **NO** |
| bge-m3 | 8192 | 1024 | ~2.2 GB | MIT | XLM-RoBERTa | too large |
| bge-small-en-v1.5 | 512 | 384 | 130 MB | MIT | BERT-absolute | yes (same 512 cap) |
| e5-small-v2 | 512 | 384 | 127 MB | MIT | BERT-absolute | yes (same 512 cap) |
| snowflake-arctic-embed-m | 512 | 768 | 436 MB | Apache-2.0 | BERT-absolute | yes (same 512 cap) |

**Result: zero viable >512-token models on GoMLX.** Staying pure-Go caps embedding quality at 2021-era BERT variants with 512-token context. The panic in #62 therefore cannot be fixed by swapping model while keeping the backend.

Sub-chunking with parent-doc retrieval was considered as a workaround but rejected: Deadzone doesn't just index, it **restitutes** content to LLM agents, and returning a fragment that starts mid-section breaks that contract. Schema complexity (`parent_id`) also grows without solving the underlying problem that each embedding still only sees part of the content.

---

## The three spikes

If the embedder is going to exceed 512 tokens, the ONNX backend has to change. Three candidates ran in parallel:

- **#67 — hugot with ORT backend.** Keep hugot, swap `NewGoSession()` → `NewORTSession()`. Requires `CGO_ENABLED=1` and `libtokenizers.a` at build time.
- **#68 — onnxer.** Pure-Go inference via `purego` calling `libonnxruntime` at runtime, paired with `gomlx/go-huggingface/tokenizers` (pure-Go tokenizer). Preserves CGO-free build but requires writing the pipeline (~200 LOC).
- **#69 — pure-onnx.** Higher-level wrapper with a built-in pipeline and auto-download. Also CGO-free.

### Benchmark matrix (nomic-embed-text-v1.5 int8)

| | #67 hugot-ORT | #68 onnxer | #69 pure-onnx |
|---|---|---|---|
| CGO required | yes | no | no |
| Short-query latency | **3.8 ms** | 4.0 ms | 12 ms |
| Long-text ~1500tok | 393 ms | 227 ms | 496 ms |
| Discrimination gap | **0.360** | 0.303 | 0.281 |
| RSS memory | 835 MB | 1036 MB | **221 MB** |
| Sequence length | dynamic | dynamic | fixed (padded) |
| Pipeline | **included** | to build (~200 LOC) | included |
| Bugs to patch | none | `AddCleanup` (7 lines) | none |
| API delta from current | **1 line** | full rewrite | full rewrite |

### Why hugot-ORT won

It led on the two metrics that move retrieval quality (discrimination gap, short latency) and required a one-line change in `internal/embed/hugot.go` — the rest of the Embedder surface and the Turso schema path stay identical. onnxer came close on latency but required reimplementing the pipeline and shipping a 7-line vendor patch for a goroutine-leak bug. pure-onnx pays a 3× latency penalty on short queries due to fixed-padding sequence length, which is the dominant query shape for an MCP client.

**What we gave up:** `CGO_ENABLED=0` at build time. This is load-bearing for cross-compilation and was originally the reason GoMLX was picked. The packaging problem was addressed in [#70](https://github.com/laradji/deadzone/issues/70) (research), which landed on **native runners per target** instead of goreleaser-cross — see `.github/workflows/release.yml`. `libtokenizers.a` is downloaded per-platform from `daulet/tokenizers` releases and statically linked; `libonnxruntime` is runtime-dlopened after a SHA256-verified download on first launch via `internal/ort.Bootstrap`.

From the end-user's perspective, the "single binary download, no system deps" property holds: one tarball, extract, run. The only observable difference is a ~33 MB first-launch fetch that is cached across runs.

---

## What changed in the code

All shipped via #72 (PR [#82](https://github.com/laradji/deadzone/pull/82)) and #73 (PR [#83](https://github.com/laradji/deadzone/pull/83)):

- `internal/embed/hugot.go`:
  - `DefaultHugotModel = "nomic-ai/nomic-embed-text-v1.5"`
  - `onnxFilename = "model_quantized.onnx"` (int8 quantized, ~131 MB)
  - Session constructor: `hugot.NewORTSession(options.WithOnnxLibraryPath(libDir))` via `internal/ort.Bootstrap("")`
  - Split the `Embedder` interface into `EmbedQuery` / `EmbedDocument` so call sites commit to nomic's `"search_query: "` / `"search_document: "` prefixes up front — skipping the prefix silently degrades retrieval quality. See `internal/embed/hugot.go:41-44`.
  - `WithNormalization()` kept — `vector_distance_cos` still expects unit-norm vectors.
- `internal/ort/ort.go`: new package for `libonnxruntime` auto-fetch. Pinned ORT v1.24.4 with per-platform SHA256 manifest, scratch-dir + atomic rename install, `DEADZONE_ORT_LIB_PATH` escape hatch for air-gapped installs.
- `internal/db/db.go`: `CurrentSchemaVersion = 3` (bumped from 2). Embedder-mismatch rejection on Open surfaces as `ErrEmbedderMismatch`. `LibEmbedder` narrowed to `EmbedDocument` only — libs are always indexed as documents.
- `.github/workflows/release.yml`: native matrix for `macos-15` / `ubuntu-24.04` / `ubuntu-24.04-arm` (no cross-compile). See `docs/research/ingestion-architecture.md` once it's updated for the architecture-level write-up.

No data migration path: pre-1.0, no existing users, `ErrEmbedderMismatch` on old DBs is the intended outcome — users re-scrape or re-download the rolling `packs` release.

---

## Holds at scale?

- **Query latency.** 3.8 ms per short query (ORT, int8 quantized) is well under any user-perceptible threshold; even the 393 ms long-text embed is fine for index-time work. At 10k libs × 100 docs each, full corpus embedding is ~11 h on a single laptop — tolerable for a one-shot rebuild, and re-scrapes are per-library not global.
- **Storage.** 768-dim `F32_BLOB` doubles the vector column size versus 384-dim (3 kB per row instead of 1.5 kB). At 1M docs that's ~3 GB of vector payload on disk — fine, no index structure change needed. Linear scan + `vector_distance_cos` still holds at current sizes; `#45` tracks the sub-second threshold beyond which an approximate index becomes necessary.
- **Cold start.** ~164 MB first-launch fetch (33 MB ORT + 131 MB model) is a one-time cost per cache. Pre-populating `DEADZONE_ORT_LIB_PATH` + `DEADZONE_HUGOT_CACHE` on machines that can't reach the internet is already wired.
- **License.** nomic-embed-text-v1.5 is Apache-2.0 (verified on the HF model card, 2026-04-12). `libonnxruntime` is MIT. `daulet/tokenizers` is MIT. All compatible with Deadzone's Apache-2.0 license. `NOTICE` updated accordingly.

---

## What was out of scope

- **Formal benchmark harness against a 50-lib test corpus.** The #50 "ideal decision" plan called for a structured evaluation (recall@K, precision@K, MRR). The #62 panic forced action before that harness existed — the decision was taken on spike results plus the end-to-end smoke test of a real second library (FastAPI, #58) where cross-lib discrimination came in clean at 0.70 vs 0.10. A formal benchmark remains useful future work but is no longer a gate on shipping 0.1.
- **Multilingual support.** nomic-embed-text-v1.5 is English-trained. Multilingual is a future issue, not blocking 0.1.
- **Code-specific embedders** (`codebert`, `unixcoder`, `codet5p`). The spikes showed nomic performs well enough on code-heavy doc chunks to ship; a code-vs-prose split is premature optimization.
- **Custom training.** Way out of scope for an MVP.

---

## Related

- #50 — this research issue (closed as superseded by #72, 2026-04-12)
- #62 — the panic that forced the decision onto the critical path (closed by #72)
- #67 / #68 / #69 — the three implementation spikes
- #70 — CGO release strategy research (concluded: native runners, not goreleaser-cross)
- #72 — embedder swap PR
- #73 — ORT auto-download bootstrap
- `docs/research/ingestion-architecture.md` — the canonical v0 decision log; decision #8 (MiniLM) is now superseded by this note
- `internal/embed/hugot.go`, `internal/ort/ort.go`, `internal/db/db.go` — the landed code
