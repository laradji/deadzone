# Deadzone — task runner.
#
# Go 1.26.2 is pinned via .mise.toml and is intentionally NOT on the system
# PATH. Every recipe wraps `go` in `mise exec --` so neither humans nor
# agents have to remember the prefix. `just` itself is also pinned in
# .mise.toml, so `mise install` brings up the whole toolchain.

set shell := ["bash", "-euo", "pipefail", "-c"]

go := "mise exec -- go"

# List available recipes
default:
    @just --list --unsorted

# Install the pinned toolchain (Go + just) via mise — one-time bootstrap
bootstrap:
    mise install

# Compile every package (CGO-free, pure Go). Fast sanity check; produces no binaries.
build:
    {{go}} build ./...

# Build the four CLI binaries with version/commit/date injected via ldflags.
#
# Reads VERSION / COMMIT / DATE from the environment. Release CI sets them
# explicitly from the workflow matrix (see #74's release.yml):
#
#   VERSION=v0.1.0 COMMIT=$GITHUB_SHA DATE=$(date -u +%FT%TZ) just build-release
#
# When unset, defaults fall back to `git describe --tags --dirty --always`,
# `git rev-parse --short HEAD`, and the current UTC timestamp so local dev
# binaries are self-labelling too.
#
# -trimpath strips absolute source paths from the binary (no $PWD leak),
# -s -w strips debug info (keeps the CGO binary small).
build-release:
    #!/usr/bin/env bash
    set -euo pipefail
    ver="${VERSION:-$(git describe --tags --dirty --always 2>/dev/null || echo dev)}"
    sha="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
    built="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
    ldflags="-s -w -X main.version=${ver} -X main.commit=${sha} -X main.date=${built}"
    for bin in server scraper consolidate packs; do
        mise exec -- go build -trimpath -ldflags "${ldflags}" -o "deadzone-${bin}" "./cmd/${bin}"
    done
    echo "built deadzone-{server,scraper,consolidate,packs} ${ver} (${sha}, built ${built})"

# Run the full test suite
test:
    {{go}} test ./...

# Format all Go sources
fmt:
    {{go}} fmt ./...

# Run `go vet` over every package
vet:
    {{go}} vet ./...

# Sync go.mod / go.sum with the source
tidy:
    {{go}} mod tidy

# Run the scraper, writing one artifact per lib to ./artifacts/ (pass lib=/org/project to refresh only that entry)
scrape lib="":
    {{go}} run ./cmd/scraper -artifacts ./artifacts {{ if lib != "" { "-lib " + lib } else { "" } }}

# Merge per-lib artifacts in ./artifacts/ into the main deadzone DB
consolidate db="deadzone.db":
    {{go}} run ./cmd/consolidate -db {{db}} -artifacts ./artifacts

# Run the MCP server against the given DB file (must already be consolidated)
serve db="deadzone.db":
    {{go}} run ./cmd/server -db {{db}}

# Upload local artifacts/*.db files to the rolling GitHub Release (see #30)
packs-upload:
    {{go}} run ./cmd/packs upload -artifacts ./artifacts -manifest ./artifacts/manifest.yaml

# Download release assets referenced by the manifest into ./artifacts (pass lib=/org/project to fetch one)
packs-download lib="":
    {{go}} run ./cmd/packs download -artifacts ./artifacts -manifest ./artifacts/manifest.yaml {{ if lib != "" { "-lib " + lib } else { "" } }}

# Print the manifest as a table to stdout
packs-list:
    {{go}} run ./cmd/packs list -manifest ./artifacts/manifest.yaml

# Remove built binaries, per-lib artifacts, and the local DB files (preserves artifacts/manifest.yaml)
clean:
    rm -f deadzone deadzone-server deadzone-scraper deadzone-consolidate deadzone-packs
    rm -f deadzone.db deadzone.db-wal deadzone.db-shm
    rm -f artifacts/*.db artifacts/*.db-wal artifacts/*.db-shm
