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

# Compile every package (CGO-free, pure Go)
build:
    {{go}} build ./...

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
    rm -f deadzone deadzone-server deadzone-consolidate deadzone-packs
    rm -f deadzone.db deadzone.db-wal deadzone.db-shm
    rm -f artifacts/*.db artifacts/*.db-wal artifacts/*.db-shm
