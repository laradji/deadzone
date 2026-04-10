# Deadzone

Go-based MCP server exposing full-text search over third-party library documentation. Context7 alternative, no ML dependency for MVP.

## Stack

- **Language**: Go
- **Database**: libSQL + FTS5 (fallback: modernc.org/sqlite if CGO causes issues)
- **Protocol**: MCP via `github.com/modelcontextprotocol/go-sdk` (official SDK)
- **Future embedding**: Jina v5 small MLX (vector search, post-MVP)

## Project setup

```bash
go mod init github.com/laradji/deadzone
go get github.com/modelcontextprotocol/go-sdk
go get github.com/tursodatabase/go-libsql   # requires CGO_ENABLED=1
```

> go-libsql requires CGO. Set `CGO_ENABLED=1` in your build env. If CGO is unavailable, fall back to `modernc.org/sqlite` (pure Go).

## Directory structure

```
deadzone/
├── cmd/
│   └── server/
│       └── main.go      # MCP server entrypoint
├── internal/
│   ├── db/              # libSQL setup, FTS5 schema, queries
│   ├── scraper/         # Doc fetcher + parser per library
│   └── search/          # FTS5 query logic
└── pkg/                 # Reusable exportable packages
```

## MCP server pattern

```go
// cmd/server/main.go
package main

import (
    "context"
    "log"

    "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Library IDs follow the format /org/project (e.g. /hashicorp/terraform)
type SearchDocsInput struct {
    Query  string `json:"query" jsonschema:"the search query"`
    LibID  string `json:"lib_id,omitempty" jsonschema:"library ID filter in /org/project format (optional)"`
    Topic  string `json:"topic,omitempty" jsonschema:"topic or section to focus on (optional)"`
    Tokens int    `json:"tokens,omitempty" jsonschema:"max tokens to return, min 1000, default 5000 (optional)"`
}

type Snippet struct {
    LibID   string `json:"lib_id"`
    Title   string `json:"title"`
    Content string `json:"content"`
}

type SearchDocsOutput struct {
    Snippets []Snippet `json:"snippets"`
}

func handleSearchDocs(ctx context.Context, req *mcp.CallToolRequest, input SearchDocsInput) (*mcp.CallToolResult, SearchDocsOutput, error) {
    // TODO: query FTS5 via internal/search
    return nil, SearchDocsOutput{}, nil
}

func main() {
    s := mcp.NewServer(&mcp.Implementation{Name: "deadzone", Version: "v0.1.0"}, nil)
    mcp.AddTool(s, &mcp.Tool{
        Name:        "search_docs",
        Description: "Search documentation snippets for a library. Use lib_id in /org/project format to filter by library.",
    }, handleSearchDocs)

    if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
        log.Fatal(err)
    }
}
```

## libSQL / FTS5 pattern

```go
// internal/db/db.go
import (
    "database/sql"
    _ "github.com/tursodatabase/go-libsql"
)

// Local file only (no Turso remote for MVP)
db, err := sql.Open("libsql", "file:deadzone.db")

// FTS5 virtual table
db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(
    lib, title, content, tokenize="unicode61"
)`)
```

## MVP goal

1. Scrape a lib (e.g. Terraform AWS provider) → parse docs
2. Index into libSQL/FTS5
3. Expose via MCP: `search_docs(query, lib_id?, topic?, tokens?)` → relevant snippets

## References

- go-sdk docs (official): https://github.com/modelcontextprotocol/go-sdk
- go-libsql docs: https://github.com/tursodatabase/go-libsql
- Context7 protocol (reference): https://github.com/upstash/context7
- Project Kanban: Obsidian > Projects/Deadzone.md

## Local environment

Env vars loaded via `.envrc` (direnv). Never commit keys.

## Conventions

- No web framework — MCP stdio only
- Integration tests against real DB (no SQLite mocks)
- Semantic commits: `add/update/fix` + context
- `CGO_ENABLED=1` required for go-libsql builds
