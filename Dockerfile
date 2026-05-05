# syntax=docker/dockerfile:1.7
#
# Deadzone OCI image — packaging only, no compilation. The native
# binaries (one per linux arch) are produced upstream by release.yml's
# `build` job from `just build-release` on a real linux runner; this
# Dockerfile downloads the matching tarball + libonnxruntime, lays them
# out, and emits a distroless image. Multi-arch via `docker buildx
# build --platform linux/amd64,linux/arm64` — TARGETARCH selects the
# right per-arch dist/ subdir at build time.
#
# Why distroless: no shell, no package manager, ~25 MB base. Smaller
# attack surface than debian-slim and the user-facing pitch matches
# what the brew tap / tarball install give: a single binary + libs, no
# system packages.
#
# Why DB baked (#203, reverses #196's no-bake decision):
# `docker run --rm` produces a fresh container per MCP invocation, so
# every Claude Desktop / Cursor / Continue restart with the previous
# no-bake image re-downloaded the ~72 MB deadzone.db from GitHub
# Releases (5-15 s of latency per MCP session start). Baking the DB
# at /home/nonroot/.local/share/deadzone/deadzone.db (the path
# internal/db.DefaultCacheDir resolves to with HOME=/home/nonroot)
# plus DEADZONE_DB_OFFLINE=1 short-circuits internal/db.Bootstrap to
# the baked file, eliminates the auto-fetch on first launch, and
# drops the named-volume workaround from the README's MCP wire-up.
# The "no tag-build race" guarantee from #196 is preserved by moving
# the docker build out of release.yml and into scrape-pack.yml, where
# it runs AFTER `dbrelease` has produced and uploaded the DB.
# Native binaries (Brew tap / tarball / AppImage) are unaffected —
# they continue to use the auto-fetch + boot-time freshness probe
# (#197). Image size grows ~25 MB → ~100 MB, accepted trade-off.

# Stage 1 — collect the per-arch payload from the build context. Alpine
# is the cheapest image with a real `cp`; the staging stage exists so
# the final image carries no traces of the source paths or unused
# context files. Each arch reads from dist/linux_${TARGETARCH}/, which
# CI populates from actions/download-artifact + a curl of the pinned
# libonnxruntime tarball (see release.yml's `Stage build context` step).
#
# TARGETARCH is one of buildx's pre-defined ARGs — auto-populated per
# platform when the build is invoked with `--platform linux/amd64,
# linux/arm64`. It MUST be declared inside this stage (and NOT also at
# the global pre-FROM scope) so the auto-injection isn't shadowed by an
# empty global. https://docs.docker.com/reference/dockerfile/#scope
FROM alpine:3 AS staging
ARG TARGETARCH
WORKDIR /staged
COPY dist/linux_${TARGETARCH}/deadzone /staged/deadzone
# Both libonnxruntime.so (symlink) and libonnxruntime.so.<version>
# (real ELF) must land — distroless has no ldconfig, so dlopen
# resolves whichever name hugot's options.WithOnnxLibraryPath was
# given against the file directly. Globbing keeps the Dockerfile
# version-agnostic so an ORT bump only touches internal/ort.
COPY dist/linux_${TARGETARCH}/lib/ /staged/lib/
# Hugot model weights staged by scrape-pack.yml's `Stage build context`
# step: 6 files under dist/linux_<arch>/models/<dest_dirname>/, where
# dest_dirname matches embed.ModelDestDirname() (the "/" → "_"
# substitution of nomic-ai/nomic-embed-text-v1.5). See #207.
COPY dist/linux_${TARGETARCH}/models/ /staged/models/
COPY LICENSE NOTICE README.md /staged/

# Stage 2 — final distroless image. cc-debian13 (trixie) ships glibc
# 2.41+ which matches what the binary links against on the
# ubuntu-24.04 build runner (glibc 2.39). cc-debian12 (bookworm,
# glibc 2.36) is too old and produces "GLIBC_2.39 not found" at
# `dlopen` time. :nonroot creates uid/gid 65532 with
# HOME=/home/nonroot and an entrypoint that runs as that uid.
FROM gcr.io/distroless/cc-debian13:nonroot
COPY --from=staging /staged/deadzone /usr/local/bin/deadzone
COPY --from=staging /staged/lib/     /usr/local/lib/
COPY --from=staging /staged/LICENSE /staged/NOTICE /staged/README.md /

# DEADZONE_ORT_LIB_PATH is a *directory* (see internal/ort/ort.go's
# Bootstrap — the env var short-circuits to whatever path is given
# verbatim, and hugot's options.WithOnnxLibraryPath walks it for the
# right LibName). Pointing at the .so file itself silently breaks
# resolution. Setting this here means internal/ort.Bootstrap returns
# from cache without making any network call, which is what makes
# `docker run --network none ... --version` a meaningful smoke test.
ENV DEADZONE_ORT_LIB_PATH=/usr/local/lib

# Pin HOME so the XDG fallback in internal/db.DefaultCacheDir lands
# at /home/nonroot/.local/share/deadzone/deadzone.db (writable by the
# nonroot user). Distroless sets HOME implicitly via passwd, but being
# explicit guards against future base-image changes that drop the
# /etc/passwd entry.
ENV HOME=/home/nonroot

# Bake the consolidated deadzone.db at the path internal/db.DefaultCacheDir
# resolves to for the nonroot user (Linux fallback: $HOME/.local/share/deadzone).
# The build context staging step in scrape-pack.yml drops `deadzone.db` at
# the root of the build context — `just docker-build` does the same after
# `just consolidate`. Path matching DefaultCacheDir means the image needs
# no DEADZONE_DB_CACHE override.
COPY deadzone.db /home/nonroot/.local/share/deadzone/deadzone.db

# Bake the cache sidecar alongside the DB. internal/db.Bootstrap reads
# `deadzone.db.release` (a JSON {tag, sha256, fetched_at} object — see
# internal/db/sidecar.go) BEFORE trusting the cached DB: a present DB
# without a matching sidecar tag fails the version-pin check and errors
# out under OFFLINE=1 (bootstrap.go:228-233). The sidecar is generated
# in the docker job from the release tag and the DB's sha256; `dbrelease`
# does not upload it because it's a local-cache artefact, not a release
# asset.
COPY deadzone.db.release /home/nonroot/.local/share/deadzone/deadzone.db.release

# Force the baked file to win unconditionally. With OFFLINE=1, Bootstrap
# never contacts the network — neither for the initial fetch nor for the
# #197 boot-time freshness probe — so `docker run --network none ... server`
# is a meaningful end-to-end test of the MCP handshake against the baked DB.
# Image refresh story: a new scrape-pack.yml run with a non-empty tag
# clobbers ghcr.io/laradji/deadzone:<tag> and :latest with a fresh image
# carrying the freshly-released DB, mirroring what `dbrelease` does to the
# GH-Releases asset.
ENV DEADZONE_DB_OFFLINE=1

# Bake the hugot model files (#207) at the path embed.DefaultCacheDir()
# resolves to when DEADZONE_HUGOT_CACHE is set. Without this, the
# embedder's NewHugot would fall through to hugot.DownloadModel on first
# server launch and pull ~138 MB from huggingface — turning every
# `docker run --rm` (i.e. every Claude Desktop / Cursor / Continue
# restart) into a 5-15 s first-byte stall, and breaking
# `--network none` outright. The COPY lays out
# /home/nonroot/.cache/deadzone/models/<dest_dirname>/{config.json,
# model_quantized.onnx, special_tokens_map.json, tokenizer.json,
# tokenizer_config.json, vocab.txt} — the exact 6-file shape
# hugot.NewPipeline expects in its ModelPath dir. The dest_dirname
# subdir matches embed.ModelDestDirname() — the "/" → "_" substitution
# of the model name applied by hugot's downloader. See
# internal/embed/model_pin.go for the pinned-revision rationale and
# internal/embed/hugot.go:127 for the lookup path.
#
# The image grows from ~100 MB (post-#203) to ~230 MB. Accepted
# trade-off: one larger pull replaces a per-MCP-session re-download,
# and the image becomes a self-contained MCP-server-in-a-box.
COPY --from=staging /staged/models/ /home/nonroot/.cache/deadzone/models/
ENV DEADZONE_HUGOT_CACHE=/home/nonroot/.cache/deadzone/models

# OCI labels — image.version is appended at build time via
# docker/build-push-action's `labels:` input (see scrape-pack.yml's
# docker job, post-#203). The rest are stable across releases and
# live here so a `docker inspect` of any tag carries the provenance
# even if a future CI change forgets to re-set them.
LABEL org.opencontainers.image.source="https://github.com/laradji/deadzone"
LABEL org.opencontainers.image.title="deadzone"
LABEL org.opencontainers.image.description="Local-first MCP doc server with semantic search"
LABEL org.opencontainers.image.licenses="Apache-2.0"
# Required by the official MCP Registry to verify ownership when the
# follow-up registry submission lands. Adding it now means the image
# is registry-eligible from day one without a re-publish.
# Source: https://modelcontextprotocol.io/registry/package-types
LABEL io.modelcontextprotocol.server.name="io.github.laradji/deadzone"

ENTRYPOINT ["/usr/local/bin/deadzone"]
CMD ["server"]
