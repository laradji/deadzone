# Context7 Protocol — Research Notes

## MCP tools exposed

**Two tools:**

### `resolve-library-id(libraryName, query)`
Resolves a library name into a canonical ID.

**Returns:**
```
- /facebook/react — React (High reputation, 1200 snippets)
- /facebook/react/v18 — React v18 (versioned)
```

### `get-library-docs(context7CompatibleLibraryID, topic?, tokens?)`
Fetches filtered documentation.

- `tokens`: response budget, min 1000, default 5000
- `topic`: section filter (e.g., `"hooks"`, `"authentication"`)

**Flow:**
```
resolve-library-id("React") → /facebook/react
get-library-docs("/facebook/react", topic="hooks", tokens=5000) → snippets
```

## Internal architecture

- **REST backend**: `GET /v1/search?query=X` + `GET /v1/{libraryID}?tokens=Y&topic=Z`
- **Indexing**: automatic GitHub + npm crawling, 33k+ libraries, explicit versioning
- **Ranking**: ML-based (confidence score + semantic relevance)
- **Chunking**: server-side, sliced by token budget
- **Security**: client IP encrypted with AES-256-CBC before hitting the API

## Comparison with Deadzone

| | Context7 | Deadzone MVP |
|---|---|---|
| MCP tools | `resolve-library-id` + `get-library-docs` | `search_docs(query, lib_id?, topic?, tokens?)` |
| Retrieval | ML + vector | FTS5 (`unicode61`) |
| Indexing | Automatic, 33k libs | Manual, one lib at a time |
| Storage | Hosted Upstash API | Local libSQL |
| Token budget | Explicit parameter | Implemented (chars/4) |
| ID format | `/org/project` | `/org/project` (adopted) |

## Decisions taken from this analysis

- **ID format adopted**: `/org/project` (e.g., `/hashicorp/terraform`)
- **`search_docs` signature updated**: added `lib_id`, `topic`, `tokens`
- **Structured response**: `[]Snippet{LibID, Title, Content}` instead of `[]string`

## FTS5 vs vector search limits

The main limitation of FTS5 compared to Context7 is **exact vs semantic retrieval**:

- FTS5 requires exact terms — `"AddTool"` matches, `"register a tool"` does not
- Context7 uses ML/vector — `"how to register a tool"` returns the right snippets

**Conclusion**: getting close to a Context7-like experience makes **vector search effectively required**. FTS5 is good enough for a personal MVP (the user knows which terms to search for) but does not hold up to native LLM usage where queries arrive in natural language.

**Post-MVP option**: libSQL natively supports vectors (`vector_distance_cos`). The intended model — Jina v5 small MLX — runs locally on Apple Silicon. The schema would stay the same, with an `embedding BLOB` column added to the `docs` table and a hybrid FTS5 + ANN retrieval pipeline.

## References

- Repo: https://github.com/upstash/context7
- DeepWiki architecture: https://deepwiki.com/upstash/context7-mcp/2-system-architecture
