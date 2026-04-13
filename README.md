# Deadzone

A Go-based [MCP](https://modelcontextprotocol.io) server that exposes semantic search over third-party library documentation, indexed locally with [Turso](https://turso.tech) vector storage.

> **Status:** early MVP. Vector search is wired end-to-end on a CGO-free
> [tursogo](https://github.com/tursodatabase/turso/tree/main/bindings/go)
> driver and a [hugot](https://github.com/knights-analytics/hugot) embedder
> running [`nomic-ai/nomic-embed-text-v1.5`](https://huggingface.co/nomic-ai/nomic-embed-text-v1.5)
> on the ONNX Runtime backend (CGO). Full
> [roadmap](https://github.com/laradji/deadzone/issues).

Deadzone is a self-hosted alternative to [Context7](https://github.com/upstash/context7) for users who want to keep their docs index on their own machine.

## Features

- **Self-hosted** — local file database, no cloud dependency, no API key
- **Single download** — one archive, four CLIs, ONNX Runtime auto-fetched on first run
- **Semantic search** — vector embeddings with cosine similarity via Turso's native vector support
- **MCP native** — stdio protocol, plugs directly into Claude Code, Cursor, and other MCP clients
- **Multi-library** — `/org/project` namespacing with first-class `lib_id` filtering
- **Token-budget aware** — trims response size to fit the caller's context window
- **Cross-platform** — prebuilt for macOS arm64, Linux amd64, and Linux arm64

## What it does

Deadzone exposes two MCP tools to clients (Claude Code, Cursor, etc.):

```
search_docs(query, lib_id?, topic?, tokens?) → []Snippet
```

- `query` — natural-language search query (matched semantically against the indexed docs)
- `lib_id` — optional `/org/project` filter (e.g. `/modelcontextprotocol/go-sdk`)
- `topic` — optional section filter (not yet implemented)
- `tokens` — response budget, default 5000, min 1000 (`~4 chars/token`)

```
search_libraries(name, limit?) → []LibraryHit
```

- `name` — free-text library name to resolve (e.g. `"terraform aws"`); empty returns the most-indexed libraries by `doc_count`
- `limit` — max results, default 10, max 50
- Each `LibraryHit` carries `lib_id`, `doc_count`, and a `match_score` in `[0, 1]` (1.0 = closest cosine match) so the LLM can pick a single canonical id or surface multiple candidates to the user.

`search_libraries` is the resolver step: a free-text query like `"react"` is matched against a dedicated `libs` vector table and returns ranked canonical `lib_id` values. Pass one of those into `search_docs` to get the actual snippets.

Documentation is fetched by a separate `scraper` CLI, embedded into vectors, and stored in a local Turso database file.

## Install

Pre-built binaries for **macOS Apple Silicon**, **Linux amd64**, and **Linux arm64** are published on the [Releases page](https://github.com/laradji/deadzone/releases). Windows is blocked upstream (no `libtokenizers.a`). If you want to build from source instead — most useful if you're contributing or running on an unsupported platform — skip to [Build from source](#build-from-source).

### Quick install

Pick the archive for your platform and extract it into the directory you want to run deadzone from:

```bash
VERSION=v0.1.0

# macOS Apple Silicon
curl -L "https://github.com/laradji/deadzone/releases/download/${VERSION}/deadzone_${VERSION}_darwin_arm64.tar.gz" | tar xz

# Linux amd64
curl -L "https://github.com/laradji/deadzone/releases/download/${VERSION}/deadzone_${VERSION}_linux_amd64.tar.gz" | tar xz

# Linux arm64
curl -L "https://github.com/laradji/deadzone/releases/download/${VERSION}/deadzone_${VERSION}_linux_arm64.tar.gz" | tar xz
```

Each archive extracts four binaries (`deadzone-server`, `deadzone-scraper`, `deadzone-consolidate`, `deadzone-packs`) plus `LICENSE`, `NOTICE`, and `README.md`.

### Verify checksums

```bash
curl -L -O "https://github.com/laradji/deadzone/releases/download/${VERSION}/deadzone_${VERSION}_checksums.txt"

# Linux
sha256sum --ignore-missing -c "deadzone_${VERSION}_checksums.txt"

# macOS
shasum -a 256 --ignore-missing -c "deadzone_${VERSION}_checksums.txt"
```

### macOS: clear the quarantine attribute

The 0.1.x binaries are unsigned, so Gatekeeper blocks them on first launch. Strip the quarantine xattr once, after extracting the archive:

```bash
xattr -d com.apple.quarantine deadzone-*
```

This workaround goes away once notarization lands.

### Four binaries, briefly

End users usually only touch the first three. `deadzone-scraper` is for contributors maintaining [`libraries_sources.yaml`](libraries_sources.yaml).

| Binary | What it's for |
|---|---|
| `deadzone-server` | MCP stdio server — what your AI client talks to |
| `deadzone-packs` | Pulls (and for contributors, pushes) per-lib artifacts from the rolling GitHub Release |
| `deadzone-consolidate` | Merges per-lib artifacts into a single `deadzone.db` |
| `deadzone-scraper` | Re-scrapes a library from its configured sources |

### Runtime dependencies

Deadzone follows the same pattern for every native runtime dependency: **no system installs, nothing bundled in the binary, nothing pulled at build time except what links statically**. Instead, each shared library is fetched on first use, SHA256-verified against a pinned manifest, cached in the user-cache dir, and loaded via `purego` (`tursogo`) or `dlopen` (ONNX Runtime) at runtime. Subsequent runs reuse the cache; second-launch startup is instant.

This keeps the install flow to "download the tarball, extract, run" across macOS arm64, Linux amd64, and Linux arm64 without a package manager or a C toolchain. It also makes air-gapped installs easy: pre-populate the caches or point the escape-hatch env vars at hand-positioned libraries.

**What gets fetched on first launch of any deadzone binary:**

| Dependency | Size | Where it's cached | Escape-hatch env var |
|---|---|---|---|
| ONNX Runtime shared library (`libonnxruntime`) | ~33 MB | `$DEADZONE_ORT_CACHE` (defaults to `<user-cache>/deadzone/ort/`) | `DEADZONE_ORT_LIB_PATH` — point at a hand-positioned library to skip the download |
| `nomic-ai/nomic-embed-text-v1.5` ONNX weights (int8 quantized) | ~131 MB | `$DEADZONE_HUGOT_CACHE` (defaults to `<user-cache>/deadzone/models/`) | `DEADZONE_HUGOT_CACHE` — set before first launch to pre-position the model |

The platform `<user-cache>` resolves to `~/Library/Caches/` on macOS and `~/.cache/` on Linux (or `$XDG_CACHE_HOME` when set). Both downloads are pinned in the binary (ORT version in `internal/ort/ort.go`, model name in `internal/embed/hugot.go`) and verified with SHA256 before being moved into place — there's no fallback to an un-verified fetch.

**Linked at build time, not fetched:**

- **Go standard library** — pinned to Go 1.26.2 via `.mise.toml`.
- **`tursogo` (SQLite driver)** — pure Go via `purego`, no C toolchain needed.
- **`libtokenizers.a` (Rust-built, from `daulet/tokenizers` releases)** — downloaded per-platform by `just fetch-tokenizers` (or CI's `install-native-deps` action), **statically linked** into the binary. Users never see it.

The single CGO surface (hugot's ORT backend + `libtokenizers.a`) is the 2026-04-12 trade-off that unblocked #62 — see [`docs/research/embedder-choice.md`](docs/research/embedder-choice.md) and [`docs/research/ingestion-architecture.md`](docs/research/ingestion-architecture.md) decision 8 for the full reasoning.

### Hello-world pipeline

Two paths, pick one. The first is the fastest way to get a working `deadzone.db`; the second is what you want if you're tracking the rolling corpus between tagged releases.

#### One-curl path (prebuilt DB, tagged releases)

Starting with `v0.1.0`, every tagged release ships a prebuilt `deadzone.db` covering the libraries listed in [`libraries_sources.yaml`](libraries_sources.yaml). Grab the binary tarball, then the DB, and you're done:

```bash
VERSION=v0.1.0

curl -L -o deadzone.db \
  "https://github.com/laradji/deadzone/releases/download/${VERSION}/deadzone.db"
curl -L -o deadzone.db.sha256 \
  "https://github.com/laradji/deadzone/releases/download/${VERSION}/deadzone.db.sha256"

# Linux
sha256sum -c deadzone.db.sha256
# macOS
shasum -a 256 -c deadzone.db.sha256

./deadzone-server -db deadzone.db  # MCP stdio server
```

The aggregated `deadzone_${VERSION}_checksums.txt` release asset also includes the `deadzone.db` hash, so `sha256sum --ignore-missing -c deadzone_${VERSION}_checksums.txt` works against everything in one shot.

#### Three-step path (rolling corpus, latest per-lib artifacts)

Use this when you want the freshest per-lib artifacts (refreshed between tags), or when you've pulled a single library via `deadzone-packs download lib=/org/project` and want to re-consolidate:

```bash
mkdir -p artifacts
curl -L -o artifacts/manifest.yaml \
  https://raw.githubusercontent.com/laradji/deadzone/main/artifacts/manifest.yaml

./deadzone-packs download          # per-lib artifacts/*.db from the rolling release
./deadzone-consolidate             # artifacts/*.db → deadzone.db
./deadzone-server -db deadzone.db  # MCP stdio server
```

With the server running, point any MCP-capable client at it — see [Wire it into an MCP client](#wire-it-into-an-mcp-client) for the exact JSON snippet.

## Stack

| | |
|---|---|
| Language | Go 1.26.2 (pinned via [`mise`](https://mise.jdx.dev)) |
| Storage | [Turso](https://turso.tech) (local file) with native vector support (`F32_BLOB(N)` + `vector_distance_cos`, dim discovered from the embedder at first open) |
| Driver | [`turso.tech/database/tursogo`](https://pkg.go.dev/turso.tech/database/tursogo) — **CGO-free**, via [`purego`](https://github.com/ebitengine/purego) |
| Embeddings | [`hugot`](https://github.com/knights-analytics/hugot) running [`nomic-ai/nomic-embed-text-v1.5`](https://huggingface.co/nomic-ai/nomic-embed-text-v1.5) (768-dim, 8192-token context) on the ONNX Runtime backend — CGO-linked, `libonnxruntime` auto-fetched + SHA256-verified at first use |
| Protocol | [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) over stdio |

## Build from source

Contributor path — skip this section if you installed a pre-built binary from [Install](#install).

Go 1.26.2 and [`just`](https://just.systems) are pinned via [`.mise.toml`](.mise.toml) and intentionally not on the system `PATH`. The repo ships a `justfile` that wraps every Go invocation in `mise exec --`, so you don't need to remember the prefix:

```bash
# 1. Install the pinned toolchain — Go + just (one-time)
mise install

# 2. Fetch libtokenizers.a for your platform into ./lib/ (one-time, idempotent)
just fetch-tokenizers  # darwin-arm64, linux-amd64, linux-arm64

# 3. Build (CGO + -tags ORT, links ./lib/libtokenizers.a)
just build             # = mise exec -- go build -tags ORT ./...

# 4. Pull pre-built per-lib artifacts from the rolling GitHub Release
just packs-download    # = mise exec -- go run ./cmd/packs download -artifacts ./artifacts -manifest ./artifacts/manifest.yaml
# → reads artifacts/manifest.yaml and downloads every referenced .db
# → verifies sha256 on the way down; aborts loudly on mismatch

# 5. Merge the per-lib artifacts into the main deadzone.db
just consolidate       # = mise exec -- go run ./cmd/consolidate -db deadzone.db -artifacts ./artifacts

# 6. Run the MCP server against the consolidated DB
just serve             # = mise exec -- go run ./cmd/server -db deadzone.db
```

The `artifacts/*.db` files and `deadzone.db` are both gitignored — `artifacts/*.db` are the per-lib source-of-truth blobs (distributed via GitHub Releases, see [Refreshing a single library](#refreshing-a-single-library)) and `deadzone.db` is the derived view the server reads. The committed [`artifacts/manifest.yaml`](artifacts/manifest.yaml) is the audit trail mapping every lib to its current sha256. The server refuses to start if `deadzone.db` is missing and tells you to run `consolidate` first; it never auto-creates an empty file.

Run `just` (no args) to list every recipe. Override the DB path with positional args: `just consolidate foo.db` / `just serve foo.db`. If you'd rather call `go` directly, prefix every command with `mise exec --` so you pick up the pinned toolchain.

### Building release binaries

`just build` is a fast compile check (`go build ./...` — produces no output binaries). To produce the four named CLIs at the repo root with version info embedded, use `just build-release`:

```bash
# Local dev build — version/commit/date default from git describe + rev-parse + UTC now
just build-release
./deadzone-server -version
# → deadzone-server v0.1.0-2-gabc1234-dirty (abc1234, built 2026-04-12T12:00:00Z)

# Release build — CI sets VERSION/COMMIT/DATE explicitly from the workflow
VERSION=v0.1.0 COMMIT=$(git rev-parse --short HEAD) DATE=$(date -u +%FT%TZ) just build-release
./deadzone-server -version
# → deadzone-server v0.1.0 (abc1234, built 2026-04-12T12:00:00Z)
```

All four binaries accept `-version` (server, scraper, consolidate) or `version` as a subcommand (packs), which prints the banner and exits without touching the DB or embedder. The recipe compiles with `-trimpath -ldflags "-s -w -X main.version=… -X main.commit=… -X main.date=…"`, so absolute build-host paths never leak into the binary and the stripped output stays small despite the CGO ORT dependency.

### Refreshing a single library

The per-lib artifact layout means one library can be re-scraped, re-uploaded, and re-distributed without touching the others. The flow has four steps and is the same for both single-version libs and multi-version (`/facebook/react/v18`, `/facebook/react/v19`, …) entries:

```bash
# 1. Re-scrape locally (rebuilds exactly the matching artifacts/*.db files)
just scrape /facebook/react           # base — every versioned child
just scrape /facebook/react/v18       # one expanded version

# 2. Push the refreshed artifact(s) to the rolling GitHub Release.
#    Idempotent: artifacts whose sha256 already matches the manifest
#    are skipped, no `gh` calls made.
just packs-upload

# 3. Commit the manifest diff so reviewers can see exactly which libs
#    were refreshed (sha256 + indexed_at change in lockstep).
git add artifacts/manifest.yaml && git commit -m "refresh /facebook/react"
git push

# 4. Anyone else just runs `just packs-download && just consolidate` to
#    pick up the new artifact.
```

`packs upload` shells out to `gh release upload <tag> <file> --clobber` for each changed artifact, auto-creating the release on first use. The `gh` CLI handles auth via your existing `gh auth login` state — Deadzone does not handle GitHub tokens directly. Override the target repo with `-repo owner/name` if you're working from a fork.

Use `just packs-list` to print the current manifest contents as a table, or `just packs-download lib=/facebook/react` to fetch only one library's assets without pulling the whole corpus.

### Configuring which libraries to scrape

The scraper reads its registry from [`libraries_sources.yaml`](libraries_sources.yaml) at the project root. Each entry maps a `lib_id` to the documentation URLs the scraper should fetch:

```yaml
libraries:
  # Single-version lib — no `versions` key, urls used as-is.
  - lib_id: /modelcontextprotocol/go-sdk
    kind: github-md
    urls:
      - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/main/README.md
      - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/main/docs/quick_start.md

  # Multi-version lib — `versions` expands `{version}` in each URL,
  # producing one effective lib_id per version (`/facebook/react/v18`,
  # `/facebook/react/v19`, …) — matches Context7's `/org/project/version`
  # convention.
  - lib_id: /facebook/react
    kind: github-md
    versions: [v18, v19]
    urls:
      - https://raw.githubusercontent.com/facebook/react/{version}/README.md
      - https://raw.githubusercontent.com/facebook/react/{version}/docs/getting-started.md
```

| Field | Required | Purpose |
|---|---|---|
| `lib_id` | yes | canonical `/org/project` identifier (matches `db.docs.lib_id`) |
| `kind` | yes | source kind discriminator — `github-md` for raw markdown, `scrape-via-agent` for HTML/text via an LLM (see [Scraping non-trivial doc sources](#scraping-non-trivial-doc-sources-scrape-via-agent)) |
| `urls` | yes | list of doc URLs (with optional `{version}` placeholder) |
| `versions` | no | list of version tags; expands `{version}` in `urls` and produces one effective `lib_id` per version |

Adding a new library means adding a YAML entry — no Go editing, no recompile.

The scraper accepts a few flags for working with the registry and the artifact directory:

```bash
# Use a non-default registry path
mise exec -- go run ./cmd/scraper -artifacts ./artifacts -config /path/to/libraries_sources.yaml

# Use a non-default artifacts directory
mise exec -- go run ./cmd/scraper -artifacts /var/cache/deadzone/artifacts

# Scrape every configured version of one base lib
mise exec -- go run ./cmd/scraper -artifacts ./artifacts -lib /facebook/react

# Scrape only one specific versioned lib
mise exec -- go run ./cmd/scraper -artifacts ./artifacts -lib /facebook/react/v18
```

`-lib` matches at two levels: a base `lib_id` selects every expanded version of that base; a fully versioned `lib_id` selects exactly one expanded entry. Omitting `-lib` scrapes everything in the registry. Each entry produces (or replaces) one `artifacts/<lib_id>.db` file — the leading `/` is stripped and the remaining `/` characters become `_`, so `/facebook/react/v18` lands at `artifacts/facebook_react_v18.db`.

### Scraping non-trivial doc sources (`scrape-via-agent`)

The `github-md` kind only works on libraries that publish raw markdown on GitHub. For everything else — Terraform providers (HTML), React (`react.dev`), mkdocs/docusaurus/vitepress sites, GitBook, ReadTheDocs — Deadzone supports a second source kind, `scrape-via-agent`, that delegates **content → clean markdown** extraction to any OpenAI-compatible chat completions endpoint.

Deadzone does **not** host an LLM. You bring your own runtime — [Ollama](https://ollama.ai), [llama.cpp server](https://github.com/ggerganov/llama.cpp/tree/master/examples/server), [vLLM](https://github.com/vllm-project/vllm), [LocalAI](https://localai.io), [LM Studio](https://lmstudio.ai), [Groq](https://groq.com), OpenAI itself, anything that speaks `POST /v1/chat/completions` — and point Deadzone at the endpoint via three environment variables:

```bash
# Required
export DEADZONE_AGENT_ENDPOINT=http://localhost:11434/v1
export DEADZONE_AGENT_ENDPOINT_MODEL=qwen2.5:7b

# Optional — only set if your endpoint requires auth
export DEADZONE_AGENT_ENDPOINT_API_KEY=sk-...
```

Then add an entry with `kind: scrape-via-agent` to `libraries_sources.yaml`:

```yaml
libraries:
  - lib_id: /hashicorp/terraform-provider-aws
    kind: scrape-via-agent
    urls:
      - https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/s3_bucket
      - https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_role
      - https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/lambda_function
```

The downstream pipeline (`ParseMarkdown` → chunk → embed → store) is **identical** for both kinds. The only thing that changes is where the markdown comes from: `github-md` reads it directly from a `raw.githubusercontent.com` URL, `scrape-via-agent` fetches the page, hands it to the LLM, and indexes whatever clean markdown comes back.

**Startup contract.** If `libraries_sources.yaml` contains any `scrape-via-agent` source, the scraper resolves the agent config from env, pings the endpoint with a trivial completion, and aborts the run with a clear error if anything is missing or unreachable. There is no silent fallback — a misconfigured endpoint fails the run before any URL is processed.

**Hallucination protection.** Every fenced code block in the LLM's output is verified to appear verbatim in the source content. If the model invents a code example, the doc is dropped (`scraper.agent_verification_failed` in the log) and the rest of the URLs in that source still get processed. Prose hallucination is still possible — this catches the most dangerous failure mode but is not a complete defense.

**Input budget.** Inputs longer than ~48 KiB are truncated with a single `agent.input_truncated` warning. Smart chunking is a planned follow-up.

**Supported content types in v1.**

| Content type | Status |
|---|---|
| `text/html`, `application/xhtml+xml` | supported |
| `text/markdown`, `text/x-markdown` | supported |
| `text/plain` | supported |
| `application/pdf` | reserved — clear error, planned follow-up |
| anything else | clear `unsupported content type` error |

> **First-run model download.** The first `just scrape` or `just serve` invocation downloads the MiniLM-L6-v2 ONNX weights (~90 MB) into the platform user-cache directory under `deadzone/models/`:
>
> - Linux: `$XDG_CACHE_HOME/deadzone/models` (or `~/.cache/deadzone/models`)
> - macOS: `~/Library/Caches/deadzone/models`
> - Windows: `%LOCALAPPDATA%\deadzone\models`
>
> Subsequent runs reuse the on-disk model. Set `DEADZONE_HUGOT_CACHE` to override the location (used by tests and CI to share a workspace-local cache).

### Wire it into an MCP client

Add to your client's MCP config (Claude Code, Cursor, etc.):

```json
{
  "mcpServers": {
    "deadzone": {
      "type": "stdio",
      "command": "/path/to/deadzone-server",
      "args": ["-db", "/path/to/deadzone.db"]
    }
  }
}
```

Then call the `search_docs` or `search_libraries` tool from the client.

## Layout

```
deadzone/
├── cmd/
│   ├── server/        # MCP server entrypoint (search_docs / search_libraries)
│   ├── scraper/       # CLI: fetch, embed & write per-lib artifacts
│   ├── consolidate/   # CLI: merge per-lib artifacts into the main DB
│   └── packs/         # CLI: upload/download per-lib artifacts via GitHub Releases
├── internal/
│   ├── db/            # Turso schema, vector queries, consolidation helper
│   ├── embed/         # Embedder interface + hugot/MiniLM implementation
│   ├── scraper/       # Markdown fetcher + parser (H2-split, fence-aware)
│   └── packs/         # Manifest schema, upload/download/list, gh wrapper
├── artifacts/
│   ├── manifest.yaml  # tracked: sha256 + indexed_at audit trail
│   └── *.db           # gitignored: per-lib source-of-truth files
└── docs/
    └── research/      # Design notes (Context7 analysis, tursogo migration, etc.)
```

## Why vector search

LLM clients send natural-language queries — `"how to register a tool"` should find the right snippet even if the doc says `AddTool`. Pure exact-match retrieval (FTS5) misses this entirely. Deadzone uses vector embeddings + cosine similarity to handle semantic queries natively, with no hosted dependency.

More background in [`docs/research/context7-analysis.md`](docs/research/context7-analysis.md).

## Debugging

All four binaries emit structured JSON logs to **stderr** using `log/slog`. Stdout is reserved for the MCP JSON-RPC channel on `cmd/server`, so anything written there that isn't a valid JSON-RPC message disconnects the client — `cmd/scraper`, `cmd/consolidate`, and `cmd/packs` follow the same convention for consistency. (`cmd/packs list` is the one exception: it writes a human-facing table to stdout so callers can pipe it through `awk`/`column`.)

- **Scraper.** `just scrape` writes logs straight to your terminal. Look for `scraper.start`, a `scraper.lib_start` per resolved library (with the `artifact_path` it's writing to), one `scraper.fetch` per URL (with `bytes`, `duration_ms`, `docs_extracted`, and `kind`), `scraper.indexed` summaries, a `scraper.lib_done` per library, and a final `scraper.done`. The "silently stalls on one URL" failure mode shows up as a missing `scraper.fetch` event for that URL. Errors land as `scraper.fetch_failed` / `scraper.insert_failed` with the URL and wrapped error. When any source uses `kind: scrape-via-agent`, expect `scraper.agent_configured` and `scraper.agent_ping_ok` once at startup; per-doc hallucination drops show up as `scraper.agent_verification_failed`, and oversized inputs as `agent.input_truncated`.
- **Consolidate.** `just consolidate` emits a `consolidate.start` and a `consolidate.done` with the `artifacts` count, `docs_merged`, `libs_merged`, and `duration_ms`. A failure aborts before any write reaches the main DB; the wrapped error names the offending artifact.
- **Packs.** `just packs-upload` emits `packs.upload.start` (with the resolved repo and `repo_source=flag|manifest|default`), one `packs.upload.skipped` per artifact whose sha256 already matches the manifest, one `packs.upload.uploaded` per artifact pushed via `gh release upload`, an optional `packs.upload.creating_release` if the rolling tag didn't exist yet, and a final `packs.upload.done` with `uploaded`/`skipped`/`preserved` counts. `just packs-download` emits `packs.download.start`, one `packs.verified` per local file whose sha256 matches the manifest (zero network calls), `packs.verified_redownload` when a tampered local file is being silently re-fetched, `packs.downloaded` per fresh fetch, and `packs.download.done` with the rollup. Server-side sha256 mismatches abort with a `download <lib_id>: sha256 mismatch` error and never overwrite the canonical local file.
- **Server.** `cmd/server`'s stderr is captured by the MCP client. In Claude Code that's the `~/Library/Logs/Claude/mcp-server-deadzone.log` file (macOS) or your client's equivalent — check the MCP client docs. On startup the server emits a `server.start` line with the embedder meta and the indexed `doc_count`; each `search_docs` call emits one `search_docs` line with `lib_id`, `tokens`, `results`, and `latency_ms`. If the configured `-db` is missing the server refuses to start and prints a one-liner pointing at `deadzone-consolidate`.
- **Verbose mode.** All four binaries take `-verbose`. On the server it adds the raw `query` field to per-call logs (off by default because queries may contain user data). On the scraper it adds per-doc `scraper.doc_indexed` Debug lines, useful when debugging the parser on a new library.

## Roadmap

Tracked on the [GitHub issues board](https://github.com/laradji/deadzone/issues). Open issues are scoped via the `mvp`, `feature`, `research`, and `post-mvp` labels.

## License

Deadzone is licensed under the [Apache License, Version 2.0](LICENSE). See [`NOTICE`](NOTICE) for the third-party attributions that ship with the binary.

## Content rights (scraped documentation)

The Apache 2.0 license above covers the **Deadzone source code only**. It does **not** grant any rights over the third-party documentation that the scraper indexes. Each documentation source you point Deadzone at — whether through a `github-md` source or a `scrape-via-agent` source — remains the property of its original authors and is governed by whatever license those authors chose for it.

In practice this means:

- Running `deadzone scrape` against a public doc site is subject to that site's Terms of Service.
- A pre-built `pack` distributed via the project's GitHub Releases is bound by the original content's license, not by Apache 2.0. The manifest in `artifacts/manifest.yaml` records each source's `lib_id` so you can trace back to the upstream license if needed.
- Redistributing scraped content outside Deadzone's local search use case may require permission from the original authors.

If you're indexing your own content for personal use, none of this matters. If you're considering distributing a Deadzone pack publicly, do the homework on each source first.
