# tursogo Migration — Research Notes

> **Status: research complete, 2026-04-10.** Original FTS-focused plan in
> [#13](https://github.com/laradji/deadzone/issues/13) was rejected after
> finding that (a) turso's FTS is compile-time gated and missing accent
> folding, and (b) tursogo ships fully working vector search. Migration
> reframed as **vector-first**. First implementation step tracked in
> [#14](https://github.com/laradji/deadzone/issues/14). Issue #13 closed.

Research for [#13](https://github.com/laradji/deadzone/issues/13) — moving the
DB driver from `github.com/tursodatabase/go-libsql` (CGO) to a CGO-free
alternative. Conducted 2026-04-10 against `tursodatabase/turso@main`
(last push 2026-04-10, latest stable tag `v0.5.3` from 2026-04-02).

---

## TL;DR — recommendation

**Skip FTS entirely. Migrate to tursogo and go straight to vector search.**

Updated 2026-04-10 after confirming that `tursogo` ships full vector
support — see "Vector search — confirmed working" below. This changes the
calculus from the original FTS-focused plan in #13.

Why the migration is now attractive:

1. **Vectors work out of the box in tursogo** — `vector_distance_cos`,
   `vector32`/`vector64`/`vector8`/`vector1bit` types, and `F32_BLOB(N)`
   columns, all proven by `bindings/go/driver_db_test.go::TestVectorOperations`
   in the upstream test suite. **No CGO, no compile flags, no experimental
   gating.** Function names match libSQL.
2. **Per `context7-analysis.md`, vectors are the long-term target anyway.**
   Context7 uses Upstash Vector + reranking; getting close to that
   experience makes vector retrieval effectively required for deadzone too.
3. **The FTS regression no longer matters** because we're abandoning FTS,
   not migrating it. The whole "no `unicode61` equivalent" / "compile-time
   gated" / "weak unicode test" rabbit hole becomes irrelevant.

Why the migration is still mildly risky:

1. **Vector indexes are NOT yet implemented** in turso — the docs say "All
   similarity searches use a linear scan over the table." For deadzone's
   small corpus (a handful of repos worth of markdown) this is fine. It
   would be a problem at >100k snippets.
2. **`tursogo` is BETA, FTS is experimental.** We don't care about FTS
   anymore, but we still depend on a BETA driver.
3. **Re-scrape required on cut-over.** Turso is a from-scratch Rust engine,
   not a SQLite/libSQL fork — existing `deadzone.db` files cannot be opened.
   For a local dev tool with a fixed corpus this is cheap.

**Recommended path:**

1. Spike: rewrite `internal/db/db.go` to use tursogo + vector columns
   instead of FTS5. Replace `Doc` storage with `(lib_id, title, content,
   embedding F32_BLOB(N))`. Replace `Search` with a `vector_distance_cos`
   ORDER BY query.
2. Wire up an embedding step in the scraper. Local Apple Silicon target:
   Jina v3/v5 small via MLX (per the existing post-MVP plan in
   `context7-analysis.md`).
3. Rewrite `TestSearch_*` tests to assert semantic recall instead of
   keyword recall (e.g., "register a tool" finds the `mcp.AddTool` snippet).
4. Drop `CGO_ENABLED=1` from `.envrc` and update `README.md` / `CLAUDE.md`.

If you want a smaller first step, do (1) only — keep the embedding step
on-deck and use deterministic stub vectors to prove the schema works
end-to-end before pulling in MLX.

---

## Verified facts

### Package identity

| Question | Answer | Source |
|---|---|---|
| Module path | `turso.tech/database/tursogo` | `bindings/go/go.mod` line 1 |
| Repo of record | `github.com/tursodatabase/turso/bindings/go` | github |
| `sql.Open` driver name | `"turso"` | `bindings/go/driver_db.go` — `sql.Register("turso", &tursoDbDriver{})` |
| Embedded version | `6119c6f0` (commit hash, not semver) | `bindings/go/VERSION` |
| Latest stable repo tag | `v0.5.3` (2026-04-02) | `gh api repos/tursodatabase/turso/releases` |
| Latest pre-release | `v0.6.0-pre.16` (2026-04-08) | same |
| Stability disclaimer | "BETA. May still contain bugs and unexpected behavior." | `bindings/go/README.md` |
| Platforms | Linux, macOS, Windows | `bindings/go/README.md` |
| CGO needed? | No — uses `purego` to call the Rust C ABI | `bindings/go/README.md` |

The issue's `turso.tech/database/tursogo` path is **correct** — it's a vanity
import that resolves to the github repo. Both pkg.go.dev entries
(`turso.tech/database/tursogo` and `github.com/tursodatabase/turso/bindings/go`)
exist and refer to the same code. The `github.com/tursodatabase/turso-go` repo
has been merged back into the main turso repo and now redirects.

### DSN format

From `bindings/go/driver_db.go` `parseDSN`:

```
<path>[?experimental=<string>&async=0|1&vfs=<string>&encryption_cipher=<string>&encryption_hexkey=<string>&_busy_timeout=<int>]
```

Translation for our case:

```go
db, err := sql.Open("turso", "deadzone.db?experimental=fts")
```

The `experimental` query param is parsed as a string and stored in
`config.ExperimentalFeatures`. Whether passing `experimental=fts` actually
turns FTS on at runtime is **unverified** — see "Compile-time FTS gating"
below.

### FTS — what's actually available

`docs/fts.md` and `docs/manual.md` agree on a list of **five** tokenizers
(the issue claimed three):

| Tokenizer | Behavior | Useful for |
|---|---|---|
| `default` | Lowercase, punctuation split, 40 char limit | English |
| `simple` | Whitespace/punctuation split, **no** lowercasing | exact-case text |
| `whitespace` | Splits on whitespace only | preserved punctuation |
| `ngram` | 2-3 char n-grams | autocomplete / substring |
| `raw` | No tokenization | UUIDs, tags, exact match |

**None of the documentation mentions:**

- Unicode normalization (NFC/NFD)
- Diacritic / accent folding
- Stemming
- Language-specific tokenization

There is no equivalent of SQLite FTS5's `unicode61` tokenizer, which is what
`internal/db/db.go:25` currently uses.

### FTS — DDL and query syntax

The issue had this part right. New syntax is:

```sql
CREATE INDEX idx_docs ON docs USING fts (lib_id, title, content);
-- or with config
CREATE INDEX idx_docs ON docs USING fts (title, content)
  WITH (tokenizer = 'simple', weights = 'title=2.0,content=1.0');
```

Query forms:

```sql
-- function form
SELECT * FROM docs WHERE fts_match(title, content, 'server');
-- operator form
SELECT * FROM docs WHERE content MATCH 'server';
```

Ranking via `fts_score()`, highlighting via `fts_highlight()`. Query string
follows Tantivy's QueryParser syntax (AND/OR/NOT, `"phrase"`, `prefix*`,
`field:term`).

This is **not** a drop-in replacement for the current
`CREATE VIRTUAL TABLE docs USING fts5(...)`. The schema needs to be a real
table plus a separate `CREATE INDEX ... USING fts`.

### Compile-time FTS gating — the show-stopper

`docs/manual.md`, verbatim:

> Full-text search is an experimental feature and requires the `fts` feature
> to be enabled at compile time.

This is **not** a `--experimental-fts` runtime CLI flag (the issue's
guess). It's a Rust/Cargo feature flag that has to be on when the native
`turso_core` library is built.

**Why this matters for `tursogo`:** The Go binding embeds a precompiled native
library that is "automatically extracted and loaded at runtime." We have no
control over how that library was built. If the embedded blob was compiled
without `--features fts`, then:

- `CREATE INDEX ... USING fts` will fail at runtime
- No DSN/pragma/env var can recover from it
- The only fix is upstream rebuilding the embedded lib

**Action item**: Before doing the spike, write a 10-line standalone Go
program that opens an in-memory turso DB and runs
`CREATE INDEX foo ON t USING fts(c);` to verify FTS is compiled in. If it
errors with an unknown-index-method message, the migration is blocked.

### Vector search — confirmed working

This is the most decision-relevant finding and was missing from the original
plan in #13. `tursogo` ships **full vector support out of the box**, with
the same function names as libSQL. There is no compile flag, no DSN
parameter, no experimental gating — it just works.

**Proof from upstream's own test suite** —
`bindings/go/driver_db_test.go::TestVectorOperations` (verbatim):

```go
func TestVectorOperations(t *testing.T) {
    db := openMem(t)
    _, err := db.Exec(`CREATE TABLE vector_test (id INTEGER PRIMARY KEY, embedding F32_BLOB(64))`)
    if err != nil {
        t.Fatalf("Error creating vector table: %v", err)
    }

    _, err = db.Exec(`INSERT INTO vector_test VALUES (1, vector('[0.1, 0.2, 0.3, 0.4, 0.5]'))`)
    if err != nil {
        t.Fatalf("Error inserting vector: %v", err)
    }

    var similarity float64
    err = db.QueryRow(`SELECT vector_distance_cos(embedding, vector('[0.2, 0.3, 0.4, 0.5, 0.6]')) FROM vector_test WHERE id = 1`).Scan(&similarity)
    if err != nil {
        t.Fatalf("Error calculating vector similarity: %v", err)
    }

    var extracted string
    err = db.QueryRow(`SELECT vector_extract(embedding) FROM vector_test WHERE id = 1`).Scan(&extracted)
    // ...
}
```

**What this confirms:**

- Schema: `embedding F32_BLOB(64)` for a 64-dimension f32 vector column
- Insert: `vector('[0.1, 0.2, ...]')` parses a JSON array of floats
- Query: `vector_distance_cos(col, vector('[...]'))` returns a REAL between
  0 and 1 (cosine distance)
- Read-back: `vector_extract(blob)` returns the vector as a TEXT JSON array
- `database/sql` round-trip works through the standard `Scan` interface

**Available functions** (from `docs/sql-reference/functions/vector.mdx` and
confirmed in `COMPAT.md` as `✅ Yes`):

| Constructor | Storage |
|---|---|
| `vector(json_array)` / `vector32(...)` | 32-bit float, 4 bytes/dim |
| `vector64(...)` | 64-bit float, 8 bytes/dim |
| `vector8(...)` | 8-bit quantized, 1 byte/dim |
| `vector1bit(...)` | 1-bit binary, 8 dims/byte |

| Distance | Use |
|---|---|
| `vector_distance_cos(v1, v2)` | cosine — best for normalized embeddings |
| `vector_distance_l2(v1, v2)` | Euclidean |
| `vector_distance_dot(v1, v2)` | dot product |
| `vector_distance_jaccard(v1, v2)` | sparse / set similarity |

| Utility | |
|---|---|
| `vector_extract(blob)` | blob → JSON text |
| `vector_concat(v1, v2, ...)` | combine |
| `vector_slice(blob, start, end)` | substring |

**Important caveat — no vector index yet:**

> "Vector indexes are not yet supported. All similarity searches use a
> linear scan over the table." — `docs/sql-reference/functions/vector.mdx`

There are sketches of an IVF method (`core/index_method/toy_vector_sparse_ivf.rs`)
but they're labelled "toy" and not exposed. For deadzone's corpus size
(< 10k snippets) linear scan is fine: 10k × 1024-dim cosine distances is
sub-millisecond on a modern CPU.

**libSQL compatibility:** the COMPAT.md notes "The `vector` extension is
compatible with libSQL native vector search" — so the SQL we'd write
against tursogo is forward-compatible if we ever switched back.

### On-disk format

Confirmed via the "Beyond FTS5" blog post and the docs index: turso is a
ground-up Rust rewrite, not a SQLite fork. Its FTS storage uses Tantivy
segments inside turso's own B-Tree, not FTS5's shadow tables. There is **no
forward compatibility with existing libSQL `.db` files**.

**Migration impact:** Any deployment carrying state (a populated
`deadzone.db` from the scraper) will need to re-scrape after the cut-over.
For a research/dev tool that scrapes from a small fixed corpus this is
cheap, but it should be called out in the migration commit.

---

## Risks ranked (updated for the vector-first plan)

| # | Risk | Likelihood | Severity | Mitigation |
|---|---|---|---|---|
| 1 | No vector index → linear scan as corpus grows | Certain | Low (small corpus) | Cap corpus size; revisit when turso ships IVF/HNSW |
| 2 | Tracking BETA `tursogo` driver | High | Medium | Pin to `v0.5.3` in go.sum; document upgrade path |
| 3 | Re-scrape required on cut-over | Certain | Low | Call out in commit; scraper is fast on go-sdk corpus |
| 4 | Embedding model dependency (MLX / Jina) added to scraper | Certain | Medium | Stub vectors first; layer in MLX behind a flag |
| 5 | `purego` issues on macOS arm64 (rare but real) | Low | Medium | Test on the dev box before merging |
| 6 | We lose pure-keyword recall (`AddTool` → only the literal string match) | Medium | Low | Hybrid retrieval later: vector + LIKE / `instr` for known identifiers |

The original FTS-related risks (compile-time gating, accent folding, the
weak `TestSearch_UnicodeContent` test) **no longer apply** — we're
abandoning FTS, not migrating it. They're preserved in git history for
context but should not influence the spike.

---

## Tokenizer pick — N/A under vector-first plan

The original draft of this doc weighed turso's 5 tokenizers (`default`,
`simple`, `whitespace`, `ngram`, `raw`) and recommended `default` as the
least bad replacement for `unicode61`. **Under the vector-first plan this
section is moot** — we don't use FTS at all. The notes are kept in git
history in case we ever need a hybrid mode.

---

## Open questions for the user

1. **OK with vector-first instead of "drop-in FTS5 → tursogo FTS" swap?**
   This is the real question. The migration becomes a feature change, not
   a driver swap. Search behavior shifts from keyword to semantic.
2. **Embedding model choice now or later?** Easiest path: stub vectors
   first (deterministic hash → float array) to prove the schema, then
   layer in MLX/Jina later. Otherwise we couple this PR to the MLX bringup.
3. **Re-scrape on cut-over: confirmed acceptable?** Assumed yes.
4. **Pin to BETA `v0.5.3` of `tursogo`?** vs waiting for a stable tag.

---

## Recommended next steps (in order, vector-first plan)

1. **Vector probe** (10 min) — standalone Go program that:
   - opens an in-memory `tursogo` DB
   - creates `embedding F32_BLOB(5)`
   - inserts a `vector('[0.1, 0.2, 0.3, 0.4, 0.5]')`
   - runs `vector_distance_cos(embedding, vector('[0.2, 0.3, 0.4, 0.5, 0.6]'))`

   Expected: a float in `(0, 1)`. If yes, the binding works as documented
   and we can proceed to step 2. The upstream test suite already does this,
   so failure would be very surprising.

2. **Schema rewrite** (~30 min) — update `internal/db/db.go`:
   - drop `CREATE VIRTUAL TABLE docs USING fts5(...)`
   - replace with `CREATE TABLE docs (id INTEGER PRIMARY KEY, lib_id TEXT, title TEXT, content TEXT, embedding F32_BLOB(N))`
   - replace `Insert(doc)` with `Insert(doc, embedding)` taking a `[]float32`
   - replace `Search(query, libID)` with `SearchByEmbedding(queryEmbedding []float32, libID string, k int)` using `ORDER BY vector_distance_cos(embedding, vector(?)) LIMIT ?`
   - keep the existing token-budget logic in `internal/search/search.go`
     intact

3. **Test rewrite** (~20 min) — `internal/db/db_test.go`:
   - drop `TestSearch_UnicodeContent` (FTS-specific, no longer relevant)
   - rewrite `TestSearch_*` to use deterministic stub embeddings
     (e.g., a tiny hash-based embedder) and assert the right snippet ranks
     first
   - keep the round-trip test for `Insert`/read-back

4. **Scraper update** (~20 min) — `cmd/scraper/main.go`:
   - take an `Embedder` interface so we can stub it in tests
   - default impl: stub (deterministic hash) to keep this PR self-contained
   - leave a TODO for the MLX/Jina embedder behind a build tag or flag

5. **Server update** (~10 min) — `cmd/server/main.go` /
   `internal/search/search.go`:
   - `handleSearchDocs` needs to embed the incoming query string with the
     same `Embedder` before calling `db.SearchByEmbedding`

6. **Cleanup** — drop `CGO_ENABLED=1` from `.envrc`, update README and
   CLAUDE.md to reflect "Turso (CGO-free) + native vector search". Verify
   `go build ./...` works without `CGO_ENABLED=1`.

7. **Bench** — time the scrape + index loop on the go-sdk corpus (with
   stub embeddings, since real embeddings would dominate the wall time and
   make the comparison meaningless). Compare to the current libSQL+FTS5
   path to make sure we haven't introduced a perf cliff.

If you'd rather do a smaller, lower-risk change first, steps 1-3 alone
(driver swap + vector schema + tests with stub embeddings) form a coherent
PR that proves the architecture works without committing to the embedding
pipeline.

---

## Sources

- Repo: https://github.com/tursodatabase/turso
- Go binding: https://github.com/tursodatabase/turso/tree/main/bindings/go
- `bindings/go/go.mod`: https://github.com/tursodatabase/turso/blob/main/bindings/go/go.mod
- `bindings/go/README.md`: https://github.com/tursodatabase/turso/blob/main/bindings/go/README.md
- `bindings/go/driver_db.go`: https://github.com/tursodatabase/turso/blob/main/bindings/go/driver_db.go
- FTS docs: https://github.com/tursodatabase/turso/blob/main/docs/fts.md
- Manual: https://github.com/tursodatabase/turso/blob/main/docs/manual.md
- "Beyond FTS5" blog post: https://turso.tech/blog/beyond-fts5
- Vector functions reference: https://github.com/tursodatabase/turso/blob/main/docs/sql-reference/functions/vector.mdx
- COMPAT.md (vector section): https://github.com/tursodatabase/turso/blob/main/COMPAT.md
- `bindings/go/driver_db_test.go::TestVectorOperations`: https://github.com/tursodatabase/turso/blob/main/bindings/go/driver_db_test.go
- pkg.go.dev (vanity path): https://pkg.go.dev/turso.tech/database/tursogo
- pkg.go.dev (github path): https://pkg.go.dev/github.com/tursodatabase/turso/bindings/go
- Old driver being replaced: https://github.com/tursodatabase/go-libsql
- Existing vector-search rationale: `docs/research/context7-analysis.md`
