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
# Why no DB baked: deadzone.db is fetched on first `server` launch from
# the GitHub Release matching the binary's compiled-in version (see
# internal/db/bootstrap.go). Baking it would chain `docker` → `release`
# → re-tag with a window where :latest points at a binary-only image,
# and would prevent an out-of-band `dbrelease` re-upload from reaching
# image users without a fresh image push. Tradeoff: --network none
# fails on first launch, which is fine for the MCP-client use case
# where network is the default.

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

# OCI labels — image.version is appended at build time via
# docker/build-push-action's `labels:` input (see release.yml). The
# rest are stable across releases and live here so a `docker inspect`
# of any tag carries the provenance even if a future CI change forgets
# to re-set them.
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
