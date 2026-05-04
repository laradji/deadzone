package godoc

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/laradji/deadzone/internal/scraper"
)

// SumdbBaseEnv is the env var that overrides the sum.golang.org base
// URL. Test code points it at an httptest.NewServer; production leaves
// it unset so the default takes effect.
const SumdbBaseEnv = "DEADZONE_SUMDB_BASE"

// GitHubTokenEnv is the env var that, when set, is used as a Bearer
// token on api.github.com requests. Opt-in: unauthenticated callers
// get the 60 req/h public quota; authenticated callers get 5000 req/h.
const GitHubTokenEnv = "GITHUB_TOKEN"

const defaultSumdbBase = "https://sum.golang.org"

// FetchOneViaGodoc downloads source bytes for one godoc URL, parses
// them via ParseGodoc, and returns the resulting db.Doc chunks. The
// URL shape determines the fetch path:
//
//   - "<host>/<module>/@v/<version>.zip"   → proxy.golang.org path,
//     verified against sum.golang.org before parse.
//   - "<host>/repos/<owner>/<repo>/contents/<path>?ref=<ref>"
//                                          → GitHub Contents API path,
//     used for the stdlib (golang/go is not a Go module).
//
// Anything else returns an error. The host is matched on URL path
// shape so httptest.NewServer URLs (127.0.0.1:port) work transparently
// in tests.
func FetchOneViaGodoc(ctx context.Context, client *http.Client, libID, rawURL string) (scraper.FetchOneResult, error) {
	switch {
	case isProxyZipURL(rawURL):
		return fetchProxyZip(ctx, client, libID, rawURL)
	case isGitHubContentsURL(rawURL):
		return fetchGitHubContents(ctx, client, libID, rawURL)
	default:
		return scraper.FetchOneResult{}, fmt.Errorf("godoc: unsupported URL shape %q (need /@v/<ver>.zip or /contents/...)", rawURL)
	}
}

func isProxyZipURL(rawURL string) bool {
	return strings.Contains(rawURL, "/@v/") && strings.HasSuffix(rawURL, ".zip")
}

func isGitHubContentsURL(rawURL string) bool {
	return strings.Contains(rawURL, "/contents/")
}

// fetchProxyZip downloads the module zip, verifies its h1 hash
// against sumdb, extracts to a temp dir, and parses via ParseGodoc.
//
// URL shape: <scheme>://<host>/<module>/@v/<version>.zip — the module
// path may be in its natural (unescaped) form; this function escapes
// it via module.EscapePath before re-issuing the actual fetch and
// before querying sumdb. That way the YAML carries a human-readable
// module path even for case-sensitive ones (github.com/Azure/...).
func fetchProxyZip(ctx context.Context, client *http.Client, libID, rawURL string) (scraper.FetchOneResult, error) {
	modPath, version, err := parseProxyZipURL(rawURL)
	if err != nil {
		return scraper.FetchOneResult{}, err
	}
	escaped, err := escapeModulePath(modPath)
	if err != nil {
		return scraper.FetchOneResult{}, err
	}

	// Rewrite the URL to use the escaped module path so any uppercase
	// letters become !<lower> per the proxy protocol. If the YAML
	// already had an escaped path (lowercase only), this is a no-op.
	u, err := url.Parse(rawURL)
	if err != nil {
		return scraper.FetchOneResult{}, fmt.Errorf("parse url %s: %w", rawURL, err)
	}
	u.Path = "/" + escaped + "/@v/" + version + ".zip"
	canonicalURL := u.String()

	// 1. Download zip to a temp file.
	tmpDir, err := os.MkdirTemp("", "deadzone-godoc-")
	if err != nil {
		return scraper.FetchOneResult{}, fmt.Errorf("mktmp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, "module.zip")
	bytesRead, err := downloadToFile(ctx, client, canonicalURL, zipPath)
	if err != nil {
		return scraper.FetchOneResult{}, err
	}

	// 2. Verify against sumdb. Sumdb base URL is overrideable via env
	// for tests; production uses the public sum.golang.org.
	sumdbBase := defaultSumdbBase
	if v := os.Getenv(SumdbBaseEnv); v != "" {
		sumdbBase = v
	}
	expectedHash, err := fetchSumdbHash(ctx, client, sumdbBase, modPath, version)
	if err != nil {
		return scraper.FetchOneResult{}, err
	}
	if err := verifyZipChecksum(zipPath, expectedHash); err != nil {
		return scraper.FetchOneResult{}, err
	}

	// 3. Extract and parse. Module zip entries are prefixed with
	// "<module>@<version>/" — strip that so ParseGodoc sees the
	// natural source layout.
	srcDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return scraper.FetchOneResult{}, fmt.Errorf("mkdir src: %w", err)
	}
	prefix := modPath + "@" + version + "/"
	if err := extractZipStripPrefix(zipPath, srcDir, prefix); err != nil {
		return scraper.FetchOneResult{}, err
	}

	docs, err := ParseGodoc(libID, srcDir)
	if err != nil {
		return scraper.FetchOneResult{}, err
	}
	return scraper.FetchOneResult{Docs: docs, Bytes: bytesRead}, nil
}

// parseProxyZipURL extracts (modPath, version) from a URL of the form
// "<scheme>://<host>/<module>/@v/<version>.zip". The module path is
// returned unescaped (the YAML's natural form).
func parseProxyZipURL(rawURL string) (string, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("parse url %s: %w", rawURL, err)
	}
	p := strings.TrimPrefix(u.Path, "/")
	idx := strings.Index(p, "/@v/")
	if idx < 0 {
		return "", "", fmt.Errorf("godoc: not a proxy zip URL: %s", rawURL)
	}
	modPath := p[:idx]
	rest := p[idx+len("/@v/"):]
	if !strings.HasSuffix(rest, ".zip") {
		return "", "", fmt.Errorf("godoc: expected .zip suffix in %s", rawURL)
	}
	version := strings.TrimSuffix(rest, ".zip")
	if modPath == "" || version == "" {
		return "", "", fmt.Errorf("godoc: empty module or version in %s", rawURL)
	}
	return modPath, version, nil
}

func downloadToFile(ctx context.Context, client *http.Client, rawURL, dstPath string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, fmt.Errorf("build request %s: %w", rawURL, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// proceed
	case http.StatusNotFound:
		return 0, fmt.Errorf("%w: %s", ErrModuleNotFound, rawURL)
	case http.StatusGone:
		return 0, fmt.Errorf("%w: %s", ErrModuleWithdrawn, rawURL)
	default:
		if resp.StatusCode >= 500 {
			return 0, &scraper.HTTPStatusError{Status: resp.StatusCode, URL: rawURL}
		}
		return 0, &scraper.HTTPStatusError{Status: resp.StatusCode, URL: rawURL}
	}

	f, err := os.Create(dstPath)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", dstPath, err)
	}
	defer f.Close()
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return int(n), fmt.Errorf("write %s: %w", dstPath, err)
	}
	return int(n), nil
}

// extractZipStripPrefix unzips zipPath into dstDir, removing prefix
// from each entry name. Entries that don't start with prefix are
// silently skipped (defensive — real proxy zips always conform).
// Symlinks and absolute paths are rejected to prevent zip-slip.
func extractZipStripPrefix(zipPath, dstDir, prefix string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip %s: %w", zipPath, err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		name := f.Name
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" {
			continue
		}
		// Reject path traversal — clean and verify the result stays
		// inside dstDir.
		cleanRel := filepath.Clean(rel)
		if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
			return fmt.Errorf("zip-slip: refusing to extract %q", name)
		}
		dst := filepath.Join(dstDir, cleanRel)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dst, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir parent %s: %w", filepath.Dir(dst), err)
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open zip entry %s: %w", name, err)
		}
		out, err := os.Create(dst)
		if err != nil {
			rc.Close()
			return fmt.Errorf("create %s: %w", dst, err)
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return fmt.Errorf("write %s: %w", dst, err)
		}
		rc.Close()
		out.Close()
	}
	return nil
}

// fetchGitHubContents queries the GitHub Contents API at rawURL
// (which must resolve to a *directory*), downloads each .go file
// (skipping _test.go), writes them to a temp dir, and parses via
// ParseGodoc. Subdirectories are NOT recursed — the operator picks
// the exact package path; subpackages get their own lib_id entry.
func fetchGitHubContents(ctx context.Context, client *http.Client, libID, rawURL string) (scraper.FetchOneResult, error) {
	entries, bytesIndex, err := getGitHubDirEntries(ctx, client, rawURL)
	if err != nil {
		return scraper.FetchOneResult{}, err
	}

	tmpDir, err := os.MkdirTemp("", "deadzone-godoc-stdlib-")
	if err != nil {
		return scraper.FetchOneResult{}, fmt.Errorf("mktmp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	bytesTotal := bytesIndex
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		if !strings.HasSuffix(e.Name, ".go") || strings.HasSuffix(e.Name, "_test.go") {
			continue
		}
		if e.DownloadURL == "" {
			continue
		}
		dst := filepath.Join(tmpDir, e.Name)
		n, err := githubDownload(ctx, client, e.DownloadURL, dst)
		if err != nil {
			return scraper.FetchOneResult{}, err
		}
		bytesTotal += n
	}

	docs, err := ParseGodoc(libID, tmpDir)
	if err != nil {
		return scraper.FetchOneResult{}, err
	}
	return scraper.FetchOneResult{Docs: docs, Bytes: bytesTotal}, nil
}

type ghContentEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	DownloadURL string `json:"download_url"`
}

func getGitHubDirEntries(ctx context.Context, client *http.Client, rawURL string) ([]ghContentEntry, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request %s: %w", rawURL, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv(GitHubTokenEnv); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, 0, fmt.Errorf("read body %s: %w", rawURL, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return nil, 0, fmt.Errorf("%w: %s", ErrModuleNotFound, rawURL)
		}
		return nil, 0, &scraper.HTTPStatusError{Status: resp.StatusCode, URL: rawURL, Body: string(body)}
	}
	// The Contents API returns an array for directories and an object
	// for single files. We only support directory queries; reject the
	// object case explicitly so a misconfigured URL fails loudly.
	if len(body) > 0 && body[0] != '[' {
		return nil, 0, fmt.Errorf("godoc: GitHub Contents API returned non-array (URL points at a file, not a directory): %s", rawURL)
	}
	var entries []ghContentEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, 0, fmt.Errorf("parse contents response %s: %w", rawURL, err)
	}
	return entries, len(body), nil
}

func githubDownload(ctx context.Context, client *http.Client, rawURL, dstPath string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, fmt.Errorf("build request %s: %w", rawURL, err)
	}
	// download_url points at raw.githubusercontent.com; auth not
	// strictly required there, but pass through the token if set so
	// rate-limit bookkeeping stays attached to the same identity.
	if tok := os.Getenv(GitHubTokenEnv); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, &scraper.HTTPStatusError{Status: resp.StatusCode, URL: rawURL}
	}
	f, err := os.Create(dstPath)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", dstPath, err)
	}
	defer f.Close()
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return int(n), fmt.Errorf("write %s: %w", dstPath, err)
	}
	return int(n), nil
}

