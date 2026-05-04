package godoc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb/dirhash"
)

// Typed sentinels surfaced by the proxy/sumdb fetch path. Callers
// classify them via errors.Is in classifyFetchErr (cmd/deadzone/scrape.go)
// — soft skips advance to the next URL, fatal errors abort the lib.
//
//   - ErrModuleNotFound: proxy.golang.org returned 404. Treat as fatal:
//     a misspelled lib_id should fail loudly, not silently produce an
//     empty artifact (decision 9 cross-cutting principle in
//     ingestion-architecture.md).
//   - ErrModuleWithdrawn: proxy.golang.org returned 410 Gone (the
//     upstream withdrew the version). Soft skip — we can't index what's
//     gone, but the rest of the registry should still scrape.
//   - ErrSumDBMismatch: sum.golang.org returned a hash that doesn't
//     match the downloaded zip. Treat as fatal: an integrity failure
//     means either a tamper or a proxy bug, neither of which we should
//     silently ignore.
//   - ErrSumDBUnavailable: sum.golang.org is unreachable / returned 5xx.
//     Treat as fatal for the lib — refusing to index unverified bytes
//     matches the "fail fast and loud" principle.
var (
	ErrModuleNotFound   = errors.New("godoc: module not found on proxy.golang.org")
	ErrModuleWithdrawn  = errors.New("godoc: module version withdrawn")
	ErrSumDBMismatch    = errors.New("godoc: zip checksum does not match sum.golang.org")
	ErrSumDBUnavailable = errors.New("godoc: sum.golang.org unavailable")
)

// escapeModulePath wraps module.EscapePath with a typed error message.
// The escape rule (uppercase letters → !<lower>) is documented at
// https://go.dev/ref/mod#goproxy-protocol — needed because case
// distinctions in module paths must survive case-insensitive
// filesystems and URL routers along the proxy path.
func escapeModulePath(modPath string) (string, error) {
	esc, err := module.EscapePath(modPath)
	if err != nil {
		return "", fmt.Errorf("escape module path %q: %w", modPath, err)
	}
	return esc, nil
}

// fetchSumdbHash queries sum.golang.org for the h1: hash of the
// module zip at (modPath, version). The response format is documented
// at https://go.dev/ref/mod#checksum-database — first line is a
// lookup id, second line is "<module> <version> h1:<hash>", third is
// the same for /go.mod, then a blank line and the Merkle tree proof.
// We only care about the second line (the zip hash).
//
// sumdbBase is the base URL ("https://sum.golang.org" in production,
// or an httptest.NewServer URL in tests). modPath must be unescaped;
// this helper escapes it itself.
func fetchSumdbHash(ctx context.Context, client *http.Client, sumdbBase, modPath, version string) (string, error) {
	escaped, err := escapeModulePath(modPath)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(sumdbBase, "/") + "/lookup/" + escaped + "@" + version
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build sumdb request %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %s", ErrSumDBUnavailable, url, err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Module unknown to sumdb — treat the same as proxy 404 so
		// the operator gets a single coherent classification.
		return "", fmt.Errorf("%w: %s", ErrModuleNotFound, url)
	}
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("%w: %s status %d", ErrSumDBUnavailable, url, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sumdb %s status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read sumdb body %s: %w", url, err)
	}
	return parseSumdbResponse(body)
}

// parseSumdbResponse extracts the h1: zip hash from a sumdb /lookup
// response. The relevant line has the shape:
//
//	<module> <version> h1:<base64hash>=
//
// (no "/go.mod" suffix on the module path). We scan for the first
// such line; the response also contains a "<module> <version>/go.mod
// h1:..." line which we deliberately ignore.
func parseSumdbResponse(body []byte) (string, error) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Expected form: "<module> <version> h1:<hash>". Reject
		// /go.mod lines explicitly so a malformed response can't
		// trick us into using the go.mod hash instead of the zip
		// hash.
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		if !strings.HasPrefix(fields[2], "h1:") {
			continue
		}
		// The go.mod hash line has shape "<module> <version>/go.mod h1:..." —
		// reject by checking the second field (version), where the
		// /go.mod suffix actually lives. Without this guard a body
		// containing only the go.mod line would silently verify the
		// wrong artifact.
		if strings.HasSuffix(fields[1], "/go.mod") {
			continue
		}
		return fields[2], nil
	}
	return "", fmt.Errorf("sumdb response: no h1: zip hash line found in %d bytes", len(body))
}

// verifyZipChecksum computes the dirhash of the local zip at zipPath
// using the canonical Hash1 algorithm and compares it to the expected
// value from sumdb. Returns ErrSumDBMismatch on divergence.
//
// The Hash1 algorithm is non-trivial: it lists the zip entries,
// strips the "<module>@<version>/" prefix, sorts the names, sha256s
// each file, then sha256s the manifest. Reimplementing it in-tree
// would be a tamper-equivalent bug. We delegate to dirhash.HashZip
// which is what `go mod download` itself uses.
func verifyZipChecksum(zipPath, expectedH1 string) error {
	got, err := dirhash.HashZip(zipPath, dirhash.Hash1)
	if err != nil {
		return fmt.Errorf("compute h1 of %s: %w", zipPath, err)
	}
	if got != expectedH1 {
		return fmt.Errorf("%w: got %s, want %s (zip %s)", ErrSumDBMismatch, got, expectedH1, zipPath)
	}
	return nil
}
