# Go stdlib + pkg.go.dev ingestion

> **Status**: research, locked 2026-05-04. Tracked in #133. Filed against milestone 0.6.
>
> **Cross-references**:
> - [`ingestion-architecture.md`](ingestion-architecture.md) decision 1 — the canonical source-kinds enumeration this note extends
> - [`embedder-choice.md`](embedder-choice.md) — the embedder-side context (chunks land in the same `db.Doc` shape, embedded with `EmbedDocument` + `"search_document: "` prefix)
> - #27 — `scrape-via-agent` infrastructure (option 2's substrate)
> - #95 — `github-rst` precedent for a per-shape kind addition
> - #103 — `ref:` reproducibility pin (re-used by the picked option for stdlib)
> - #46 — `github-glob` (related but different: glob is on GitHub, godoc is on Go source)
> - The tursogo registry research (Go's sister issue, filed same day) — same pattern of "the registry's own language is a special case"

---

## TL;DR — decision

**Add a fourth source kind: `godoc`.** Parses `.go` files via the `go/doc` stdlib package, produces one `db.Doc` per exported identifier (funcs, types, type-methods, consts, vars) plus one for the package overview. Source-bytes adapter is split by lib shape:

- **Go stdlib (`golang/go`)** — fetch via the existing `github-md`-style raw HTTP path, pinning the source tree at a tag via `{ref}` from #103. The stdlib is not a Go module, so `proxy.golang.org` does not serve it.
- **Third-party Go modules** — fetch the `.zip` archive from the **Go Module Proxy Protocol** (`proxy.golang.org/<module>/@v/<version>.zip`), checksum-verified via `sum.golang.org`. Officially supported, immutable per (module, version), Google-Cloud-backed.

Both shapes feed the **same `go/doc` parser** producing the **same chunk shape**. The `kind: godoc` enum entry stays small (one parser, one tester, no plugin interface) — consistent with decision 1's "never per-source code" principle.

Option 3 (pkg.go.dev JSON spike) is dead: `pkg.go.dev` returns HTML regardless of `?format=json` or `Accept: application/json` (probed 2026-05-04). Option 2 (`scrape-via-agent` against pkg.go.dev HTML) was rejected on cost-at-scale grounds. Option 4 (hybrid `github-md` + `godoc`) collapses to option 1 because `golang/go` ships only 2 narrative `.md` files (`doc/README.md`, `doc/godebug.md`) — the `github-md` half is anecdotal.

---

## 0. Context

Go is the language deadzone is written in, and the natural first-party candidate for a "fully self-referential" lib in the registry. The corpus targets — stdlib first, then popular third-party modules at Context7-scale — share a documentation source that none of the existing kinds maps onto:

| Existing kind | Fit for Go | Why |
|---|---|---|
| `github-md` | ❌ thin | `golang/go` ships **2 `.md` files** in `/doc/` (confirmed 2026-05-04 against `master`: `README.md` + `godebug.md`). The Go language spec is `/doc/go_spec.html`, not markdown. The `doc/initial/*.md` files are draft release notes, not library docs |
| `github-rst` | ❌ wrong shape | Go does not ship reStructuredText anywhere |
| `scrape-via-agent` | ⚠️ works but expensive | `go.dev/doc/` and `pkg.go.dev` are valid HTML targets; #27's pipeline can extract them. But `pkg.go.dev` has ~195 stdlib package pages × N versions, and burns LLM tokens on every re-scrape |

The actual source of truth for Go documentation is **godoc**: comments in `.go` files, parsed by the `go/doc` stdlib package, rendered by `pkg.go.dev` for human consumption. The pipeline-shape question is: do we ingest the rendered HTML, or the underlying source?

---

## 1. Options considered

| Option | Approach | Source bytes | Parser | LLM cost / re-scrape | New code surface |
|---|---|---|---|---|---|
| **1 — `godoc` kind** | Parse `.go` files directly via `go/doc` | stdlib: GitHub raw + `{ref}`. Third-party: `proxy.golang.org/.../@v/<v>.zip` (signed, immutable) | new `ParseGodoc` (~150 LOC) | **zero** | one new kind, one new parser, one new sumdb-verify helper |
| **2 — `scrape-via-agent` on `pkg.go.dev`** | Reuse #27 against rendered HTML pages | `pkg.go.dev/<package>` HTML | existing `ParseAgent` | **N pages × LLM tokens × N rescrapes** | none (config-only addition) |
| **3 — `pkg.go.dev?format=json`** | Hypothetical structured-data endpoint | `pkg.go.dev/<pkg>?format=json` | new `ParseJSON` if endpoint exists | zero | trivial config bump |
| **4 — Hybrid `github-md` + `godoc`** | Two `lib_id` entries per Go lib: narrative `.md` + API godoc | both | both | low | union of #1 + zero |

### Option 3 — the spike

Probe (2026-05-04):

```
$ curl -sIL 'https://pkg.go.dev/encoding/json?format=json'
HTTP/2 200
content-type: text/html; charset=utf-8

$ curl -sIL 'https://pkg.go.dev/encoding/json' -H 'Accept: application/json'
HTTP/2 200
content-type: text/html; charset=utf-8

$ curl -s 'https://pkg.go.dev/encoding/json?format=json' | head -c 60
<!DOCTYPE html>
<html lang="en" data-layout="responsive"
```

**`?format=json` is silently ignored. `Accept: application/json` is silently ignored. `pkg.go.dev` exposes no JSON endpoint.** Option 3 is dead, file no follow-up.

### Option 2 — why rejected

`scrape-via-agent` works against `pkg.go.dev` HTML (the agent path is shape-agnostic), but the cost model breaks at the corpus targets:

- **Stdlib**: 195 user-facing packages (counted at `GOROOT/src` excluding `internal/`, `vendor/`, `testdata/`, `cmd/` on Go 1.26.2). Across 5 supported Go versions (Go's stdlib is documented per release), that's **~975 LLM extractions per full re-scrape** of the stdlib alone.
- **Third-party Context7-scale target**: ~2000 popular Go modules × N versions × N pages per module ≈ tens of thousands of LLM calls per refresh.
- **Per-page noise**: `pkg.go.dev` pages carry sidebar nav, version-picker, "Imported by" widgets, syntax-highlighting `<span>` wraps. The agent's verifier (#7 strict-byte-substring, decision 7 in `ingestion-architecture.md`) already mis-rejects valid extractions on dense doc sites — `pkg.go.dev` is exactly the shape that surfaces #64's loosening problem. We'd be eating the cost of #64 to use option 2.
- **No reproducibility win**: `pkg.go.dev` URLs already embed `@<version>` so version-pinning is fine, but the LLM extraction itself is non-deterministic (different runs produce different markdown shapes). `{ref}` pinning from #103 doesn't help when the parser is an LLM.

The agent path remains the right tool for sites with no structured source (terraform.io, FastAPI's mkdocs-material). Go is not one of those sites — the structured source is `go/doc` itself.

### Option 4 — why it collapses

The `github-md` half of a hybrid would cover narrative content like the language spec or "Effective Go". But:

- The Go language spec ships as `doc/go_spec.html` — HTML, not markdown.
- "Effective Go" lives at `go.dev/doc/effective_go` — HTML, not markdown.
- Confirmed `.md` files in `golang/go`: `README.md`, `doc/README.md`, `doc/godebug.md`, `CONTRIBUTING.md`, `SECURITY.md`, plus 60+ `.github/`, `doc/initial/*` (release notes drafts), and `internal/**/README.md` files. **Total user-facing narrative `.md`: 2 files** (`doc/README.md` is a 4-line index of the directory; `doc/godebug.md` is the GODEBUG flag reference).

The `github-md` component would index 2 files. The `godoc` component would index 195 packages × ~50 chunks each. The hybrid is option 1 plus a rounding error. Adding a second `lib_id` per lib for two files breaks the "one user-typed identifier → one search target" property of `search_libraries` for no measurable retrieval gain.

If the spec or "Effective Go" ever migrate to `.md`, revisit. Not the bet today.

---

## 2. Decision (locked 2026-05-04)

| # | Decision | Reason |
|---|---|---|
| 1 | **Add `kind: godoc`** to the enumerated source kinds (joins `github-md`, `github-rst`, `scrape-via-agent`) | Existing kinds don't map onto Go's source-comments-as-docs model; per-source code is rejected by decision 1 of `ingestion-architecture.md` |
| 2 | **Source-bytes adapter is shape-split**, **parser is unified** | stdlib is not a Go module → no `proxy.golang.org`, fall back to GitHub raw. Third-party modules → `proxy.golang.org` (official, signed). Both produce a `[]*ast.File` that the same `go/doc` call consumes |
| 3 | **One `db.Doc` per exported identifier + one for the package overview** | Matches the chunk-shape contract (`db.Doc{Title, Content}`) and aligns with how `pkg.go.dev` itself structures human-readable docs |
| 4 | **`{ref}` pinning from #103 applies** | For stdlib, `{ref}` resolves to a Go release tag (e.g. `go1.26.2`). For third-party, `{ref}` is the module version that drives the `proxy.golang.org/.../@v/<ref>.zip` URL |
| 5 | **Checksum verification via `sum.golang.org`** for third-party modules | Module proxy archives are immutable per (module, version) AND signed in the transparency log. Verifying matches the "fail fast and loud" principle (decision 9 cross-cutting in `ingestion-architecture.md`) |
| 6 | **No second `lib_id` for narrative content** | Hybrid (option 4) collapses to option 1 because `golang/go` ships ~zero narrative markdown. Revisit if the spec or "Effective Go" land in `.md` |

---

## 3. Architecture sketch (informative — implementation lives in the follow-up issue)

```
┌──────────────────────┐         ┌────────────────────────┐         ┌──────────────────┐
│   libraries_         │         │  fetch (kind: godoc)   │         │  parse (godoc)   │
│   sources.yaml       │  YAML   │                        │ []byte  │                  │
│                      ├────────▶│  if /<host>/golang/go: │────────▶│  go/parser       │
│  - lib_id: /go       │         │    GitHub raw +        │         │   .ParseDir()    │
│    kind: godoc       │         │    {ref} substitution  │         │                  │
│    urls: [...]       │         │                        │         │  go/doc          │
│    ref: go1.26.2     │         │  else (third-party):   │         │   .New(pkg, ...) │
│                      │         │    proxy.golang.org/   │         │                  │
│  - lib_id: /spf13/   │         │    <mod>/@v/<ref>.zip  │         │  per identifier: │
│    cobra             │         │    + sum.golang.org    │         │    db.Doc{       │
│    kind: godoc       │         │    SHA256 verify       │         │      Title,      │
│    ref: v1.10.2      │         │                        │         │      Content,    │
│                      │         └────────────────────────┘         │    }             │
└──────────────────────┘                                            └─────────┬────────┘
                                                                              │
                                                                              ▼
                                                              same downstream as #27/#95:
                                                              chunk → embed → store in
                                                              artifacts/<slug>/artifact.db
```

**Hot path**:

1. Resolver in `internal/scraper/config.go` recognizes `kind: godoc`; `{ref}` is resolved from the `versions:` map (via the per-version `ref:` from #120) or from the lib-level `ref:`.
2. Source-bytes adapter dispatches on `lib_id` shape:
   - `lib_id == "/golang/go"` (and any future stdlib-bundle entries) → walk the configured `urls:` against GitHub raw with `{ref}` substitution, exactly like `github-md` does today.
   - Anything else → resolve the module path (typically `lib_id` minus the leading `/`), call `proxy.golang.org/<escaped_mod>/@v/<ref>.zip`, verify against `sum.golang.org/lookup/<mod>@<ref>`, unpack into a temp tree.
3. `ParseGodoc(srcDir)` walks the temp tree, runs `go/parser.ParseDir` filtering out `_test.go`, hands the resulting `*ast.Package` to `go/doc.New`, and emits one `db.Doc` per exported identifier:
   - Package overview: `Title = "<pkg> package"`, `Content = doc.Doc + per-Note rollup`
   - Funcs: `Title = "<pkg>.<Name>"`, `Content = doc.Func.Doc + signature`
   - Types: `Title = "<pkg>.<Name>"`, `Content = doc.Type.Doc + decl + per-method rollup`
   - Consts/Vars: grouped per `*doc.Value`, one chunk per group (matches how `go/doc` already groups them)
4. Chunks flow into the existing embed pipeline unchanged. The `Embedder.EmbedDocument` path applies the `"search_document: "` prefix expected by `nomic-embed-text-v1.5` (decision 8 in `ingestion-architecture.md`).

The `godoc` parser **does not call out to any LLM**, **does not require CGO**, **does not require a Go toolchain at runtime** (the `go/parser` and `go/doc` packages are pure Go, embedded in deadzone's own binary). Adding `kind: godoc` does not weaken the "single binary, single tarball" property.

---

## 4. Holds at scale

| Scale | Sanity check | ✅ / ⚠️ / ❌ |
|---|---|---|
| **1 Go module** (e.g. `/spf13/cobra`) | Single zip fetch (~5 MB) + `go/parser.ParseDir` over a few dozen files + ~80 chunks emitted. Wall-clock budget: <5s including network. | ✅ |
| **150 stdlib packages** (the 0.6 stretch goal) | One `golang/go` source-tree fetch per Go release (tarball ~80 MB cached for the run), 150 × `go/parser.ParseDir` invocations on local files, ~7,500 chunks. | ✅ |
| **195 user-facing stdlib packages** (actual count, Go 1.26.2) | Same fetch shape, ~10,000 chunks. Embeds in <2 min on the dev laptop benchmarked in #67. | ✅ |
| **2,000 third-party Go modules** (Context7-scale target) | 2,000 × `proxy.golang.org` zip fetches (~10 MB each ≈ 20 GB transient), parallelizable per-lib (decision 2), ~100,000 chunks. `proxy.golang.org` is Google-Cloud-backed; rate limits are not documented but are observed at ~100 req/s by the broader Go ecosystem. | ✅ |
| **33,000 modules** (Context7-scale) | 33k zips ≈ 330 GB transient, 1.6M chunks. Linear vector scan exits the sub-second regime — but that's #45's concern, not this kind's. The parser cost is linear in chunk count and dominated by fetch. | ⚠️ orchestration tweak (matrix chunking from `batch-scrape-actions.md` decision applies), not architectural |

### Empirical chunk-count sample (Go 1.26.2)

Measured 2026-05-04 against `GOROOT/src/<pkg>` via a 30-line Go program calling `go/parser.ParseDir` + `go/doc.New`:

| Package | Funcs | Types | Methods+Ctors | Pkg-doc bytes | Total chunks |
|---|---|---|---|---|---|
| `context` | 8 | 3 | 4 | 2,461 | 18 |
| `fmt` | 23 | 6 | 0 | 15,649 | 30 |
| `sync` | 3 | 8 | 31 | 340 | 43 |
| `io` | 8 | 27 | 22 | 445 | 65 |
| `encoding/json` | 7 | 17 | 43 | 11,647 | 68 |
| `os` | 51 | 12 | 79 | 1,493 | 156 |
| `net/http` | 21 | 30 | 115 | 3,047 | **190** |

Median per-package: ~50 chunks. Tail dominated by `net/http`-class packages with deep type hierarchies. Stdlib total estimate: 195 × 50 ≈ **10k chunks for the user-facing stdlib at one Go version**.

For comparison, the largest existing lib in `libraries_sources.yaml` produces ~700 chunks (decision 8 long-text embed numbers from #67). The stdlib at one version is ~14× the largest current lib, but spread over 195 lib_ids if we choose to expand each stdlib package as its own `lib_id` (see "Open question 2" below).

---

## 5. Out of scope (fenced for the follow-up issue)

- **Granularity decision** — whether `golang/go` is one `lib_id` with 195 sub-packages, or 195 separate `lib_id`s. Both are reachable from this design; the choice belongs in the implementation issue with empirical input from `search_libraries` retrieval quality at both granularities.
- **Multi-version stdlib indexing** — Go ships a stdlib per release. Whether deadzone tracks Go 1.24/1.25/1.26 or only the latest is a registry policy, not a kind-design question.
- **`go.mod` graph traversal** — extracting transitive deps from a module is out of scope; the registry stays declarative (operator picks which modules are indexed).
- **`pkg.go.dev` "Examples" section** — `go/doc` exposes `Examples` from `_test.go` files via `doc.Examples()`. Useful but adds a second walk and complicates the chunk shape. Defer to a follow-up if retrieval quality on Go libs surfaces gaps.
- **CGO-flagged packages** — `go/parser` doesn't care about CGO; it parses the `.go` source. The CGO file portions are skipped silently (they're not Go syntax). Documented but no special handling needed.
- **Generated code** — packages whose `.go` is generated (gRPC, protobuf bindings) will be parsed verbatim. Filtering out `// Code generated by ...; DO NOT EDIT.` files is a nice-to-have for the follow-up, not a blocker.
- **Vendored dependencies inside a module's zip** — `proxy.golang.org` zips contain only the module's own files (vendored deps live in the consumer, not the provider), so this is mostly a non-issue. Belt-and-suspenders: skip `vendor/` directories during `ParseDir`.
- **Comparison with Context7's Go coverage** — referenced in prose; no benchmark in this issue per #133's "out of scope" fence.
- **Embedder changes** — Go docs are regular English text intermixed with code identifiers; `nomic-embed-text-v1.5` handles them as well as it handles any other technical doc (decision 8 caveats).

---

## 6. Open questions for the follow-up issue

1. **Stdlib granularity** (one `lib_id` vs 195) — decide empirically against `search_libraries("encoding json")` and `search_libraries("net http")`.
2. **`{ref}` shape for stdlib** — `golang/go` git tags are `go1.26.2`-style. The existing `ref:` format is freeform string substitution; verify URL templates compose cleanly.
3. **`proxy.golang.org` failure modes** — sumdb timeouts, withdrawn modules, modules that disappeared post-`@latest` resolution. Inherit the "fail fast and loud" principle: surface a typed error and skip the lib for the run rather than silently emitting an empty artifact.
4. **`go/doc` AST stability** — Go 1 compatibility promise covers `go/doc`'s public API but minor formatting changes between Go versions will produce different chunk content. Track which Go version the deadzone binary is built against; an embedder-style mismatch sentinel is overkill, but logging the toolchain version into `state.yaml` is cheap insurance.

---

## 7. Trace

- Issue: #133 (research, P2, milestone 0.6, 2026-04-29 → 2026-05-04)
- Spikes (2026-05-04):
  - `pkg.go.dev` JSON endpoint probe → ❌ HTML only (option 3 dead)
  - `proxy.golang.org` + `sum.golang.org` reachability → ✅ 200 OK, signed archives confirmed
  - `golang/go` `.md` count → 2 user-facing files, confirms issue body
  - `go/doc` chunk-count sweep over 7 stdlib packages → median ~50 chunks/pkg
- Pattern: this doc follows [`batch-scrape-actions.md`](batch-scrape-actions.md) and [`embedder-choice.md`](embedder-choice.md) — TL;DR + Options + Decision + Holds-at-scale + Out-of-scope
- Follow-up implementation issue: **#198** (`feat(scraper): implement kind: godoc for Go stdlib + third-party modules`)

---

## 8. Related

- [`ingestion-architecture.md`](ingestion-architecture.md) decision 1 — the source-kinds enumeration (one-line addendum landing in the same PR as this doc)
- #27 — `scrape-via-agent` (option 2's substrate, the rejected alternative)
- #46 — `github-glob` (related but different problem shape)
- #95 — `github-rst` (precedent for "add a kind for one specific source shape")
- #103 — `ref:` reproducibility pin (re-used here for stdlib + third-party)
- #120 — version-identifier / git-tag split (the per-version `ref:` shape that drives `proxy.golang.org` URLs)
- Go Module Proxy Protocol — <https://go.dev/ref/mod#module-proxy>
- Go checksum database — <https://sum.golang.org>
