# Batch scrape pipeline via GitHub Actions matrix

> **Status**: research, locked 2026-04-16. Drives the implementation work in #126 (child of #53). Supersedes the original workflow sketch in #53's body (which assumed the per-artifact `packs` release flow paused by #101).
>
> **Cross-references**:
> - [`ingestion-architecture.md`](ingestion-architecture.md) — the canonical decision log for the ingestion pipeline this workflow automates
> - #53 — research parent (body revised 2026-04-16 to link here)
> - #101 — the `deadzone.db`-only pivot that reshaped the publish target
> - #47 — freshness detection (not yet landed; see "Deferred to #47" below)

---

## 0. Context

Issue #53 proposed using GH-hosted Linux runners to batch-scrape the registry in parallel, producing per-lib `.db` files pushed to a rolling `packs` GitHub Release. That target release flow no longer exists: #101 (merged 2026-04-13) paused per-artifact distribution entirely, leaving `deadzone dbrelease` as the sole publish path, operator-driven, producing a single `deadzone.db` asset per tag.

The underlying idea — free matrix-parallel batch scrape — is still sound. What changes is the landing: instead of uploading N per-lib artifacts to a rolling release, the workflow consolidates everything into a single `deadzone.db` and publishes via `dbrelease` only when the operator explicitly passes a `tag` input.

Registry size today: 12 top-level `lib_id` entries in `libraries_sources.yaml` resolving to ~20 (lib, version) pairs. Well within GH Actions matrix limits (256/workflow on free plans); fan-out concerns are not load-bearing at this corpus size.

---

## 1. Decision (locked 2026-04-16)

| # | Decision | Reason |
|---|---|---|
| 1 | **Publish target: single `deadzone.db`** via `deadzone dbrelease` | Aligns with #101's operator-driven single-DB flow; no revival of paused per-artifact distribution |
| 2 | **Trigger: `workflow_dispatch` only**, no cron | Freshness policy is #47's job; until it lands, cron risks publishing stale rescrapes on a fixed cadence |
| 3 | **Inter-job transport: `actions/cache@v5`**, not `actions/upload-artifact` | Cache persists across runs, turning the matrix into a zero-code-change freshness shim: a lib whose config hash hasn't changed is skipped entirely on the next run |
| 4 | **Publish conditional on `inputs.tag`** | Without a tag, the workflow stops at a consolidated `deadzone.db` cache; with a tag, it enchaînes `dbrelease`. Keeps the operator in the loop for the destructive step |
| 5 | **No revival of `internal/packs/upload.go`** | The paused per-artifact distribution stays paused. If/when it comes back, it's a separate issue |

---

## 2. Architecture

```
┌─────────────────┐   ┌─────────────────────────────┐   ┌────────────────┐
│  expand-libs    │   │  scrape  (matrix)           │   │  consolidate   │
│  runs on-demand ├──▶│  max-parallel: 20           ├──▶│  restore N     │
│  via dispatch   │   │  fail-fast: false           │   │  caches        │
│                 │   │  ┌─────────────────────┐    │   │  → consolidate │
│  reads          │   │  │ per-slot steps:     │    │   │    artifacts/  │
│  libraries_     │   │  │ 1. checkout         │    │   │    → deadzone  │
│  sources.yaml   │   │  │ 2. setup-go         │    │   │      .db       │
│                 │   │  │ 3. restore hugot    │    │   │  if tag:       │
│  emits JSON     │   │  │ 4. restore ORT      │    │   │    dbrelease   │
│  [{lib,version,│   │  │ 5. restore artifact │    │   │  else:         │
│    slug}]       │   │  │    cache (by slug) │    │   │    stop        │
│                 │   │  │ 6. if miss: scrape  │    │   │                │
│                 │   │  │ 7. save artifact    │    │   │  summary:      │
│                 │   │  │    cache            │    │   │  $GITHUB_STEP_ │
│                 │   │  └─────────────────────┘    │   │  SUMMARY table │
└─────────────────┘   └─────────────────────────────┘   └────────────────┘
```

Hot path:
1. `expand-libs` calls the scraper's config resolver (`scraper.LoadConfig` + `Resolve` in `internal/scraper/config.go`) to emit the matrix.
2. Each scrape slot checks its cache key before invoking `just scrape lib=<id>`. Cache hit → noop. Cache miss → run scrape, write to `artifacts/<slug>/`, let `actions/cache/save` persist it.
3. `consolidate` restores all N lib caches into a unified `artifacts/`, runs `just consolidate`, produces `deadzone.db`.
4. If `inputs.tag` provided, invoke `deadzone dbrelease --db deadzone.db --tag <tag>`. Otherwise exit 0 with `deadzone.db` in the final cache (not uploaded anywhere public).

---

## 3. Cache strategy (pivot clé)

Cache key per matrix slot:

```
artifact-<slug>-<version>-${{ hashFiles('libraries_sources.yaml') }}-${{ hashFiles('internal/embed/hugot.go') }}
```

- **`<slug>`** — `packs.Slug(libID)` from `internal/packs/paths.go` (e.g. `/modelcontextprotocol/go-sdk` → `modelcontextprotocol_go-sdk`)
- **`<version>`** — version tag from `Resolve`, or a sentinel for libs without a `versions:` shorthand
- **`hashFiles('libraries_sources.yaml')`** — invalidates when URL lists, kinds, or version shorthands change (config bump → rescrape touched libs)
- **`hashFiles('internal/embed/hugot.go')`** — invalidates when the embedder is swapped (vector space incompatibility — see `ingestion-architecture.md` §8). Same hash the pre-existing `hugot-model-…` cache key uses in `.github/workflows/ci.yml` L114–130, so bumping the embedder invalidates BOTH caches in lockstep
- **Cached path** — `artifacts/<slug>/` (contains `artifact.db` + `state.yaml`, following the #101 folder-per-lib layout)

Pre-existing cache keys to reuse **verbatim** (also in `ci.yml`):

```
hugot-model-${{ runner.os }}-${{ hashFiles('internal/embed/hugot.go') }}
ort-lib-${{ runner.os }}-${{ hashFiles('internal/ort/ort.go') }}
```

### Invalidation properties

| Trigger | Result |
|---|---|
| Operator edits `libraries_sources.yaml` | All libs miss — full rescrape on next run |
| Embedder bump (`internal/embed/hugot.go`) | All libs miss AND hugot/ORT caches miss — full rescrape + model re-download |
| Source doc rafraîchi upstream (no config change) | Cache hit — **does NOT capture** this. Intentional gap; #47 will add the signal |
| Cold cache (first run) | All miss, first run pays full scrape cost |

The "does NOT capture upstream changes" gap is the honest limit of this design. A semantically complete freshness layer (#47) will add a per-URL content hash or ETag to the cache key; until then, weekly wipe via `Actions cache management` UI is the workaround.

### Cache keepalive

GitHub Actions evicts cache entries not accessed in **7 days**. Since `scrape-pack.yml` is `workflow_dispatch`-only (decision #2), any operator silent for a week loses every `artifact-<slug>-<version>-…` entry — defeating the freshness-shim property above. `.github/workflows/cache-keepalive.yml` (see #128) fires `on.schedule: '0 4 * * 1,4'` (Mon + Thu 04:00 UTC, max gap 4 days under the 7-day ceiling) and touches each lib cache via `actions/cache/restore@v5` with the key **mirrored verbatim** from `scrape-pack.yml` L129-130 — drift would create a distinct cache entry and silently skip the refresh. Keepalive is orthogonal to #47: it refreshes `last_accessed_at`, not content. A miss (cache already evicted) is not a failure; the lib simply rescrapes fully on the next operator dispatch. When #47 lands with per-URL freshness signals, the keepalive may be superseded.

---

## 4. Fan-in pattern in the `consolidate` job (open sub-decision)

Restoring N dynamic caches in a single job is the awkward step. `actions/cache/restore@v5` expects a compile-time `key:` — it does not batch-restore.

Three candidate patterns, to be finalized at implementation time:

### Pattern A — composite action

Ship `.github/actions/restore-lib-cache/action.yml` wrapping `actions/cache/restore@v5`. Call it N times from the consolidate job, one per expanded lib, generated via a YAML `jobs.consolidate.steps:` list that is itself produced by an earlier `run: bash` that emits a workflow file fragment.

**Pros**: stock GH Actions primitives, no REST plumbing.
**Cons**: YAML generation inside a workflow is a code smell and GHA does not support dynamic step injection. Practically this means Pattern A devolves into a `matrix` on the consolidate job (Pattern B).

### Pattern B — consolidate as matrix + upload-artifact staging

Run the consolidate job itself as a matrix (same list as scrape). Each slot restores one lib's cache, `upload-artifact`s the restored `artifacts/<slug>/`. A final `consolidate-final` job downloads all artifacts and runs `just consolidate`.

**Pros**: simple, stock, works today.
**Cons**: **reintroduces `upload-artifact` between jobs** — directly contradicts decision #3. Only acceptable if Pattern C proves unworkable.

### Pattern C — REST cache API in a single consolidate job

Single `consolidate` job runs a bash step that:
1. Reads `needs.expand-libs.outputs.libs` (JSON list)
2. For each entry, computes the same cache key the scrape slot used
3. Calls `gh api /repos/:owner/:repo/actions/caches?key=<key>` to look up the cache archive URL
4. Downloads each archive to `artifacts/<slug>/`
5. Runs `just consolidate`

**Pros**: respects decision #3 (zero upload-artifact). Single job, linear flow.
**Cons**: the GH REST endpoint for cache *downloads* is not officially documented for this use case — the `/actions/caches` endpoint lists cache metadata, but retrieving the archive content typically goes through an internal signed URL that the `actions/cache` action negotiates. Implementation risk: the endpoint may require undocumented headers or may not exist for cross-job cache reads.

### Recommendation

**Start with Pattern C.** If implementation proves the REST download path is not viable, fall back to Pattern B and document the deviation from decision #3 in the PR body. Do **not** build Pattern A — it isn't a real pattern, it's a sketch that collapses into B.

---

## 5. Composition with existing code

| Workflow step | Invokes | Reuses |
|---|---|---|
| `expand-libs` | New `deadzone scrape --list` flag (minimal JSON emitter wrapping `scraper.Resolve`) | `internal/scraper/config.go`: `LoadConfig`, `Resolve` |
| scrape slot (cache miss) | `just scrape lib=<id>` (currently `mise exec -- go run -tags ORT ./cmd/deadzone scrape --artifacts ./artifacts --lib <id>`) | `cmd/deadzone/scrape.go` flag surface (`--lib`, `--version`, `--config`, `--artifacts`, `--parallel-*`) |
| cache key path | `artifacts/<slug>/` | `internal/packs/paths.go`: `packs.Slug`, `packs.ArtifactDir` |
| hugot/ORT cache | Cache keys verbatim from `ci.yml` L114–130 | No change |
| consolidate | `just consolidate db=deadzone.db` | `cmd/deadzone/consolidate.go` flag surface (`--db`, `--artifacts`) |
| publish (if `inputs.tag`) | `mise exec -- go run -tags ORT ./cmd/deadzone dbrelease --db deadzone.db --tag <tag>` (equivalent to `just dbrelease <tag>`) | `cmd/deadzone/dbrelease.go`, `internal/packs/releaser.go` (`GHReleaser`) |

The workflow does **not** introduce new Go code except the minimal `--list` emitter. Everything else is orchestration of flags and recipes that already ship.

---

## 6. Holds at scale

| Scale | Sanity check |
|---|---|
| 20 libs today | Warm run: ~60s per slot (cache hit path is ~5s + matrix overhead). Cold run: 5–10 min dominated by first-time scrape |
| 200 libs | Matrix cap is 256 concurrent jobs on free plan. At `max-parallel: 20` this serializes in 10 waves. Still well under 6h job ceiling |
| 2000 libs | Matrix cap exceeded — would need to chunk the expansion into waves. Revisit when the registry approaches that size. Not an architectural concern, an orchestration tweak |
| 33k libs (Context7-scale) | Would require a different orchestration model (sharded scheduling, per-shard workflow, shared KV state). Out of scope here — this issue targets the 20–2000 range |

Cache size: each `artifacts/<slug>/` is on the order of 1–10 MB. GitHub gives 10 GB of cache per repo, evicting LRU. At 200 libs × 10 MB = 2 GB, still comfortable.

---

## 7. Out of scope (fencing for the implementation issue)

- **Cron triggers** — deferred until #47 adds a freshness signal worth firing on
- **Per-artifact distribution revival** — `internal/packs/upload.go` stays disabled (#101)
- **Self-hosted runners** — no demand; file separately if someone needs it for non-public libs
- **Per-PR ephemeral packs** — interesting but different shape (ephemeral preview vs scheduled batch); separate issue
- **Cost / budget reporting** — public repo Linux Actions are free
- **`release.yml` changes** — binary distribution flow is out of scope; this workflow adds a parallel flow for `deadzone.db`, not a replacement

---

## 8. Deferred to #47

- Per-URL content-hash or ETag contribution to the cache key (so upstream doc changes invalidate per-lib caches automatically)
- Per-lib refresh cadence hints (some libs change daily, others yearly)
- Automated cache purge trigger when a lib's upstream source is detected as stale

Until #47 lands, the operational workaround is manual cache invalidation via the GitHub UI's `Actions → Caches` panel, or by editing `libraries_sources.yaml` (which auto-invalidates via the config hash).
