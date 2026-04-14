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
- **Single download** — one archive, one `deadzone` binary with subcommands, ONNX Runtime auto-fetched on first run
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

Documentation is fetched by the `deadzone scrape` subcommand, embedded into vectors, and stored in a local Turso database file.

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

Each archive extracts a single `deadzone` binary plus `LICENSE`, `NOTICE`, and `README.md`.

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
xattr -d com.apple.quarantine deadzone
```

This workaround goes away once notarization lands.

### Subcommands, briefly

End users usually only touch the first three. `deadzone scrape` is for contributors maintaining [`libraries_sources.yaml`](libraries_sources.yaml).

| Subcommand | What it's for |
|---|---|
| `deadzone server` | MCP stdio server — what your AI client talks to. Auto-fetches the `deadzone.db` matching this binary's version on first run, and re-fetches only when the binary itself is upgraded (see [Data](#data)). |
| `deadzone fetch-db` | Explicit cache-warmup / refresh of `deadzone.db` (useful before going offline, or to recover from local corruption with `-force`). |
| `deadzone consolidate` | Merges per-lib artifacts into a single `deadzone.db` (contributor flow) |
| `deadzone scrape` | Re-scrapes a library from its configured sources |
| `deadzone dbrelease` | Operator-driven: uploads `deadzone.db` + `.sha256` to a tagged GitHub Release |
| `deadzone packs` | Disabled (see [#101](https://github.com/laradji/deadzone/issues/101)); use `dbrelease` |

Run `deadzone -h` for the subcommand list, or `deadzone <sub> -h` for a subcommand's flags. `deadzone -version` prints the banner without touching the DB or embedder.

### Runtime dependencies

Deadzone follows the same pattern for every native runtime dependency: **no system installs, nothing bundled in the binary, nothing pulled at build time except what links statically**. Instead, each shared library is fetched on first use, SHA256-verified against a pinned manifest, cached in the user-cache dir, and loaded via `purego` (`tursogo`) or `dlopen` (ONNX Runtime) at runtime. Subsequent runs reuse the cache; second-launch startup is instant.

This keeps the install flow to "download the tarball, extract, run" across macOS arm64, Linux amd64, and Linux arm64 without a package manager or a C toolchain. It also makes air-gapped installs easy: pre-populate the caches or point the escape-hatch env vars at hand-positioned libraries.

**What gets fetched on first launch of `deadzone server`, `deadzone scrape`, or `deadzone consolidate`:**

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

Tagged releases ship a prebuilt `deadzone.db` covering the libraries listed in [`libraries_sources.yaml`](libraries_sources.yaml). After extracting the binary, just run the server — the DB matching this binary's own version is downloaded on first launch into the platform data dir, sha256-verified, and cached. Steady-state startup is zero-network: the cache sidecar tag is compared against the binary's version at startup, and only a binary version bump triggers a re-fetch.

```bash
./deadzone server  # downloads deadzone.db on first run, then serves
```

The per-platform binary tarballs and their aggregated `deadzone_${VERSION}_checksums.txt` are uploaded by CI when the tag is pushed; `deadzone.db` and `deadzone.db.sha256` are uploaded separately by the maintainer via `deadzone dbrelease` (see [Releasing a new `deadzone.db`](#releasing-a-new-deadzonedb) below). The two halves live on the same release object.

With the server running, point any MCP-capable client at it — see [Wire it into an MCP client](#wire-it-into-an-mcp-client) for the exact JSON snippet. To pin a different DB, hand-place the file and run with `./deadzone server -db /path/to/deadzone.db` — explicit `-db` bypasses the auto-fetch entirely.

### Data

`deadzone server` (and `deadzone fetch-db`) cache `deadzone.db` under the platform's standard per-user data directory:

| Platform | Default cache path |
|---|---|
| macOS | `~/Library/Application Support/deadzone/deadzone.db` |
| Linux | `$XDG_DATA_HOME/deadzone/deadzone.db` (falls back to `~/.local/share/deadzone/deadzone.db`) |
| Windows | `%LOCALAPPDATA%\deadzone\deadzone.db` |

A sibling `deadzone.db.release` text file records the release tag the cache was fetched from.

**The cached DB is pinned to the binary's own version.** On every startup the server compares the cache sidecar tag against the binary's compiled-in version (set by `-ldflags -X main.version=...`, see `build-release` in the [`justfile`](justfile)):

- **Tag matches** → zero-network fast path; the cache is served as-is. No GitHub API call.
- **Tag differs** (the binary was upgraded) → fetch `/releases/tags/<binary-version>`, atomic-swap the cache, serve the new DB.
- **Binary is a dev build** (literal `dev`, `-dirty` suffix, or `git describe` between-tags form) → fall back to `/releases/latest` with a `server.db_version_dev_fallback` WARN so local iteration stays ergonomic.

The DB does not auto-upgrade independently of the binary: if upstream publishes a newer DB while this binary is still running, the server keeps using the cached DB it was pinned to. `deadzone upgrade` (or a tarball re-extract) is what changes the binary's version and, on next launch, triggers the DB swap.

**Env-var escape hatches** (matching the `DEADZONE_ORT_*` / `DEADZONE_HUGOT_*` pattern):

| Env var | Effect |
|---|---|
| `DEADZONE_DB_CACHE` | Override the cache directory. |
| `DEADZONE_DB_OFFLINE=1` | Never make a network call. Fails loudly on first run if nothing is cached; also fails loudly if the cache exists but its version doesn't match the binary — hand-place a `deadzone.db` that matches, or unset the env var so the auto-fetch can run. |

`deadzone fetch-db` is the explicit refresh path: pre-populate the cache before going offline, or recover from local corruption with `deadzone fetch-db -force` (same binary version, fresh bytes, sha256-verified).

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

# 4. Scrape the full corpus locally — one artifact folder per lib under ./artifacts/
just scrape            # = mise exec -- go run -tags ORT ./cmd/deadzone scrape -artifacts ./artifacts

# 5. Merge the per-lib artifacts into the main deadzone.db
just consolidate       # = mise exec -- go run -tags ORT ./cmd/deadzone consolidate -db deadzone.db -artifacts ./artifacts

# 6. Run the MCP server against the consolidated DB
just serve             # = mise exec -- go run -tags ORT ./cmd/deadzone server -db deadzone.db
```

The per-lib artifact folders under `./artifacts/<slug>/` (each containing `artifact.db` + `state.yaml`) and `deadzone.db` are all gitignored — they're local build outputs. The committed [`artifacts/manifest.yaml`](artifacts/manifest.yaml) records the most recent `deadzone.db` release (tag, sha256, embedder, counts) as a release-history trace; it's rewritten by `deadzone dbrelease`, not by hand. When `-db` points at a missing file the server errors out and points at both auto-fetch (run without `-db`) and `consolidate` (build from local artifacts); it never auto-creates an empty file.

> **Note.** The per-artifact GitHub Release distribution flow (`deadzone packs {upload,download,list}`) is paused as of [#101](https://github.com/laradji/deadzone/issues/101) — contributors who want a working DB run `just scrape && just consolidate` locally. Releases carry `deadzone.db` as a single consolidated asset; per-artifact distribution will return when CI takes over at scale.

Run `just` (no args) to list every recipe. Override the DB path with positional args: `just consolidate foo.db` / `just serve foo.db`. If you'd rather call `go` directly, prefix every command with `mise exec --` so you pick up the pinned toolchain.

### Building release binaries

`just build` is a fast compile check (`go build ./...` — produces no output binaries). To produce the single `deadzone` CLI at the repo root with version info embedded, use `just build-release`:

```bash
# Local dev build — version/commit/date default from git describe + rev-parse + UTC now
just build-release
./deadzone -version
# → deadzone v0.1.0-2-gabc1234-dirty (abc1234, built 2026-04-12T12:00:00Z)

# Release build — CI sets VERSION/COMMIT/DATE explicitly from the workflow
VERSION=v0.1.0 COMMIT=$(git rev-parse --short HEAD) DATE=$(date -u +%FT%TZ) just build-release
./deadzone -version
# → deadzone v0.1.0 (abc1234, built 2026-04-12T12:00:00Z)
```

`deadzone -version` prints the banner and exits without touching the DB or embedder — the fast path used by CI's smoke job. The recipe compiles with `-trimpath -ldflags "-s -w -X main.version=… -X main.commit=… -X main.date=…"`, so absolute build-host paths never leak into the binary and the stripped output stays small despite the CGO ORT dependency.

### Refreshing a single library

The per-lib folder layout means one library can be re-scraped without touching the others. The flow is the same for both single-version libs and multi-version (`/facebook/react/v18`, `/facebook/react/v19`, …) entries:

```bash
# Re-scrape locally (rebuilds exactly the matching artifacts/<slug>/artifact.db)
just scrape /facebook/react           # base — every versioned child
just scrape /facebook/react/v18       # one expanded version

# Then re-consolidate to pick up the change in the main DB.
just consolidate
```

Each scrape rewrites `artifacts/<slug>/artifact.db` (and its `state.yaml`) in place; re-running `consolidate` merges the refreshed rows over the top of the existing main-DB slice under the same `lib_id`. There is currently no incremental distribution of individual libs — maintainers who want the whole refreshed corpus on a release ship the consolidated `deadzone.db` via `just dbrelease <tag>` (see below).

### Releasing a new `deadzone.db`

Releases are **two-phase** as of [#101](https://github.com/laradji/deadzone/issues/101): CI publishes the per-platform binary tarballs when the tag is pushed, and the maintainer uploads the consolidated `deadzone.db` + its sha256 to the same release from their laptop.

```bash
# 1. Regenerate deadzone.db from the committed scraper config.
just scrape
just consolidate

# 2. Tag + push. CI's release.yml builds the three binary tarballs,
#    uploads them, and creates the release object.
git tag v0.1.0
git push --tags

# 3. Ship the DB to the same tag (sha256 is computed on-the-fly and
#    uploaded as a sibling asset). Rewrites artifacts/manifest.yaml
#    with the new release record.
just dbrelease v0.1.0

# 4. Commit the manifest diff so the release-history trace lands in git.
git add artifacts/manifest.yaml && git commit -m "release v0.1.0" && git push
```

`deadzone dbrelease` shells out to `gh release upload <tag> deadzone.db deadzone.db.sha256 --clobber`. The `gh` CLI handles auth via your existing `gh auth login` state. Override the target repo with `-repo owner/name` when working from a fork.

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
| `kind` | yes | source kind discriminator — `github-md` for raw markdown, `github-rst` for raw reStructuredText (cpython, Django, NumPy, …), `scrape-via-agent` for HTML/text via an LLM (see [Scraping non-trivial doc sources](#scraping-non-trivial-doc-sources-scrape-via-agent)) |
| `urls` | yes | list of doc URLs (with optional `{version}` placeholder) |
| `versions` | no | list of version tags; expands `{version}` in `urls` and produces one effective `lib_id` per version |

Adding a new library means adding a YAML entry — no Go editing, no recompile.

The scrape subcommand accepts a few flags for working with the registry and the artifact directory:

```bash
# Use a non-default registry path
mise exec -- go run ./cmd/deadzone scrape -artifacts ./artifacts -config /path/to/libraries_sources.yaml

# Use a non-default artifacts directory
mise exec -- go run ./cmd/deadzone scrape -artifacts /var/cache/deadzone/artifacts

# Scrape every configured version of one base lib
mise exec -- go run ./cmd/deadzone scrape -artifacts ./artifacts -lib /facebook/react

# Scrape only one specific versioned lib
mise exec -- go run ./cmd/deadzone scrape -artifacts ./artifacts -lib /facebook/react/v18
```

`-lib` matches at two levels: a base `lib_id` selects every expanded version of that base; a fully versioned `lib_id` selects exactly one expanded entry. Omitting `-lib` scrapes everything in the registry. Each entry produces (or replaces) one `artifacts/<slug>/artifact.db` file (+ `state.yaml` sidecar) — the leading `/` is stripped from the `lib_id` and the remaining `/` characters become `_`, so `/facebook/react/v18` lands at `artifacts/facebook_react_v18/artifact.db`.

### Scraping non-trivial doc sources (`scrape-via-agent`)

> ⚠️ **Experimental.** The `scrape-via-agent` path is the messy half of Deadzone. It works, and it's how non-markdown sources (Terraform providers, mkdocs, etc.) get indexed today, but the LLM-extraction → strict-verifier loop is sensitive to: input truncation cutting mid code-block (the 48 KiB cap below), the model's HTML→markdown skill, and the verifier's appetite for verbatim code matches. Real-world hit rate on dense doc sites is currently ~50% per URL — see [#64](https://github.com/laradji/deadzone/issues/64). Prefer `github-md` whenever the project ships its docs as committed markdown in the repo (most do, including FastAPI, OpenTofu's mkdocs source, etc.). Reach for `scrape-via-agent` only when the docs genuinely live HTML-only on a doc site.

The `github-md` and `github-rst` kinds only work on libraries that publish raw markdown or reStructuredText on GitHub. For everything else — Terraform providers (HTML), React (`react.dev`), mkdocs/docusaurus/vitepress sites, GitBook, ReadTheDocs — Deadzone supports a third source kind, `scrape-via-agent`, that delegates **content → clean markdown** extraction to any OpenAI-compatible chat completions endpoint.

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
      "command": "/path/to/deadzone",
      "args": ["server"]
    }
  }
}
```

The server resolves `deadzone.db` from the platform data dir on first launch (see [Data](#data) for cache paths and env-var overrides) and auto-upgrades it on subsequent launches. To pin a specific DB file, add `"-db", "/path/to/deadzone.db"` to `args`.

Then call the `search_docs` or `search_libraries` tool from the client.

## Layout

```
deadzone/
├── cmd/
│   └── deadzone/      # single CLI with subcommands:
│                      #   server       — MCP stdio entrypoint (search_docs / search_libraries)
│                      #   scrape       — fetch, embed & write per-lib artifacts
│                      #   consolidate  — merge per-lib artifacts into the main DB
│                      #   dbrelease    — upload ./deadzone.db to a tagged GitHub Release
│                      #   packs        — disabled (#101); use dbrelease instead
├── internal/
│   ├── db/            # Turso schema, vector queries, consolidation helper
│   ├── embed/         # Embedder interface + hugot/MiniLM implementation
│   ├── scraper/       # Markdown fetcher + parser (H2-split, fence-aware)
│   └── packs/         # Folder layout helpers, manifest schema, gh wrapper
├── artifacts/
│   ├── manifest.yaml  # tracked: release-history trace (tag, sha256, counts)
│   └── <slug>/        # gitignored: per-lib folder with artifact.db + state.yaml
└── docs/
    └── research/      # Design notes (Context7 analysis, tursogo migration, etc.)
```

## Why vector search

LLM clients send natural-language queries — `"how to register a tool"` should find the right snippet even if the doc says `AddTool`. Pure exact-match retrieval (FTS5) misses this entirely. Deadzone uses vector embeddings + cosine similarity to handle semantic queries natively, with no hosted dependency.

More background in [`docs/research/context7-analysis.md`](docs/research/context7-analysis.md).

## Debugging

Every subcommand emits structured JSON logs to **stderr** using `log/slog`. Stdout is reserved for the MCP JSON-RPC channel on `deadzone server`, so anything written there that isn't a valid JSON-RPC message disconnects the client — `deadzone scrape`, `deadzone consolidate`, and `deadzone packs` follow the same convention for consistency. (`deadzone packs list` is the one exception: it writes a human-facing table to stdout so callers can pipe it through `awk`/`column`.)

- **Scraper.** `just scrape` writes logs straight to your terminal. Look for `scraper.start`, a `scraper.lib_start` per resolved library (with the `artifact_path` it's writing to), one `scraper.fetch` per URL (with `bytes`, `duration_ms`, `docs_extracted`, and `kind`), `scraper.indexed` summaries, a `scraper.lib_done` per library, and a final `scraper.done`. The "silently stalls on one URL" failure mode shows up as a missing `scraper.fetch` event for that URL. Errors land as `scraper.fetch_failed` / `scraper.insert_failed` with the URL and wrapped error. When any source uses `kind: scrape-via-agent`, expect `scraper.agent_configured` and `scraper.agent_ping_ok` once at startup; per-doc hallucination drops show up as `scraper.agent_verification_failed`, and oversized inputs as `agent.input_truncated`.
- **Consolidate.** `just consolidate` emits a `consolidate.start` and a `consolidate.done` with the `artifacts` count, `docs_merged`, `libs_merged`, and `duration_ms`. A failure aborts before any write reaches the main DB; the wrapped error names the offending artifact.
- **DB release.** `just dbrelease v0.1.0` emits `dbrelease.start` (with `db_path`, `tag`, `repo`), then `packs.dbrelease.uploaded` per uploaded asset (`deadzone.db` + `deadzone.db.sha256`), and a final `dbrelease.done` line carrying `sha256`, `size`, `lib_count`, `doc_count`, and the manifest path. The operator then commits the manifest diff to record the release.
- **Server.** `deadzone server`'s stderr is captured by the MCP client. In Claude Code that's the `~/Library/Logs/Claude/mcp-server-deadzone.log` file (macOS) or your client's equivalent — check the MCP client docs. On startup the server emits a `server.start` line with the embedder meta and the indexed `doc_count`; each `search_docs` call emits one `search_docs` line with `lib_id`, `tokens`, `results`, and `latency_ms`. When `-db` is unset the server runs `db.Bootstrap` first; expect a `server.db_upgraded` line when the binary version bump triggered a cache swap, a `server.db_version_dev_fallback` WARN when running a dev build (dev builds use `/releases/latest` instead of pinning to a tag), or a `server.db_tag_sidecar_write_failed` WARN if the tag sidecar couldn't be persisted after a successful DB install (non-fatal — next startup will just re-fetch). If an explicit `-db <path>` is missing the server refuses to start and points at both the auto-fetch path (run without `-db`) and `deadzone consolidate`.
- **Verbose mode.** Every subcommand takes `-verbose`. On the server it adds the raw `query` field to per-call logs (off by default because queries may contain user data). On the scraper it adds per-doc `scraper.doc_indexed` Debug lines, useful when debugging the parser on a new library.

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
