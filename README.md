```
         _                _
      __| | ___  __ _  __| |_______  _ __   ___
     / _` |/ _ \/ _` |/ _` |_  / _ \| '_ \ / _ \
    | (_| |  __/ (_| | (_| |/ / (_) | | | |  __/
     \__,_|\___|\__,_|\__,_/___\___/|_| |_|\___|

    > semantic doc search. local file. no cloud. no key.
    > you ask in english. it answers in snippets.
```

> **Status.** `v0.2.0` shipped 2026-04-17. Vector search wired end-to-end. MCP over stdio. One binary, three platforms, zero telemetry. Milestone `0.3` in flight — see the [roadmap](https://github.com/laradji/deadzone/milestones).
> The scraper is still the messy half — [#64](https://github.com/laradji/deadzone/issues/64) is honest about it.

---

## The pitch, in one paragraph

Your AI client says `"how do I register a tool?"`. The doc says `AddTool`. A grep-based index shrugs; a vector index doesn't. Deadzone is the vector index — `nomic-embed-text-v1.5` over Turso's native cosine distance, wrapped in a Go binary that speaks MCP over stdio and keeps every byte on your laptop. It is, roughly, [Context7](https://github.com/upstash/context7) with the internet turned off.

---

## Rules of the deadzone

1. **One binary.** `deadzone`. Subcommands for everything. No `pip install`, no `npm i`, no `docker compose up`.
2. **The index never leaves.** Local Turso file. No account. No API key. No egress on the hot path.
3. **Natural language first.** Embeddings over cosine. `FTS5` is not invited.
4. **The binary is the version.** The DB is pinned to the binary. Upgrade the binary, the DB follows; don't, and it won't.
5. **Fail loudly or not at all.** `DEADZONE_DB_OFFLINE=1` refuses to guess. Verification failures in the scraper drop the doc, not the run.

---

## Install (pick one; they all converge on the same binary)

```sh
# macOS Apple Silicon — the one-liner
brew install laradji/deadzone/deadzone

# Linux — self-mounting AppImage (amd64 | arm64)
VERSION=v0.2.0 ARCH=amd64
curl -L -O "https://github.com/laradji/deadzone/releases/download/${VERSION}/deadzone_${VERSION}_linux_${ARCH}.AppImage"
chmod +x "deadzone_${VERSION}_linux_${ARCH}.AppImage"

# Anything else — tarball + quarantine strip on macOS
curl -L "https://github.com/laradji/deadzone/releases/download/${VERSION}/deadzone_${VERSION}_darwin_arm64.tar.gz" | tar xz
xattr -d com.apple.quarantine ./deadzone   # macOS only, until notarization lands
```

Windows is blocked upstream — no `libtokenizers.a`. Use WSL.

**Verify checksums** (optional but cheap):

```sh
curl -L -O "https://github.com/laradji/deadzone/releases/download/${VERSION}/deadzone_${VERSION}_checksums.txt"
sha256sum  --ignore-missing -c "deadzone_${VERSION}_checksums.txt"   # Linux
shasum -a 256 --ignore-missing -c "deadzone_${VERSION}_checksums.txt"   # macOS
```

**AppImage needs FUSE v2.** Most desktops ship it; minimal servers don't. If you get `dlopen(): libfuse.so.2`, either `apt-get install libfuse2` (or `dnf install fuse-libs`) or pass `--appimage-extract-and-run` to bypass FUSE entirely.

---

## Run

```sh
./deadzone server
```

That's the quick-start. On first launch it fetches `deadzone.db` matched to this binary's version, SHA256-verifies, caches it under the platform data dir, and serves. Second launch onwards: zero network. Upgrade the binary and the DB re-fetches on next launch; don't, and the cache is served forever.

MCP client wire-up:

```json
{
  "mcpServers": {
    "deadzone": {
      "type": "stdio",
      "command": "/path/to/deadzone",
      "args": ["server"]
    }
  }
}
```

Then, from the client:

```
search_libraries("terraform aws")                 → ranked (lib_id, version) pairs
search_docs("creating an s3 bucket", lib_id=...)  → snippets, token-budgeted
```

---

## The two tools

```
┌─────────────────────────────────────────────────────────────────────┐
│  search_libraries(name, limit?) → []LibraryHit                      │
│  ─────────────────────────────────────────────                      │
│  free text   ──►  vector match against the `libs` table             │
│                   ──► [{lib_id, version, doc_count, match_score}]   │
├─────────────────────────────────────────────────────────────────────┤
│  search_docs(query, lib_id?, version?, tokens?) → []Snippet         │
│  ──────────────────────────────────────────────────                 │
│  natural  ──► 768-dim embed ──► cosine over docs                    │
│  language                      ──► token-budgeted snippets back     │
└─────────────────────────────────────────────────────────────────────┘
```

| Arg         | Shape                   | Notes                                                                 |
|-------------|-------------------------|-----------------------------------------------------------------------|
| `query`     | string                  | Matched semantically. Don't write keywords; write what you want.      |
| `lib_id`    | `/org/project`          | Optional filter. Grab one from `search_libraries`.                    |
| `version`   | `"1.14"` or similar     | Optional pin; **requires** `lib_id`. `version` alone is rejected.     |
| `tokens`    | int                     | Response budget. Default `5000`, min `1000`, ≈ 4 chars/token.         |
| `limit`     | int                     | On `search_libraries` — max results. Default `10`, max `50`.          |
| `name`      | string                  | Free text on `search_libraries`. Empty returns the most-indexed libs. |

---

## Under the hood

```
  deadzone server
       │
       ▼
  ┌──────────────┐   stdio JSON-RPC       ┌───────────────┐
  │  MCP client  │ ─────────────────────► │   handler     │
  └──────────────┘                        └──────┬────────┘
                                                 │
                              ┌──────────────────┴──────────────────┐
                              ▼                                     ▼
                     ┌────────────────┐                   ┌──────────────────┐
                     │   embedder     │                   │   Turso (local)  │
                     │  hugot + ORT   │                   │  F32_BLOB(768)   │
                     │  nomic v1.5    │                   │  vector_distance │
                     └────────┬───────┘                   └──────────────────┘
                              │  768-dim                             ▲
                              └──────────────  query vector  ────────┘
```

| Layer      | Choice                                                                        |
|------------|-------------------------------------------------------------------------------|
| Language   | Go 1.26.2, pinned via [`mise`](https://mise.jdx.dev)                           |
| Storage    | [Turso](https://turso.tech) local file — native `F32_BLOB(N)` + `vector_distance_cos` |
| Driver     | [`tursogo`](https://pkg.go.dev/turso.tech/database/tursogo) — **CGO-free** via [`purego`](https://github.com/ebitengine/purego) |
| Embedder   | [`hugot`](https://github.com/knights-analytics/hugot) → `nomic-ai/nomic-embed-text-v1.5`, 768-dim, 8192-token ctx (int8 quantized) |
| Runtime    | ONNX Runtime — binary CGO-linked at build time; `libonnxruntime` auto-fetched + SHA256-verified on first launch |
| Protocol   | [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) over stdio |

The binary itself is CGO-linked (hugot ORT backend + static `libtokenizers.a`). At **runtime** the only native surface is `libonnxruntime`, loaded via `dlopen` after a SHA256-verified auto-download. Everything else — Go stdlib, `tursogo`, the model weights — is either statically linked or fetched on first launch against a pinned hash. No system installs. No `sudo`. If a download drifts from its pinned hash, the run aborts; there is no fallback to an unverified fetch.

Escape hatches for air-gapped boxes:

| Env var                   | Effect                                                      |
|---------------------------|-------------------------------------------------------------|
| `DEADZONE_ORT_LIB_PATH`   | Hand-positioned `libonnxruntime` path. Skips the download.  |
| `DEADZONE_ORT_CACHE`      | Override the ORT library cache dir.                         |
| `DEADZONE_HUGOT_CACHE`    | Override the model-weights cache dir.                       |
| `DEADZONE_DB_CACHE`       | Override the `deadzone.db` cache dir.                       |
| `DEADZONE_DB_OFFLINE=1`   | Refuse any network call. Fails loudly if nothing is cached. |

Default cache paths per platform:

| Platform | `deadzone.db` lives at                                           |
|----------|------------------------------------------------------------------|
| macOS    | `~/Library/Application Support/deadzone/deadzone.db`              |
| Linux    | `$XDG_DATA_HOME/deadzone/deadzone.db` (else `~/.local/share/...`) |
| Windows  | `%LOCALAPPDATA%\deadzone\deadzone.db`                             |

A sibling `deadzone.db.release` text file records the tag the cache was fetched from. Startup compares it against the binary's compiled-in version: match → zero-network; differs → fetch and atomic-swap; dev build → fall back to `/releases/latest` with a `server.db_version_dev_fallback` WARN.

---

## Add a library

Contributor path. End users don't touch this — they just get what ships in `deadzone.db`.

**Not editing YAML yourself?** Open an issue via the [New issue](https://github.com/laradji/deadzone/issues/new/choose) page and pick **Add a library** or **Refresh a library**. The template collects exactly what a registry entry needs.

**Editing YAML yourself?** Append to [`libraries_sources.yaml`](libraries_sources.yaml):

```yaml
libraries:
  # Single-version lib — no `versions` key, urls used as-is.
  - lib_id: /modelcontextprotocol/go-sdk
    kind: github-md
    urls:
      - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/main/README.md
      - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/main/docs/quick_start.md

  # Multi-version lib — `versions` expands into one effective lib_id
  # per version (/org/project/1.4, /org/project/1.5, …). {ref} is
  # substituted from each version's ref: field.
  - lib_id: /modelcontextprotocol/go-sdk
    kind: github-md
    versions:
      "1.4": { ref: v1.4.1 }
      "1.5": { ref: v1.5.0 }
    urls:
      - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/{ref}/README.md
      - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/{ref}/docs/getting-started.md
```

| Field                | Req | Purpose                                                                                                          |
|----------------------|-----|------------------------------------------------------------------------------------------------------------------|
| `lib_id`             | yes | Canonical `/org/project` identifier (matches `db.docs.lib_id`).                                                  |
| `kind`               | yes | `github-md` (raw markdown), `github-rst` (raw reStructuredText), or `scrape-via-agent` (HTML/text via LLM).      |
| `urls`               | yes | Doc URL list with an optional `{ref}` placeholder.                                                               |
| `versions`           | no  | `{"1.4": {ref: v1.4.1, urls: [...]}, "1.5": {ref: v1.5.0}, …}` — user-facing identifiers prefer `major.minor`.   |
| `ref`                | no  | Git tag or commit SHA substituted into `{ref}`. Per-version `ref:` overrides top-level.                          |
| `versions[v].urls`   | no  | Per-version URL list — replaces baseline wholesale. Use for structurally divergent versions.                     |

Pre-1.0: no Go editing, no recompile. Just edit YAML and re-scrape.

---

## Scrape-via-agent (experimental)

> ⚠️ **The messy half.** Works today for non-markdown sources (Terraform providers, mkdocs, GitBook, …), but the LLM→verifier loop is sensitive to input truncation (48 KiB cap), HTML→markdown skill, and verbatim-code matching. Real-world hit rate on dense doc sites ≈ 50%/URL — see [#64](https://github.com/laradji/deadzone/issues/64). **Prefer `github-md` whenever the project ships committed markdown.**

Bring your own LLM runtime — [Ollama](https://ollama.ai), [llama.cpp](https://github.com/ggerganov/llama.cpp/tree/master/examples/server), [vLLM](https://github.com/vllm-project/vllm), LocalAI, LM Studio, Groq, OpenAI, anything that speaks `POST /v1/chat/completions`:

```sh
export DEADZONE_AGENT_ENDPOINT=http://localhost:11434/v1
export DEADZONE_AGENT_ENDPOINT_MODEL=qwen2.5:7b
export DEADZONE_AGENT_ENDPOINT_API_KEY=sk-...   # optional
```

Then add a `kind: scrape-via-agent` entry to `libraries_sources.yaml` with a list of page URLs. The downstream pipeline (parse → chunk → embed → store) is **identical** to `github-md`; only the markdown source changes.

**Guardrails.** Every fenced code block in the LLM output is verified verbatim against the source — invented examples drop the doc (`scraper.agent_verification_failed`), not the run. Missing/unreachable endpoint aborts at startup; no silent fallback.

---

## Local pipeline (contributors)

```sh
# 1. Install the pinned toolchain (Go 1.26.2 + just)
mise install

# 2. Fetch libtokenizers.a for your platform into ./lib/
just fetch-tokenizers

# 3. Build, scrape, consolidate, serve
just build
just scrape                       # all libs — one artifact folder per lib
just scrape /hashicorp/terraform  # one base lib, every version
just scrape /hashicorp/terraform/1.14   # one exact version
just consolidate                  # merge artifacts/*/artifact.db → deadzone.db
just serve                        # MCP server against deadzone.db
```

`just` with no args lists every recipe. Each scrape rewrites `artifacts/<slug>/artifact.db` + `state.yaml` in place; `consolidate` merges all artifact DBs atomically under `deadzone.db`. Per-lib folders are gitignored; the committed [`artifacts/manifest.yaml`](artifacts/manifest.yaml) records release history only.

**Full registry via CI.** `gh workflow run scrape-pack.yml -f tag=vX.Y.Z` fans out the matrix, consolidates, and uploads `deadzone.db` to the tagged release. Omit `-f tag=…` to stop at a consolidated-db cache.

---

## Release flow

Two-phase as of [#101](https://github.com/laradji/deadzone/issues/101) — CI ships binaries, operator ships the DB.

```sh
# 1. Regenerate deadzone.db from the committed scraper config.
just scrape && just consolidate

# 2. Tag + push. CI's release.yml builds the tarballs + AppImages + creates the release.
git tag v0.X.0 && git push --tags

# 3. Ship deadzone.db + deadzone.db.sha256 to the same release.
just dbrelease v0.X.0

# 4. Bump the Homebrew tap (until #130 lands the PAT-based auto-trigger).
gh workflow run update-package-channels.yml -f tag=v0.X.0

# 5. Commit artifacts/manifest.yaml so the release-history trace lands in git.
git add artifacts/manifest.yaml && git commit -m "release v0.X.0" && git push
```

---

## Logs

Structured JSON on **stderr** via `log/slog`. Stdout is reserved for MCP JSON-RPC on `deadzone server`.

| Subcommand      | Key events                                                                                                                      |
|-----------------|--------------------------------------------------------------------------------------------------------------------------------|
| `scrape`        | `scraper.start`, `scraper.lib_start`, `scraper.fetch` (per URL), `scraper.indexed`, `scraper.lib_done`, `scraper.done`. Errors: `scraper.fetch_failed`, `scraper.insert_failed`. Agent path adds `scraper.agent_configured`, `scraper.agent_ping_ok`, `scraper.agent_verification_failed`, `agent.input_truncated`. |
| `consolidate`   | `consolidate.start`, `consolidate.done` with `artifacts`, `docs_merged`, `libs_merged`, `duration_ms`.                          |
| `dbrelease`     | `dbrelease.start`, `packs.dbrelease.uploaded` (per asset), `dbrelease.done` with `sha256`, `size`, `lib_count`, `doc_count`.    |
| `server`        | `server.start` (embedder + `doc_count`), one `search_docs` per call (`lib_id`, `tokens`, `results`, `latency_ms`). Boot may emit `server.db_upgraded`, `server.db_version_dev_fallback` WARN, `server.db_tag_sidecar_write_failed` WARN. |

`-verbose` on any subcommand adds debug-level detail. On `server` it logs the raw `query` (off by default — queries may carry user data). On `scrape` it adds per-doc `scraper.doc_indexed`.

MCP client log paths: Claude Code on macOS writes to `~/Library/Logs/Claude/mcp-server-deadzone.log`; other clients vary.

---

## Roadmap & contributing

Issues: [`laradji/deadzone/issues`](https://github.com/laradji/deadzone/issues). Scope via [milestones](https://github.com/laradji/deadzone/milestones) (`0.1` shipped, `0.2` shipped, `0.3` in flight). Category via `feature` / `research` labels; priority via `P1` / `P2` / `P3`.

New library or refresh: use the [New issue](https://github.com/laradji/deadzone/issues/new/choose) page and pick the matching form.

---

## Why bother with vectors

Because `"how to register a tool"` should find the doc that says `AddTool`, and no FTS5 query will get you there without the human already knowing the answer. Embeddings-first retrieval is the point; everything else is plumbing.

Long-form: [`docs/research/context7-analysis.md`](docs/research/context7-analysis.md).

---

## License

[Apache License, Version 2.0](./LICENSE). Third-party attributions in [`NOTICE`](./NOTICE).

**One important asterisk.** Apache 2.0 covers the Deadzone source code, and only that. It does **not** cover the third-party documentation the scraper indexes — those docs belong to their original authors under their own licenses. Running `deadzone scrape` is subject to each source's ToS. A pre-built pack is bound by the original content's license, not Apache 2.0. Personal local indexing: fine. Public redistribution: do the homework first.
