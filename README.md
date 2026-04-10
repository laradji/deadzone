# Deadzone

A Go-based [MCP](https://modelcontextprotocol.io) server that exposes semantic search over third-party library documentation, indexed locally with [Turso](https://turso.tech) vector storage.

> **Status:** early MVP. Vector search is wired end-to-end on a CGO-free
> [tursogo](https://github.com/tursodatabase/turso/tree/main/bindings/go)
> driver, with embeddings produced by [hugot](https://github.com/knights-analytics/hugot)
> running [`sentence-transformers/all-MiniLM-L6-v2`](https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2)
> on a pure-Go GoMLX backend (no CGO, no Python). Full
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
| Storage | [Turso](https://turso.tech) (local file) with native vector support (`F32_BLOB(384)` + `vector_distance_cos`; column width tracks the embedder's dimension) |
| Driver | [`turso.tech/database/tursogo`](https://pkg.go.dev/turso.tech/database/tursogo) — **CGO-free**, via [`purego`](https://github.com/ebitengine/purego) |
| Embeddings | [hugot](https://github.com/knights-analytics/hugot) running [`sentence-transformers/all-MiniLM-L6-v2`](https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2) (384-dim) on the pure-Go GoMLX backend — no CGO, no Python |
| Protocol | [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) over stdio |

## Quick start

```bash
# 1. Install Go via mise (project-pinned)
mise install

# 2. Build — no CGO required
go build ./...

# 3. Scrape and index a library
go run ./cmd/scraper -db deadzone.db
# → indexes the modelcontextprotocol/go-sdk docs
# → first run downloads the MiniLM-L6-v2 model (~90 MB) into the
#   per-user cache (macOS: ~/Library/Caches/deadzone/models, Linux:
#   $XDG_CACHE_HOME/deadzone/models). Subsequent runs reuse it.

# 4. Run the MCP server
go run ./cmd/server -db deadzone.db
```

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
│   ├── embed/     # Embedder interface + hugot/MiniLM-L6-v2 implementation
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
