# Deadzone

A Go-based [MCP](https://modelcontextprotocol.io) server that exposes semantic search over third-party library documentation, indexed locally with [Turso](https://turso.tech) vector storage.

> **Status:** early MVP. Vector search is wired end-to-end on a CGO-free
> [tursogo](https://github.com/tursodatabase/turso/tree/main/bindings/go)
> driver, currently backed by a **deterministic stub embedder** (hash-based
> bag-of-tokens) until a real model lands — see
> [#2](https://github.com/laradji/deadzone/issues/2). Full
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

Deadzone exposes one MCP tool to clients (Claude Code, Cursor, etc.):

```
search_docs(query, lib_id?, topic?, tokens?) → []Snippet
```

- `query` — natural-language search query (matched semantically against the indexed docs)
- `lib_id` — optional `/org/project` filter (e.g. `/modelcontextprotocol/go-sdk`)
- `topic` — optional section filter (not yet implemented)
- `tokens` — response budget, default 5000, min 1000 (`~4 chars/token`)

Documentation is fetched by a separate `scraper` CLI, embedded into vectors, and stored in a local Turso database file.

## Stack

| | |
|---|---|
| Language | Go 1.26.2 (pinned via [`mise`](https://mise.jdx.dev)) |
| Storage | [Turso](https://turso.tech) (local file) with native vector support (`F32_BLOB(64)` + `vector_distance_cos`) |
| Driver | [`turso.tech/database/tursogo`](https://pkg.go.dev/turso.tech/database/tursogo) — **CGO-free**, via [`purego`](https://github.com/ebitengine/purego) |
| Embeddings | Deterministic stub (hash-based bag-of-tokens with camelCase splitting). Real model tracked in [#2](https://github.com/laradji/deadzone/issues/2) |
| Protocol | [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) over stdio |

## Quick start

Go 1.26.2 and [`just`](https://just.systems) are pinned via [`.mise.toml`](.mise.toml) and intentionally not on the system `PATH`. The repo ships a `justfile` that wraps every Go invocation in `mise exec --`, so you don't need to remember the prefix:

```bash
# 1. Install the pinned toolchain — Go + just (one-time)
mise install

# 2. Build — no CGO required
just build             # = mise exec -- go build ./...

# 3. Scrape and index a library (defaults to ./deadzone.db)
just scrape            # = mise exec -- go run ./cmd/scraper -db deadzone.db
# → indexes the modelcontextprotocol/go-sdk docs

# 4. Run the MCP server
just serve             # = mise exec -- go run ./cmd/server -db deadzone.db
```

Run `just` (no args) to list every recipe. Override the DB path with positional args: `just scrape foo.db` / `just serve foo.db`. If you'd rather call `go` directly, prefix every command with `mise exec --` so you pick up the pinned toolchain.

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

Then call the `search_docs` tool from the client.

## Layout

```
deadzone/
├── cmd/
│   ├── server/    # MCP server entrypoint (search_docs tool)
│   └── scraper/   # CLI: fetch, embed & index a library's docs
├── internal/
│   ├── db/        # Turso schema and vector queries (F32_BLOB + vector_distance_cos)
│   ├── embed/     # Embedder interface + deterministic stub implementation
│   └── scraper/   # Markdown fetcher + parser (H2-split, fence-aware)
└── docs/
    └── research/  # Design notes (Context7 analysis, tursogo migration, etc.)
```

## Why vector search

LLM clients send natural-language queries — `"how to register a tool"` should find the right snippet even if the doc says `AddTool`. Pure exact-match retrieval (FTS5) misses this entirely. Deadzone uses vector embeddings + cosine similarity to handle semantic queries natively, with no hosted dependency.

More background in [`docs/research/context7-analysis.md`](docs/research/context7-analysis.md).

## Roadmap

Tracked on the [GitHub issues board](https://github.com/laradji/deadzone/issues). Open issues are scoped via the `mvp`, `feature`, `research`, and `post-mvp` labels.

## License

TBD.
