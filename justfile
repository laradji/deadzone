# Deadzone — task runner.
#
# Go 1.26.2 is pinned via .mise.toml and is intentionally NOT on the system
# PATH. Every recipe wraps `go` in `mise exec --` so neither humans nor
# agents have to remember the prefix. `just` itself is also pinned in
# .mise.toml, so `mise install` brings up the whole toolchain.
#
# CGO: the hugot embedder runs on the ORT (onnxruntime) backend, which
# pulls in daulet/tokenizers — a Rust-backed CGO tokenizer. Building
# therefore requires:
#
#   CGO_ENABLED=1
#   -tags ORT
#   libtokenizers.a available at link time
#
# By default recipes pass -L./lib to cgo. Set DEADZONE_TOKENIZERS_LIB to
# point `go build` at a different directory (e.g. /opt/homebrew/lib). The
# library itself is a static archive from
# https://github.com/daulet/tokenizers/releases — run `just
# fetch-tokenizers` once after cloning to drop the right prebuilt into
# ./lib/ for your platform (or hand-place one and override the env var).
# The ORT shared library (libonnxruntime.{dylib,so}) is downloaded +
# SHA256-verified + cached on first run by internal/ort.Bootstrap; set
# DEADZONE_ORT_LIB_PATH to bypass the download and point at a
# hand-positioned library (air-gapped installs).
# #74 will wire libtokenizers.a into release CI.

set shell := ["bash", "-euo", "pipefail", "-c"]

# List available recipes
default:
    @just --list --unsorted

# Install the pinned toolchain (Go + just) via mise — one-time bootstrap
bootstrap:
    mise install

# Idempotent: skips the download if the file already exists at the
# expected path. Set TOKENIZERS_VERSION to override the pinned version
# (default must match .github/workflows/ci.yml).
#
# Download libtokenizers.a for the current platform into ./lib/.
fetch-tokenizers:
    #!/usr/bin/env bash
    set -euo pipefail
    ver="${TOKENIZERS_VERSION:-v1.26.0}"
    target="${DEADZONE_TOKENIZERS_LIB:-./lib}"
    if [ -f "${target}/libtokenizers.a" ]; then
        echo "libtokenizers.a already present at ${target}/"
        exit 0
    fi
    case "$(uname -sm)" in
        "Darwin arm64") asset="libtokenizers.darwin-arm64.tar.gz" ;;
        "Linux x86_64") asset="libtokenizers.linux-amd64.tar.gz" ;;
        "Linux aarch64"|"Linux arm64") asset="libtokenizers.linux-arm64.tar.gz" ;;
        *) echo "unsupported platform: $(uname -sm)" >&2; exit 1 ;;
    esac
    mkdir -p "${target}"
    url="https://github.com/daulet/tokenizers/releases/download/${ver}/${asset}"
    echo "fetching ${url}"
    curl -fL -o "${target}/tok.tgz" "${url}"
    tar -C "${target}" -xzf "${target}/tok.tgz"
    rm "${target}/tok.tgz"
    echo "libtokenizers.a → ${target}/libtokenizers.a"

# Compile every package. Fast sanity check; produces no binaries.
build:
    CGO_ENABLED=1 CGO_LDFLAGS="-L${DEADZONE_TOKENIZERS_LIB:-./lib}" \
        mise exec -- go build -tags ORT ./...

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
    export CGO_ENABLED=1
    export CGO_LDFLAGS="-L${DEADZONE_TOKENIZERS_LIB:-./lib}"
    for bin in server scraper consolidate packs; do
        mise exec -- go build -tags ORT -trimpath -ldflags "${ldflags}" -o "deadzone-${bin}" "./cmd/${bin}"
    done
    echo "built deadzone-{server,scraper,consolidate,packs} ${ver} (${sha}, built ${built})"

# Run the full test suite
test:
    CGO_ENABLED=1 CGO_LDFLAGS="-L${DEADZONE_TOKENIZERS_LIB:-./lib}" \
        mise exec -- go test -tags ORT ./...

# Format all Go sources
fmt:
    mise exec -- go fmt ./...

# Run `go vet` over every package
vet:
    CGO_ENABLED=1 CGO_LDFLAGS="-L${DEADZONE_TOKENIZERS_LIB:-./lib}" \
        mise exec -- go vet -tags ORT ./...

# Sync go.mod / go.sum with the source
tidy:
    mise exec -- go mod tidy

# Run the scraper, writing one artifact per lib to ./artifacts/ (pass lib=/org/project to refresh only that entry)
scrape lib="":
    CGO_ENABLED=1 CGO_LDFLAGS="-L${DEADZONE_TOKENIZERS_LIB:-./lib}" \
        mise exec -- go run -tags ORT ./cmd/scraper -artifacts ./artifacts {{ if lib != "" { "-lib " + lib } else { "" } }}

# Merge per-lib artifacts in ./artifacts/ into the main deadzone DB
consolidate db="deadzone.db":
    CGO_ENABLED=1 CGO_LDFLAGS="-L${DEADZONE_TOKENIZERS_LIB:-./lib}" \
        mise exec -- go run -tags ORT ./cmd/consolidate -db {{db}} -artifacts ./artifacts

# Run the MCP server against the given DB file (must already be consolidated)
serve db="deadzone.db":
    CGO_ENABLED=1 CGO_LDFLAGS="-L${DEADZONE_TOKENIZERS_LIB:-./lib}" \
        mise exec -- go run -tags ORT ./cmd/server -db {{db}}

# Upload local artifacts/*.db files to the rolling GitHub Release (see #30)
packs-upload:
    mise exec -- go run ./cmd/packs upload -artifacts ./artifacts -manifest ./artifacts/manifest.yaml

# Download release assets referenced by the manifest into ./artifacts (pass lib=/org/project to fetch one)
packs-download lib="":
    mise exec -- go run ./cmd/packs download -artifacts ./artifacts -manifest ./artifacts/manifest.yaml {{ if lib != "" { "-lib " + lib } else { "" } }}

# Print the manifest as a table to stdout
packs-list:
    mise exec -- go run ./cmd/packs list -manifest ./artifacts/manifest.yaml

# Remove built binaries, per-lib artifacts, and the local DB files (preserves artifacts/manifest.yaml)
clean:
    rm -f deadzone deadzone-server deadzone-scraper deadzone-consolidate deadzone-packs
    rm -f deadzone.db deadzone.db-wal deadzone.db-shm
    rm -f artifacts/*.db artifacts/*.db-wal artifacts/*.db-shm
