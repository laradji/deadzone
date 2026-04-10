# Deadzone

A Go-based [MCP](https://modelcontextprotocol.io) server that exposes semantic search over third-party library documentation, indexed locally with [Turso](https://turso.tech) vector storage.

> **Status:** early MVP, currently pivoting from FTS5 (exact-term) to a pure vector retrieval pipeline. See the [roadmap](https://github.com/laradji/deadzone/issues).

Deadzone is a self-hosted alternative to [Context7](https://github.com/upstash/context7) for users who want to keep their docs index on their own machine.

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
| Storage | [Turso](https://turso.tech) (local file) with native vector support (`F32_BLOB` + `vector_distance_cos`) |
| Driver | [`tursodatabase/go-libsql`](https://github.com/tursodatabase/go-libsql) — CGO required today, [migration to CGO-free `tursogo`](https://github.com/laradji/deadzone/issues/13) tracked |
| Embeddings | TBD (see issue [#2](https://github.com/laradji/deadzone/issues/2)) |
| Protocol | [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) over stdio |

## Quick start

```bash
# 1. Install Go via mise (project-pinned)
mise install

# 2. Build
CGO_ENABLED=1 go build ./...

# 3. Scrape and index a library
CGO_ENABLED=1 go run ./cmd/scraper -db deadzone.db
# → indexes the modelcontextprotocol/go-sdk docs

# 4. Run the MCP server
CGO_ENABLED=1 go run ./cmd/server -db deadzone.db
```

### Wire it into an MCP client

Add to your client's MCP config (Claude Code, Cursor, etc.):

```json
{
  "mcpServers": {
    "deadzone": {
      "type": "stdio",
      "command": "/path/to/deadzone-server",
      "args": ["-db", "/path/to/deadzone.db"],
      "env": { "CGO_ENABLED": "1" }
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
│   ├── db/        # Turso schema and vector queries
│   ├── scraper/   # Markdown fetcher + parser (H2-split, fence-aware)
│   └── search/    # (placeholder for retrieval logic)
└── docs/
    └── research/  # Design notes (Context7 analysis, etc.)
```

## Why vector search

LLM clients send natural-language queries — `"how to register a tool"` should find the right snippet even if the doc says `AddTool`. Pure exact-match retrieval (FTS5) misses this entirely. Deadzone uses vector embeddings + cosine similarity to handle semantic queries natively, with no hosted dependency.

More background in [`docs/research/context7-analysis.md`](docs/research/context7-analysis.md).

## Roadmap

Tracked on the [GitHub issues board](https://github.com/laradji/deadzone/issues). Open issues are scoped via the `mvp`, `feature`, `research`, and `post-mvp` labels.

## License

TBD.
