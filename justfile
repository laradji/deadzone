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

# Run the scraper, indexing into the given DB file
scrape db="deadzone.db":
    {{go}} run ./cmd/scraper -db {{db}}

# Run the MCP server against the given DB file
serve db="deadzone.db":
    {{go}} run ./cmd/server -db {{db}}

# Remove built binaries and the local DB files
clean:
    rm -f deadzone deadzone-server
    rm -f deadzone.db deadzone.db-wal deadzone.db-shm
