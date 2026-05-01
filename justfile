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
# point `go build` at a different directory (e.g. /opt/homebrew/lib);
# the tokenizers_lib variable below resolves the env var at recipe
# expansion time so the override propagates to every recipe in one
# place. The library itself is a static archive from
# https://github.com/daulet/tokenizers/releases — run `just
# fetch-tokenizers` once after cloning to drop the right prebuilt into
# ./lib/ for your platform (or hand-place one and override the env var).
# The ORT shared library (libonnxruntime.{dylib,so}) is downloaded +
# SHA256-verified + cached on first run by internal/ort.Bootstrap; set
# DEADZONE_ORT_LIB_PATH to bypass the download and point at a
# hand-positioned library (air-gapped installs).
#
# CGO link warning: on macOS arm64 the linker may emit
#   ld: warning: ignoring duplicate libraries: '-ldl'
# This is harmless — daulet/tokenizers' build script and Go's cgo runtime
# both pass `-ldl` and `ld` deduplicates with a warning instead of an
# error. Silencing via `-Wl,-no_warn_duplicate_libraries` would mask
# unrelated duplicates if they appear later, so we live with the warning.
#
# Worktree onboarding: after cloning OR creating a fresh git worktree,
# run `mise trust` once at the worktree root before any other recipe.
# Without it `mise exec --` refuses to read .mise.toml and every Go
# recipe fails with a "config file is not trusted" error.

set shell := ["bash", "-euo", "pipefail", "-c"]

# Tokenizers static-archive directory. Resolved at recipe-expansion time
# from DEADZONE_TOKENIZERS_LIB with a `./lib` default — single source of
# truth for every recipe below.
tokenizers_lib := env_var_or_default('DEADZONE_TOKENIZERS_LIB', './lib')

# List available recipes
default:
    @just --list --unsorted

# Install the pinned toolchain (Go + just) via mise — one-time bootstrap (also run `mise trust` once per worktree, see file header).
bootstrap:
    mise install

# Idempotent: skips the download if the file already exists at the
# expected path. Set TOKENIZERS_VERSION to override the pinned version
# (default must match .github/actions/install-native-deps/action.yml).
#
# Download libtokenizers.a for the current platform into ./lib/.
fetch-tokenizers:
    #!/usr/bin/env bash
    set -euo pipefail
    ver="${TOKENIZERS_VERSION:-v1.27.0}"
    target="{{tokenizers_lib}}"
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

# Verify libtokenizers.a is on disk before any CGO recipe attempts to
# link. Saves 5+ seconds of compile before the linker emits a cryptic
# "library 'tokenizers' not found" error.
_check-tokenizers:
    @[ -f "{{tokenizers_lib}}/libtokenizers.a" ] || { \
        echo "error: libtokenizers.a missing — run \`just fetch-tokenizers\`" >&2; \
        exit 1; \
    }

# Compile every package. Fast sanity check; produces no binaries.
build: _check-tokenizers
    CGO_ENABLED=1 CGO_LDFLAGS="-L{{tokenizers_lib}}" \
        mise exec -- go build -tags ORT ./...

# Build the single `deadzone` CLI with version/commit/date injected via ldflags.
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
# Cross-platform smoke coverage for this recipe lives in
# .github/workflows/release.yml — the per-OS matrix runs `just
# build-release` on macOS arm64 / Linux amd64 / Linux arm64 against
# pinned tokenizers + ORT, so any platform-specific drift is caught at
# release-cut time, not by an end user.
#
# -trimpath strips absolute source paths from the binary (no $PWD leak),
# -s -w strips debug info (keeps the CGO binary small).
build-release: _check-tokenizers
    #!/usr/bin/env bash
    set -euo pipefail
    ver="${VERSION:-$(git describe --tags --dirty --always 2>/dev/null || echo dev)}"
    sha="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
    built="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
    ldflags="-s -w -X main.version=${ver} -X main.commit=${sha} -X main.date=${built}"
    export CGO_ENABLED=1
    export CGO_LDFLAGS="-L{{tokenizers_lib}}"
    mise exec -- go build -tags ORT -trimpath -ldflags "${ldflags}" -o ./deadzone ./cmd/deadzone
    echo "built ./deadzone ${ver} (${sha}, built ${built})"

# Run the full test suite
test: _check-tokenizers
    CGO_ENABLED=1 CGO_LDFLAGS="-L{{tokenizers_lib}}" \
        mise exec -- go test -tags ORT ./...

# Format all Go sources
fmt:
    CGO_ENABLED=1 CGO_LDFLAGS="-L{{tokenizers_lib}}" \
        mise exec -- go fmt ./...

# Run `go vet` over every package
vet: _check-tokenizers
    CGO_ENABLED=1 CGO_LDFLAGS="-L{{tokenizers_lib}}" \
        mise exec -- go vet -tags ORT ./...

# Sync go.mod / go.sum. `go mod tidy` has no -tags flag, so we pass it
# via GOFLAGS to keep ORT-only imports (internal/embed/hugot.go) in graph.
tidy:
    CGO_ENABLED=1 CGO_LDFLAGS="-L{{tokenizers_lib}}" \
    GOFLAGS="-tags=ORT" \
        mise exec -- go mod tidy

# Run the scraper, writing one artifact per lib to ./artifacts/ (pass lib=/org/project to refresh only that entry; pass version=X to pin to one expanded version)
scrape lib="" version="": _check-tokenizers
    CGO_ENABLED=1 CGO_LDFLAGS="-L{{tokenizers_lib}}" \
        mise exec -- go run -tags ORT ./cmd/deadzone scrape --artifacts ./artifacts {{ if lib != "" { "--lib " + lib } else { "" } }} {{ if version != "" { "--version " + version } else { "" } }}

# Merge per-lib artifacts in ./artifacts/ into the main deadzone DB
consolidate db="deadzone.db": _check-tokenizers
    CGO_ENABLED=1 CGO_LDFLAGS="-L{{tokenizers_lib}}" \
        mise exec -- go run -tags ORT ./cmd/deadzone consolidate --db {{db}} --artifacts ./artifacts

# Run the MCP server against the given DB file (must already be consolidated)
serve db="deadzone.db": _check-tokenizers
    CGO_ENABLED=1 CGO_LDFLAGS="-L{{tokenizers_lib}}" \
        mise exec -- go run -tags ORT ./cmd/deadzone server --db {{db}}

# Upload ./deadzone.db to the GH Release at the given tag (operator-driven release, see #101).
# Assumes the tag already exists on origin and CI's release.yml has created the release object.
dbrelease tag: _check-tokenizers
    CGO_ENABLED=1 CGO_LDFLAGS="-L{{tokenizers_lib}}" \
        mise exec -- go run -tags ORT ./cmd/deadzone dbrelease --db deadzone.db --tag {{tag}}

# Render docs/coverage.md from the consolidated DB (#152). No embedder
# load — runs against a freshly fetched deadzone.db with no ORT setup.
# Override `db=` and `output=` for ad-hoc runs against alternate paths.
coverage db="deadzone.db" output="docs/coverage.md": _check-tokenizers
    CGO_ENABLED=1 CGO_LDFLAGS="-L{{tokenizers_lib}}" \
        mise exec -- go run -tags ORT ./cmd/deadzone coverage --db {{db}} --output {{output}}

# Download / refresh the cached deadzone.db from the latest GH Release (#108).
# Set force=true to re-fetch even when the cached tag matches the latest release.
fetch-db force="": _check-tokenizers
    CGO_ENABLED=1 CGO_LDFLAGS="-L{{tokenizers_lib}}" \
        mise exec -- go run -tags ORT ./cmd/deadzone fetch-db {{ if force != "" { "--force" } else { "" } }}

# Remove the built binary, per-lib artifact folders, and the local DB files (preserves artifacts/manifest.yaml)
clean:
    rm -f deadzone
    rm -f deadzone.db deadzone.db-wal deadzone.db-shm deadzone.db.sha256
    rm -rf artifacts/*/
