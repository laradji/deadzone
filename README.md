# Deadzone

A Go-based [MCP](https://modelcontextprotocol.io) server that exposes semantic search over third-party library documentation, indexed locally with [Turso](https://turso.tech) vector storage.

> **Status:** early MVP. Vector search is wired end-to-end on a CGO-free
> [tursogo](https://github.com/tursodatabase/turso/tree/main/bindings/go)
> driver and a CGO-free [hugot](https://github.com/knights-analytics/hugot)
> embedder running [`sentence-transformers/all-MiniLM-L6-v2`](https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2)
> on the pure-Go GoMLX backend. Full
> [roadmap](https://github.com/laradji/deadzone/issues).

Deadzone is a self-hosted alternative to [Context7](https://github.com/upstash/context7) for users who want to keep their docs index on their own machine.

## Features

- **Self-hosted** — local file database, no cloud dependency, no API key
- **Single binary** — no CGO, no Python, no native libs to install
- **Semantic search** — vector embeddings with cosine similarity via Turso's native vector support
- **MCP native** — stdio protocol, plugs directly into Claude Code, Cursor, and other MCP clients
- **Multi-library** — `/org/project` namespacing with first-class `lib_id` filtering
- **Token-budget aware** — trims response size to fit the caller's context window
- **Cross-platform** — pure Go, builds on Linux, macOS, and Windows

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

## Stack

| | |
|---|---|
| Language | Go 1.26.2 (pinned via [`mise`](https://mise.jdx.dev)) |
| Storage | [Turso](https://turso.tech) (local file) with native vector support (`F32_BLOB(N)` + `vector_distance_cos`, dim discovered from the embedder at first open) |
| Driver | [`turso.tech/database/tursogo`](https://pkg.go.dev/turso.tech/database/tursogo) — **CGO-free**, via [`purego`](https://github.com/ebitengine/purego) |
| Embeddings | [`hugot`](https://github.com/knights-analytics/hugot) running [`sentence-transformers/all-MiniLM-L6-v2`](https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2) (384-dim) on the pure-Go GoMLX backend — **CGO-free**, no Python |
| Protocol | [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) over stdio |

## Quick start

Go 1.26.2 and [`just`](https://just.systems) are pinned via [`.mise.toml`](.mise.toml) and intentionally not on the system `PATH`. The repo ships a `justfile` that wraps every Go invocation in `mise exec --`, so you don't need to remember the prefix:

```bash
# 1. Install the pinned toolchain — Go + just (one-time)
mise install

# 2. Build — no CGO required
just build             # = mise exec -- go build ./...

# 3. Scrape every library in libraries_sources.yaml into per-lib artifacts
just scrape            # = mise exec -- go run ./cmd/scraper -artifacts ./artifacts
# → writes one artifacts/<lib_id>.db file per entry in libraries_sources.yaml
# → ships preloaded with the modelcontextprotocol/go-sdk docs

# 4. Merge the per-lib artifacts into the main deadzone.db
just consolidate       # = mise exec -- go run ./cmd/consolidate -db deadzone.db -artifacts ./artifacts

# 5. Run the MCP server against the consolidated DB
just serve             # = mise exec -- go run ./cmd/server -db deadzone.db
```

The `artifacts/` directory and `deadzone.db` are both gitignored — `artifacts/` holds the per-lib source-of-truth files and `deadzone.db` is the derived view the server reads. The server refuses to start if `deadzone.db` is missing and tells you to run `consolidate` first; it never auto-creates an empty file.

Run `just` (no args) to list every recipe. Override the DB path with positional args: `just consolidate foo.db` / `just serve foo.db`. If you'd rather call `go` directly, prefix every command with `mise exec --` so you pick up the pinned toolchain.

### Refreshing a single library

The whole point of the per-lib artifact layout is that one library can be re-scraped without touching the others. Pass the lib_id to `just scrape` (matches the registry's `lib_id` field; for multi-version libs you can target the base or one expanded version):

```bash
# Re-scrape every version of /facebook/react and rebuild its artifact(s)
just scrape /facebook/react

# Re-scrape only one expanded version
just scrape /facebook/react/v18

# Then merge the refreshed artifact(s) back into the main DB
just consolidate
```

`just scrape <lib>` regenerates exactly the matching `artifacts/*.db` files; the others stay byte-identical. `just consolidate` is idempotent — re-running it after a partial scrape just replaces the rows for the libs whose artifacts changed.

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
| `kind` | yes | source kind discriminator — only `github-md` is valid today |
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
│   └── consolidate/   # CLI: merge per-lib artifacts into the main DB
├── internal/
│   ├── db/            # Turso schema, vector queries, consolidation helper
│   ├── embed/         # Embedder interface + hugot/MiniLM implementation
│   └── scraper/       # Markdown fetcher + parser (H2-split, fence-aware)
├── artifacts/         # gitignored: per-lib .db source-of-truth files
└── docs/
    └── research/      # Design notes (Context7 analysis, tursogo migration, etc.)
```

## Why vector search

LLM clients send natural-language queries — `"how to register a tool"` should find the right snippet even if the doc says `AddTool`. Pure exact-match retrieval (FTS5) misses this entirely. Deadzone uses vector embeddings + cosine similarity to handle semantic queries natively, with no hosted dependency.

More background in [`docs/research/context7-analysis.md`](docs/research/context7-analysis.md).

## Debugging

All three binaries emit structured JSON logs to **stderr** using `log/slog`. Stdout is reserved for the MCP JSON-RPC channel on `cmd/server`, so anything written there that isn't a valid JSON-RPC message disconnects the client — `cmd/scraper` and `cmd/consolidate` follow the same convention for consistency.

- **Scraper.** `just scrape` writes logs straight to your terminal. Look for `scraper.start`, a `scraper.lib_start` per resolved library (with the `artifact_path` it's writing to), one `scraper.fetch` per URL (with `bytes`, `duration_ms`, `docs_extracted`), `scraper.indexed` summaries, a `scraper.lib_done` per library, and a final `scraper.done`. The "silently stalls on one URL" failure mode shows up as a missing `scraper.fetch` event for that URL. Errors land as `scraper.fetch_failed` / `scraper.insert_failed` with the URL and wrapped error.
- **Consolidate.** `just consolidate` emits a `consolidate.start` and a `consolidate.done` with the `artifacts` count, `docs_merged`, `libs_merged`, and `duration_ms`. A failure aborts before any write reaches the main DB; the wrapped error names the offending artifact.
- **Server.** `cmd/server`'s stderr is captured by the MCP client. In Claude Code that's the `~/Library/Logs/Claude/mcp-server-deadzone.log` file (macOS) or your client's equivalent — check the MCP client docs. On startup the server emits a `server.start` line with the embedder meta and the indexed `doc_count`; each `search_docs` call emits one `search_docs` line with `lib_id`, `tokens`, `results`, and `latency_ms`. If the configured `-db` is missing the server refuses to start and prints a one-liner pointing at `deadzone-consolidate`.
- **Verbose mode.** All three binaries take `-verbose`. On the server it adds the raw `query` field to per-call logs (off by default because queries may contain user data). On the scraper it adds per-doc `scraper.doc_indexed` Debug lines, useful when debugging the parser on a new library.

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
