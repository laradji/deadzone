# Ingestion architecture (v0 / mvp)

> **Status**: shipped 2026-04-11 (`mvp` milestone closed). This document is the canonical decision log for the doc ingestion pipeline as it stood at v0. Each section captures the *why* behind a design choice, not the *what* (which lives in the code itself and in the closed sub-issues referenced from the **Trace** lines).
>
> **Cross-references**:
> - [`context7-analysis.md`](context7-analysis.md) — the upstream system whose pipeline shape this design draws on
> - [`tursogo-migration.md`](tursogo-migration.md) — the storage-layer decision that shaped half the pipeline (no-CGO + vector search via `vector_distance_cos`)
>
> **Tracking issue**: #15 (closed 2026-04-11 alongside this document being written; this file replaces it as the canonical decision log)

---

## 0. The meta-decision: target scale

Every architectural choice in this document is gated against a single constraint:

> Deadzone aims to reach a corpus comparable to Context7 — eventually **tens of thousands of indexed libraries** (Context7 currently sits around 33k). Near-term targets are in the **2,000–3,000 libs range**.

This is **not** a "long-term aspiration that doesn't influence current decisions". It IS the design constraint. Every decision below was sanity-checked against the question: *does this approach hold at 3,000 libs? At 33,000?* Approaches that only work at "personal-tool scale" (~50 libs) were rejected even when the day-1 corpus was 1.

Two earlier decisions in deadzone's history (the original "list_libraries flat enumeration" proposal in #36, and the original "1 issue per added lib" tracking pattern) had to be reversed because they were sized for personal-tool scale and would not have survived the corpus growth. Both rejections are captured in their respective sub-issues.

**Why the scale framing matters as a meta-decision**: most of the rejected alternatives below are not stupid. They are perfectly good solutions for a personal-tool corpus and would work fine at 50 libs. They get rejected here because they don't compose at the target scale, and re-architecting at 3,000 libs is much more expensive than picking the scale-aware option upfront.

---

## 1. Two source kinds, never more (#27)

### Context

Documentation lives in radically different shapes across the ecosystem: raw markdown on GitHub, rendered HTML on doc sites, JSON-serialized API specs, PDFs for vendor docs, GitBook, ReadTheDocs, mkdocs-material, vitepress, custom corporate sites. Picking the right extraction strategy per shape is the core ingestion problem.

### Options considered

| Option | Approach | Verdict |
|---|---|---|
| **A** | Single `ParseMarkdown` for everything | Rejected — only handles raw markdown sources, can't reach the majority of real-world libs |
| **B** | `Source.Parser` field selecting per-source parsers | Rejected — N parsers grow unboundedly with N source families |
| **C** | Pluggable parser registry with per-site code | Rejected — same fragmentation, just behind an interface |
| **D** | Hand-written HTML scrapers per site | Rejected — unbounded maintenance, breaks on every site redesign |
| **E** | Generic HTML-to-markdown library (e.g. html2md) | Rejected — works on simple blogs, loses structure on dense technical docs |
| **F** | LLM extraction via OpenAI-compatible endpoint | **Selected** for non-trivial sources |
| **G** | Headless browser (Playwright/Puppeteer) | Rejected — heavy dep, slow, fights the no-CGO single-binary goal |

### Decision

**Two source kinds**, never more, distinguished by `kind:` in `libraries_sources.yaml`:

- `github-md` — raw markdown URLs, fast HTTP path, zero LLM. The fast path for sources that publish reliable raw markdown.
- `scrape-via-agent` — HTML/text via an OpenAI-compatible chat completions endpoint (Ollama, vLLM, oMLX, OpenAI proper, …). The catch-all for everything else.

The downstream pipeline (`ParseMarkdown` → chunk → embed → store) is **identical** for both kinds. The only difference is where the markdown came from.

A third kind (`json-api`) is being researched in #1 (terraform.io discovery surfaced JSON:API endpoints under `/v2/` that bypass the SPA shell). If it ships, it joins the short list. The principle stays: **the kind set is bounded by source shapes, not by source families**.

### Rationale

- **Bounded code surface**: at 3,000 libs, the kind set stays at 2 or 3. Per-source code is replaced by per-source config in `libraries_sources.yaml`.
- **Single pipeline**: `ParseMarkdown` and the embed/store path stay stable. Only fetch+preprocess differs by kind.
- **Stay out of the LLM hosting business**: deadzone speaks `POST /v1/chat/completions` and nothing else. The user brings their own runtime. This keeps the binary CGO-free, single-file, ~30 MB. Compatible with every inference server in the 2026 ecosystem.
- **The verifier protects against the most dangerous LLM failure mode** (hallucinated code blocks, see decision 9 below).

### Trace

- Designed in #27, merged in #57
- Two follow-ups landed: #60→#61 (reasoning suppression) and the still-open #63 (error handling hardening), #64 (extraction quality)

### Holds at scale

✅ At 3,000 libs, the per-scrape LLM call cost is meaningful but bounded by `scrape-once-then-cached-as-artifact` (decision 2) and the freshness signal in #47 will keep re-scrapes incremental. The architecture is sound; the cost concern is operational, not architectural.

---

## 2. Per-lib artifact databases (#28)

### Context

The original scraper wrote everything into a single monolithic `deadzone.db`. This causes several problems as soon as the corpus grows past one library:

1. Can't refresh one lib in isolation without re-scraping everything (wasteful) or hand-rolling `DELETE WHERE lib_id=? + INSERT` (error-prone).
2. No natural unit to ship around — distribution is "the whole DB or nothing".
3. No contribution path for "this lib is stale" — outside contributors can't submit a self-contained refresh.
4. Test fixtures are synthetic. No way to test against realistic scraped content.
5. Single-writer DB = sequential scrape, even though the LLM agent could happily handle multiple libs concurrently.

### Options considered

| Option | Approach | Verdict |
|---|---|---|
| **A** | Single monolithic `deadzone.db` (status quo) | Rejected — fails 5 tests above |
| **B** | One Turso `.db` per `lib_id`, no consolidation | Rejected — server would need to query N files |
| **C** | Per-lib SQL dump files (text) | Rejected — re-parsing on every read, no `F32_BLOB` preservation |
| **D** | Per-lib `.db` files + explicit `consolidate` step | **Selected** |

### Decision

**One Turso `.db` artifact per `lib_id`**, written into `./artifacts/` (gitignored), then merged into `deadzone.db` via an explicit `cmd/consolidate` command. The consolidate step uses `ATTACH DATABASE` + `DELETE WHERE lib_id=?` + `INSERT ... SELECT` inside a single transaction, enforcing meta consistency at merge time.

The main DB is a **derived view**; artifacts are the local source of truth.

### Rationale

- **`deadzone scrape -lib /org/project` regenerates one artifact**, leaves the others untouched. Solves problem 1.
- **Each artifact is self-contained** and ships as a single file. Solves problems 2-3.
- **Test fixtures can use real artifacts** when available. Solves problem 4.
- **Scraping is parallelizable per lib** (per-artifact, no shared writer). Solves problem 5.
- **Explicit `consolidate` command, not implicit at server startup.** The server fails fast with a helpful message if `deadzone.db` is missing. Explicit over magic — operators know exactly when consolidation runs.
- **Both `docs` AND `libs` tables merged in the same atomic step.** When #44/#55 introduced the `libs` table for `search_libraries`, the consolidate transaction was extended to merge it alongside `docs`. No special-case logic, just two parallel `DELETE + INSERT` pairs.

### Trace

- Designed in #28, merged in #56
- Companion: #29 (skip-unchanged consolidation, designed, in 0.1 milestone)

### Holds at scale

✅ At 3,000 libs the artifacts directory is ~1 GB total, manageable on disk. The consolidate step is bounded by the number of changed artifacts (combined with #29 for skip-unchanged), not by total corpus size. Distribution is handled by decision 6 below.

---

## 3. Library discovery via dedicated `libs` vector table (#44)

### Context

MCP clients (Claude Code, Cursor, …) need to discover what's indexed in the local Deadzone DB. Without discovery, they can only call `search_docs` if they already know the `lib_id`, which defeats the purpose of having a documentation index. At scale, the right tool is **semantic-resolve-by-name** — the user types `"tf aws"` or `"react native"` and gets back the right `lib_id`.

### Options considered

| Option | Approach | Verdict |
|---|---|---|
| **A** | Flat `list_libraries` MCP tool returning all lib_ids | Rejected — doesn't scale past ~50 libs (response size + cognitive load) |
| **B** | Naive token overlap on lib_id strings | Rejected — fails on the queries LLMs actually emit (`"tf"`/`"k8s"`/`"react native"`) |
| **C** | In-memory cache of embedded lib_id strings, computed at startup | Rejected — cache invalidation nightmare, doesn't compose with per-lib artifacts |
| **D** | Dedicated `libs` vector table parallel to `docs`, embedded once at index time | **Selected** |
| **E** | Reuse `docs` table with a synthetic "title" doc per lib | Rejected — pollutes search_docs results, breaks lib_id filter semantics |

### Decision

A **dedicated `libs` vector table** in the database, one row per library, holding the lib_id text embedded with the same hugot pipeline used at index time (`/hashicorp/terraform-provider-aws` → `embed("hashicorp terraform provider aws")`), plus a `doc_count` column.

Resolution is a `vector_distance_cos` query — the **same primitive** as `search_docs`, just on a different table. The MCP tool `search_libraries(name, limit)` returns top-K hits with `lib_id`, `doc_count`, and a true query-dependent `match_score` (`1 - cosine_distance`).

Helpers `db.UpsertLibIfNew` and `db.UpdateLibCount` handle the lifecycle: row created on first scrape (idempotent), count updated at end of scrape.

### Rationale

- **Reuses** the existing `vector_distance_cos` primitive. No new search idiom to maintain.
- **Composes naturally with #28** — per-lib artifact carries its own `libs` row, merged via plain `INSERT ... SELECT` alongside the `docs` rows in the consolidate transaction.
- **Inherits future #45 vector index speedups for free** — same table shape, same query pattern.
- **No cache invalidation logic** — the table IS the state, and the embedding never gets recomputed since the lib_id is the immutable primary key.
- **Token-overlap approach (option B) was tested empirically** during design and failed on the queries LLMs actually emit. MiniLM handles the semantic projection cleanly where token overlap doesn't.

### Trace

- Designed in #44, merged in #55
- Validated empirically by smoke test #58 on 2026-04-11: `search_libraries("fastapi")` returned `/tiangolo/fastapi` with match_score 0.70, vs `/modelcontextprotocol/go-sdk` at 0.10 — clean 7× separation. Cross-lib regression check passed (`search_libraries("mcp go sdk")` → go-sdk first at 0.47 vs fastapi at 0.16).
- Replaced the closed #36 (which proposed the rejected flat-list approach)

### Holds at scale

✅ At 33k libs the libs table has 33k rows of 384-dim float vectors = ~50 MB. Linear scan via `vector_distance_cos` is fine until #45 lands a vector index, at which point both `search_docs` and `search_libraries` get the speedup for free. The embedding is computed once per lib at index time, never recomputed.

### Schema versioning side-effect

#55 also introduced **schema versioning** via `db.CurrentSchemaVersion = 2` and a new `ErrSchemaMismatch` sentinel. Old DBs without the `libs` table now fail open with a clear error instead of silently working with a missing feature. This pattern is reusable for future schema bumps and was extended by #28 to artifact validation in #56.

---

## 4. Library registry as a deliberately-dumb YAML stop-gap (#51)

### Context

Before #51, the scraper hardcoded a single library and its URLs as Go constants in `cmd/scraper/main.go`. Adding a second lib meant editing source and recompiling — a hard blocker for any multi-library demo. But **what** the long-term registry should look like (federated? committed? generated? versioned?) is a research question with no good answer yet.

### Options considered

| Option | Approach | Verdict |
|---|---|---|
| **A** | Hardcoded Go constants (status quo) | Rejected — blocks every other lib decision |
| **B** | Minimal YAML, four fields (`lib_id`, `kind`, `urls`, `versions`) | **Selected** |
| **C** | Rich YAML with metadata, lifecycle, ownership, deprecation, etc. | Rejected — premature, design unclear |
| **D** | Single-file SQLite registry with schema | Rejected — same prematurity, more friction |
| **E** | Federated multi-source registry | Rejected — way too early, no use cases yet |

### Decision

**Minimal `libraries_sources.yaml`** at the project root with four fields per entry:

```yaml
- lib_id: /modelcontextprotocol/go-sdk    # canonical /org/project ID
  kind: github-md                          # source kind discriminator
  urls:                                    # required: list of doc URLs
    - https://...
  versions: []                             # optional: shorthand for multi-version libs
```

The `versions: []` shorthand expands `{version}` placeholders in URLs and produces one effective `lib_id` per version (matching Context7's `/org/project/version` convention). Opt-in via `-config <file>` flag, with a two-level `-lib` filter:

- `-lib /org/project` matches every expanded version
- `-lib /org/project/v18` matches one specific expanded version

No metadata. No lifecycle. No sharing model. **Just enough to lift Deadzone out of the "1 hardcoded lib" state.**

### Rationale

- **Deliberately dumb so #52 (the long-term registry research) is free to redesign** without backwards-compat burden. The data structure is trivially convertible to whatever shape #52 picks.
- **Migration cost is bounded** at the current corpus size. The minute the corpus grows, the cost of switching schemas grows with it — but at 1-10 libs, conversion is a script.
- **Composable with the other decisions**: the 4-field schema is exactly what `cmd/scraper -lib X` and `cmd/scraper -config Y` need to do per-lib filtering and per-lib artifact production.

### Trace

- Designed in #51, merged in #54
- The long-term registry research lives in #52 (open, post-mvp)

### Holds at scale

⚠️ Explicitly **not at scale**. The single committed YAML file scales to maybe 100-500 libs before becoming unwieldy. By the time the corpus grows past that, #52 will have landed a real registry design (or we'll know enough about how the corpus actually grows in practice to decide what shape it should take). The tradeoff is intentional: ship something now, redesign later.

---

## 5. GitHub Releases for artifact distribution (#30)

### Context

Per-lib artifacts (decision 2) live as `.db` files in a local `artifacts/` directory. To make deadzone usable by anyone other than the maintainer, those artifacts have to travel between machines somehow. The git tree is the wrong place for binary blobs.

### Options considered

| Option | Approach | Verdict |
|---|---|---|
| **A** | Commit `.db` files directly to the git tree | Rejected — repo growth (~360 MB/year at 100 libs × 10 refreshes), clone cost, binary merge conflicts, no selective download |
| **B** | Git LFS | Rejected — adds infra dep, free quota is insufficient at scale, partial fetch is awkward |
| **C** | GitHub Releases with versioned tags (`packs-2026-04-10`, `packs-2026-04-17`, …) | Rejected — clutters the Releases page, complicates "what's latest", no real benefit at our refresh cadence |
| **D** | GitHub Releases with a single rolling tag, clobber semantics | **Selected** |
| **E** | External S3/R2/CDN | Rejected — adds ops burden, costs money, defeats the "single-binary local-first" goal |
| **F** | Self-hosted Pages site serving static `.db` files | Rejected — same as E, plus it's a worse CDN |

### Decision

**Single rolling GitHub Release tagged `packs`** with clobber semantics. Each upload uses `--clobber` to overwrite the previous asset for that lib. A committed `artifacts/manifest.yaml` is the source of truth about "what should be in `artifacts/` after a fresh clone" — and that file IS diffable in PRs, providing an audit trail.

Three CLI subcommands shipped via `cmd/packs`:

- **`upload`** — walks `artifacts/*.db`, computes SHA-256, uploads changed files via `gh release upload --clobber`, rewrites the manifest
- **`download`** — fetches every asset listed in the manifest, verifies SHA-256, drops files into `./artifacts`
- **`list`** — pretty-prints the manifest as a table

### Rationale

- **Repo stays small** — artifacts live outside the git pack
- **Clone is fast** — contributors who don't care about `artifacts/` don't pay the download cost
- **Selective download via `-lib <lib_id>`** — critical at scale, a user only needs the libs they care about
- **The committed manifest is the audit trail** — a reviewer looking at a PR sees a `manifest.yaml` diff with the old sha256 replaced by the new one, knows exactly which lib was refreshed and when
- **Rolling tag avoids tag proliferation** — at scale a versioned-tag approach would mean hundreds of tags per year. Stable download URLs are a feature, not a bug.
- **`gh` CLI handles auth** under the hood (`gh release upload packs <file> --clobber`). 2FA, token refresh, org scopes — all handled by `gh`. Reimplementing this in pure Go would just be a worse `gh`.
- **Public repo = public assets = anonymous download works.** No auth needed at runtime. (Private repo support is a future concern, not a current one.)

### Trace

- Designed in #30, merged in #59
- The first-time clone flow is now: `git clone → just build → just packs-download → just consolidate → just serve`

### Holds at scale

✅ At 3,000 libs the manifest is ~3,000 lines (manageable as a YAML diff in PR review), and selective download means the typical user pulls a small subset rather than the full corpus. Git LFS or git-native tracking wouldn't have scaled here. The rolling tag is bounded by GitHub's per-release asset limit (~hundreds of GB before friction), which is far above the projected corpus size.

---

## 6. Reasoning-mode suppression in the agent path (#60)

### Context

#57 shipped `scrape-via-agent` against the assumption that the LLM behind `DEADZONE_AGENT_ENDPOINT` is a "normal" instruction-following chat model — output goes into `choices[0].message.content`, end of story. That assumption holds for Qwen2.5, Llama 3.x, Mistral, and most pre-2026 models.

It does **not** hold for Qwen3+ and the wave of reasoning models that shipped in 2026. These models split their output across two fields:

```json
{
  "choices": [{"message": {
    "role": "assistant",
    "content": "ok",
    "reasoning_content": "Thinking Process: ..."  ← 268 tokens for a 1-token answer
  }}]
}
```

Empirical measurement against oMLX serving Qwen3.5-9B-MLX-4bit on 2026-04-11: **268 tokens of reasoning** for a 1-token "ok" ping. On real HTML extraction the multiplier is smaller but still significant (3-6× per URL). Enough to turn a 2-minute smoke test into a 10-minute one.

### Options considered

| Option | Approach | Verdict |
|---|---|---|
| **A** | Read `reasoning_content` and discard it (current behavior, slow) | Rejected — wastes tokens but is at least correct |
| **B** | Per-server admin config (e.g. oMLX admin panel → `chat_template_kwargs.enable_thinking=false`) | Rejected — couples deadzone behavior to per-server config, invisible to anyone reading the deadzone code, doesn't survive a fresh clone |
| **C** | Hardcode reasoning suppression in deadzone's chat completion request | **Selected** |
| **D** | Per-source YAML field for reasoning toggle | Rejected — premature, reasoning is uniformly wasted for HTML extraction |
| **E** | Switch to a non-reasoning model | Rejected — restricts user choice, doesn't solve the underlying portability problem |

### Decision

`internal/scraper/agent.go` sends **three reasoning-disable knobs in every chat completion request** via a `disableReasoning()` helper applied by both `Ping()` and `Extract()`:

- `chat_template_kwargs: {enable_thinking: false}` — Qwen3+, GLM-4-Reasoning (via oMLX, vLLM, Ollama, sglang)
- `reasoning_effort: "minimal"` — OpenAI o-series (o1, o3, o5)
- `enable_thinking: false` — DeepSeek-R1 family (top-level)

Servers that don't recognize a field silently ignore it (part of the OpenAI spec's permissive request handling), so this is **one code path** that handles every reasoning model deadzone might meet.

The response parser is unchanged — it still only reads `Choices[0].Message.Content` — so even if a server ignores all three disable hints and returns reasoning anyway, correctness still holds (the reasoning is silently dropped from the parsed string).

### Rationale

- **Always-on, no config**: reasoning is genuinely wasted for HTML→markdown extraction. The system prompt of `scrape-via-agent` already says "Output ONLY the extracted Markdown. No preamble, no commentary." Reasoning would only matter if deadzone ever grew a use case where the LLM has to make a non-trivial decision during extraction, and that's not in the cards.
- **Sending three fields is cheap and forward-compatible**: extra fields are ignored by servers that don't recognize them. One code path handles every reasoning model deadzone might meet, including future ones that haven't shipped yet (GPT-5-reasoning, Claude 4.7-reasoning, DeepSeek-R2, …).
- **Empirically measured**: ping went from 268 completion tokens to 1 after the change. On dense HTML pages the ratio is smaller but still 3-6×.
- **Future-proof against the Anthropic Messages API**: when/if deadzone wires that path, it's a separate fetcher (different endpoint, different schema). Out of scope here.

### Trace

- Designed in #60 after the smoke test #58 surfaced the Qwen3.5 reasoning issue
- Merged in #61
- Validated empirically during #58: ping completion tokens went from 268 to 1

### Holds at scale

✅ Reasoning models will become the default in 2026-2027. Without this fix, every new reasoning model that ships becomes a 3-6× latency tax on the scrape path. The fix lives in deadzone's code so contributors don't have to rediscover and re-fix it per server.

---

## 7. Strict-by-default code-block verifier (#27, revisit in #64)

### Context

LLMs hallucinate. When the agent path asks an LLM to extract markdown from HTML, the LLM might invent a code block that looks plausible but doesn't exist in the source. Indexing a hallucinated code example as if it were real documentation is the most dangerous failure mode of the entire pipeline — the user copies it, runs it, and it doesn't compile.

### Options considered

| Option | Approach | Verdict |
|---|---|---|
| **A** | No verification, trust the LLM | Rejected — too risky for a documentation index |
| **B** | Strict byte-substring check (`strings.Contains(source, block)`) | **Selected for v1** |
| **C** | Whitespace-normalized comparison | **Researching for v2 (#64)** |
| **D** | Token-overlap threshold (>X% of tokens in source) | Researching for v2 |
| **E** | Embedding similarity between block and source neighborhood | Researching for v2 |
| **F** | Per-source opt-out flag in YAML | Rejected — escape hatch with no protection |

### Decision

**Strict byte-substring check** in v1: every fenced code block in the LLM output must appear as a literal substring of the source content. Failure → drop the doc, log `scraper.agent_verification_failed`, continue with the next URL.

The strict-by-default test (`TestVerifyCodeBlocks_StrictWhitespace`) intentionally validates that "4-space-indented source vs tab-indented md → reject", which is exactly the failure mode we expected to revisit later.

### Rationale

- **Zero hallucination tolerance** for code blocks in v1. Better to lose a doc than to ship a fabricated example.
- **The strict test is explicit**: it documents the v1 behavior and forces any future change to be a deliberate decision, not a regression.
- **Soft-fail on rejection**: dropping the doc is a soft skip, the rest of the lib's URLs still get processed. Not a hard abort.

### Trace

- Designed in #27, merged in #57
- **Empirically over-strict**: smoke test #58 measured ~66% verification failure rate on FastAPI (mkdocs-material) tutorial pages
- Revisit tracked in #64 (research issue, post-mvp)

### Holds at scale

⚠️ **Mixed**. The strict check correctly catches hallucinations but rejects valid extractions where the LLM only normalized whitespace or unwrapped HTML `<span>` syntax-highlighting. At 3,000 libs the rejection rate is going to be the dominant data-quality concern. #64 is the research issue for picking the right loosening strategy without giving up too much hallucination protection.

The fact that the v1 decision is "wrong" in retrospect is **not a failure** — it's the smoke test working as designed. Without #58, we'd have shipped #64's design without empirical evidence that the strict check is too strict in practice.

---

## 8. Embedder: hugot + MiniLM-L6 for v1 (#19/#20)

### Context

Deadzone needs an embedder that:
- Runs in pure Go (no CGO)
- Is small enough to ship in a single binary or download on first use
- Is deterministic (same input → same output)
- Has cross-platform support (macOS arm64/amd64, Linux amd64/arm64)

### Options considered

| Option | Approach | Verdict |
|---|---|---|
| **A** | Call out to OpenAI / Cohere / etc. | Rejected — defeats the local-first goal, requires API key, costs money |
| **B** | Bundled ONNX model via `onnxruntime-go` | Rejected — onnxruntime requires CGO |
| **C** | hugot + GoMLX backend running ONNX models | **Selected** |
| **D** | Hand-written transformer in pure Go | Rejected — wheel reinvention, slow dev |
| **E** | Bundled Rust binary via FFI | Rejected — adds an FFI layer, fights the single-binary goal |

### Decision

**hugot** (`github.com/knights-analytics/hugot`) with the **GoMLX** backend, running `sentence-transformers/all-MiniLM-L6-v2`. 384-dim vectors, 512-token context window, English-only, ~90 MB on-disk model.

The embedder is pluggable via the `Embedder` interface in `internal/embed/`, so future swaps don't require call-site changes. The scraper embeds each doc at index time; the server embeds each query at query time. The same embedder must be used at both ends (enforced by `db.Meta` cross-check at DB open time).

### Rationale

- **Pure Go end-to-end**: hugot uses GoMLX (also pure Go) under the hood. No CGO, no FFI, no native dependencies.
- **Smallest fast English model that runs on pure-Go GoMLX** — that was the right call to ship, not necessarily the right call to scale.
- **Pluggable interface** lets us swap models later without changing call sites. The pluggability also enabled the `MockEmbedder` used in early tests.
- **Meta consistency check** prevents accidental DB corruption when the embedder changes — the DB metadata records the embedder kind, dimension, and model version, and the server refuses to open a DB indexed with a different embedder. See `db.Meta` and `ErrEmbedderMismatch`.

### Trace

- Designed in #2 (parent), implemented across #18 (meta consistency), #19 (hugot integration), #20 (semantic acceptance test)
- Open follow-up: #50 (embedder model choice research) — the v1 model is showing its age
- Open follow-up: #62 (hugot panics on >512-token input instead of truncating) — a real bug in v1 that surfaced in smoke test #58, in 0.1 milestone

### Holds at scale

⚠️ Conditional. The 512-token cap, English-only training, and prose-not-code training are all v1 choices that were correct to ship but won't hold at the target corpus size or content mix. #50 is the research issue for the long-term replacement (BGE-M3, E5, mpnet, Nomic, etc.). The architecture (pluggable interface + meta consistency) makes the swap mechanically straightforward; the cost is the corpus rebuild.

---

## 9. Schema versioning pattern (#55)

### Context

Deadzone's database schema was defined at v1 by `db.Open` creating tables on first run. There was no version field, no migration path, no way to detect a database created by a binary that doesn't know about the new schema.

When #44 (the `libs` vector table) was being designed, it became clear that adding a table to the schema would silently break old binaries — they'd open the DB, query the missing `libs` table, and fail at runtime with an unhelpful "no such table" error.

### Options considered

| Option | Approach | Verdict |
|---|---|---|
| **A** | Ignore the problem, hope for the best | Rejected — silent failure mode |
| **B** | Migration framework (golang-migrate, goose, etc.) | Rejected — heavy dep, premature |
| **C** | Hand-rolled `schema_version` integer in `meta` table + check on open | **Selected** |
| **D** | Version-stamp the file with a magic number prefix | Rejected — fights Turso's file format |

### Decision

**`db.CurrentSchemaVersion` constant** (currently `2`) recorded in the `meta` table at create time and cross-checked on every open. `db.Open` and `db.OpenArtifact` both reject DBs whose schema version doesn't match, surfacing `ErrSchemaMismatch` with a clear message.

When adding a table or making a breaking schema change, bump the constant in the same commit and document the migration step in the PR body.

### Rationale

- **Trivial to implement** (one constant, one column, one check at open time)
- **Fail fast on mismatch** rather than silent runtime errors deeper in the query path
- **Reusable for future schema bumps** — the pattern is established and can grow as schema migrations become real
- **No new dep** — uses existing infrastructure

### Trace

- Introduced in #55 alongside the `libs` table (#44 implementation)
- Extended by #28's `OpenArtifact` to validate per-artifact DBs against the same constant
- Will need to bump again for #29 (`meta_libs` table for skip-unchanged consolidation)

### Holds at scale

✅ The pattern is independent of corpus size. It's a property of the binary-vs-DB compatibility check, which runs once at startup regardless of how many libs are in the DB.

---

## Cross-cutting principles

The decisions above interact in ways that are worth calling out explicitly:

### The "single primitive" principle

Both `search_docs` and `search_libraries` use the same `vector_distance_cos` query against an `F32_BLOB(384)` column. This means:

- One vector index implementation (when #45 lands) speeds up both searches
- One embedder family (when #50 lands a replacement) re-embeds both `docs` and `libs` rows
- Tests for the `docs` table behavior also test the `libs` table behavior, modulo column names

This was not an explicit decision — it fell out of decisions 3 and 8 — but it's worth preserving in future architecture changes.

### The "never per-source code" principle

`libraries_sources.yaml` is *configuration*, not code. The two source kinds in decision 1 are *enumerated*, not *registered*. There is no plugin interface, no per-source Go file, no `internal/scraper/sources/terraform/` directory. Adding a library is a YAML edit, never a code change.

This principle is what keeps the code surface bounded as the corpus grows from 1 to 33k libs. The day we need a third kind (`json-api` per #1), we add it to the enumerated set, and every existing source keeps working.

### The "scrape once, derive everything" principle

Scraping is the expensive operation (LLM calls, network fetches, embedder runs). Once a doc is in an artifact, it should never be re-scraped without an explicit signal. Decisions 2 (per-lib artifacts), 5 (rolling release distribution), and the future #29 (skip-unchanged consolidate) all serve this principle.

The corollary: **the artifact format must stay stable across binary versions** until a schema bump (decision 9) is explicitly made. Otherwise users would have to re-scrape every time they upgrade.

### The "fail fast and loud" principle

Several decisions enforce this:
- `setupAgent` pings the LLM endpoint at startup, fails the entire scrape if unreachable
- `ErrAgentNotConfigured` blocks the scrape if env vars are missing for a `scrape-via-agent` source
- `ErrEmbedderMismatch` and `ErrSchemaMismatch` block DB open on incompatible meta
- `OpenArtifact`'s `ErrArtifactLibIDMissing` / `ErrArtifactLibIDMismatch` block consolidation on bad artifacts

The alternative (silent fallback) would mean shipping a half-broken scrape and discovering it weeks later in a search result. We prefer the loud error.

---

## Open research (deferred to individual issues)

These are tracked separately and don't gate the v0 architecture:

- **#1** — JSON-based source kind for structured-API doc sites (the third candidate kind)
- **#29** — Skip-unchanged consolidation via per-artifact checksums (in 0.1 milestone)
- **#45** — Vector index for search at scale (the linear scan won't survive past ~100k vectors)
- **#46** — Source discovery automation (replacing hand-curated YAML URL lists)
- **#47** — Freshness detection and refresh triggers (per-source change signals)
- **#49** — Chunking strategy beyond H2 split (the current ParseMarkdown is too coarse for some docs)
- **#50** — Embedder model choice (replacing MiniLM-L6 with something more capable at scale)
- **#52** — Library registry long-term (replacing the deliberately-dumb YAML from decision 4)
- **#53** — Batch scrape pipeline via GitHub Actions matrix (designed, all gates cleared, ready to implement)
- **#62** — Hugot panics on >512-token input (data-loss bug, in 0.1 milestone)
- **#63** — Scrape-via-agent error handling hardening (in 0.1 milestone)
- **#64** — Scrape-via-agent extraction quality on dense doc sites (truncation + verifier loosening, research)
- **#65** — Lib `doc_count` atomicity (cosmetic, in 0.1 milestone)
- **#66** — CI/CD release binaries on tag push (in 0.1 milestone)

The `0.1` milestone subset (#29, #62, #63, #65, #66) is the polish that turns the v0 architecture into a tagged release. The research issues are not blockers for any specific tag — they're the long-term agenda for keeping the architecture viable as the corpus grows past v1's design constraints.

---

## Timeline: the 2026-04-11 sprint

The v0 architecture was designed across several days of planning, then **shipped in a single afternoon** on 2026-04-11:

```
14:30 ─ Triage workflow (#5fcd5633) — automate the inbox
                     ↓
15:36 ─ #54 merged ─ libraries_sources.yaml (#51) — break the hardcoded-URL constraint
                     ↓
16:14 ─ #55 merged ─ search_libraries (#44) + libs vector table + schema versioning
                     ↓
16:14 ─ #56 merged ─ per-lib artifacts (#28) + cmd/consolidate
                     ↓
17:06 ─ #57 merged ─ scrape-via-agent (#27) + agent.go + preprocess.go
                     ↓
19:16 ─ #59 merged ─ packs distribution (#30) + cmd/packs upload/download/list
                     ↓
20:25 ─ #61 merged ─ disable reasoning (#60) — 268 → 1 token reduction
                     ↓
21:00 ─ smoke test #58 run end-to-end against FastAPI via Qwen3.5-4B-MLX-4bit
21:14 ─ consolidate works, search_libraries+search_docs validated
                     ↓
       ─ 4 follow-up bugs filed (#62, #63, #64, #65) from smoke test
       ─ #58 closed as PASS, mvp milestone closed
       ─ this ADR file written
```

This is unusual cadence — six PRs in one day is not the project's normal velocity. The reason it worked is that **the architectural decisions were locked in advance** through the issues that became this document. The PRs were execution against a settled design, not exploration.

The smoke test #58 was the validation gate that turned "MVP code shipped" into "MVP works". It surfaced four real bugs (#62, #63, #64, #65) that the unit tests had missed, validating the principle that **integration tests against real corpora are necessary**, not just nice-to-have.

---

## See also

- **[`context7-analysis.md`](context7-analysis.md)** — the upstream system whose pipeline shape this design draws on
- **[`tursogo-migration.md`](tursogo-migration.md)** — the storage-layer decision that shaped half the pipeline
- **`README.md`** — the user-facing description of the project
- **`CLAUDE.md`** — the agent-facing context and conventions
- **Closed mvp milestone**: [`https://github.com/laradji/deadzone/milestone/1`](https://github.com/laradji/deadzone/milestone/1) — the 7 issues that landed v0
- **Open 0.1 milestone**: [`https://github.com/laradji/deadzone/milestone/2`](https://github.com/laradji/deadzone/milestone/2) — the polish before tagging v0.1.0
- **Closed tracking issue #15** — superseded by this document
