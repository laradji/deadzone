// bootstrap.go owns the consumer-side fetch flow for deadzone.db,
// mirroring internal/ort/ort.go's symmetric flow for the onnxruntime
// shared library. See issue #108 for the full design.
//
// Contract (revised per PR #110 review): the cached deadzone.db is
// pinned to the binary's own version. The DB does NOT auto-upgrade
// across binary versions — that would risk pulling a schema/embedder
// newer than the binary can read. Trigger paths:
//
//  1. First run, no cached DB → fetch /releases/tags/<AppVersion>,
//     install, serve.
//  2. Subsequent run, cached tag matches AppVersion → use cache. Zero
//     API calls. Instant start.
//  3. Subsequent run, cached tag != AppVersion (i.e. the binary was
//     upgraded) → fetch /releases/tags/<AppVersion>, atomic swap.
//  4. AppVersion is a local/dev build (literal "dev", dirty working
//     tree, or git-describe between-tags) → fall back to
//     /releases/latest with a WARN so local dev is still ergonomic.
//
// Env-var escape hatches (matching DEADZONE_ORT_CACHE / DEADZONE_HUGOT_CACHE):
//
//   DEADZONE_DB_CACHE     — override the cache directory
//   DEADZONE_DB_OFFLINE=1 — never make a network call; fail loud on
//                           first run or when the cached DB's version
//                           doesn't match the binary
//
// DEADZONE_DB_NO_AUTO_UPGRADE was removed in PR #110 review: the
// per-startup API call it protected against no longer happens now that
// the tag-match path is zero-network.

package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Cache / asset constants. The asset names match what `deadzone
// dbrelease` uploads; changing either side requires changing both.
const (
	EnvCacheDir = "DEADZONE_DB_CACHE"
	EnvOffline  = "DEADZONE_DB_OFFLINE"

	BootstrapDefaultRepo = "laradji/deadzone"

	// DevVersion is the sentinel AppVersion for local / unreleased
	// builds. Bootstrap falls back to /releases/latest on it.
	DevVersion = "dev"

	dbAssetName     = "deadzone.db"
	sha256AssetName = "deadzone.db.sha256"
	tagSidecarName  = "deadzone.db.release"
)

// Two HTTP clients with different timeouts (PR #110 review item 2):
// metadata responses are tiny JSON, so a hung API call shouldn't make
// a user wait 15 minutes before failing. The asset download is the
// bandwidth-heavy one and keeps the long ceiling.
var (
	metadataHTTPClient = &http.Client{Timeout: 30 * time.Second}
	assetHTTPClient    = &http.Client{Timeout: 15 * time.Minute}
)

// releasesAPIBase is the GitHub API root. Split out so tests can point
// Bootstrap at a local httptest server.
var releasesAPIBase = "https://api.github.com"

// gitDescribeBetweenTagsRe recognises `git describe --tags --always`
// output for commits that aren't themselves a tagged release:
// "v0.1.0-2-g1234567" (with optional "-dirty"). Such builds aren't
// published releases, so Bootstrap treats them as dev and falls back
// to /releases/latest.
var gitDescribeBetweenTagsRe = regexp.MustCompile(`-\d+-g[0-9a-f]{7,}(-dirty)?$`)

// ErrNoReleaseForVersion is returned when the requested AppVersion has
// no corresponding release on GitHub. Callers (main, tests) check via
// errors.Is so the user-facing error message can be precise. Wraps
// nothing — it IS the error.
var ErrNoReleaseForVersion = errors.New("no deadzone.db published for this binary version")

// BootstrapOptions lets callers (runServer, runFetchDB, tests)
// override what Bootstrap pulls from env / defaults.
type BootstrapOptions struct {
	// CacheDir overrides DEADZONE_DB_CACHE / the platform default.
	CacheDir string
	// Repo overrides BootstrapDefaultRepo (owner/name).
	Repo string
	// AppVersion is the binary's own version string (e.g. "v0.1.0"
	// from the justfile's -ldflags). Required in practice; an empty
	// string is treated as DevVersion and triggers the /latest
	// fallback.
	AppVersion string
	// Force re-fetches even when the cached tag matches AppVersion.
	// fetch-db --force uses this; the server path leaves it false so
	// a no-op startup stays zero-network.
	Force bool
}

// Bootstrap ensures a deadzone.db is present at the default cache
// location, pinned to appVersion, and returns its path. See the file
// banner for the full contract.
//
// Returns (path, upgraded, error) where upgraded is true when this
// call just replaced an existing cache file (version bump or -force).
// First-fetch returns upgraded=false (no previous file existed).
func Bootstrap(ctx context.Context, appVersion string) (string, bool, error) {
	return BootstrapWithOptions(ctx, BootstrapOptions{AppVersion: appVersion})
}

// BootstrapWithOptions is the explicit-options form of Bootstrap.
// fetch-db uses it for -force and -repo; tests use it to inject a
// CacheDir without touching env vars.
func BootstrapWithOptions(ctx context.Context, opts BootstrapOptions) (string, bool, error) {
	if opts.AppVersion == "" {
		opts.AppVersion = DevVersion
	}
	cacheDir := opts.CacheDir
	if cacheDir == "" {
		cacheDir = DefaultCacheDir()
	}
	repo := opts.Repo
	if repo == "" {
		repo = BootstrapDefaultRepo
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", false, fmt.Errorf("db: create cache dir %q: %w", cacheDir, err)
	}

	dbPath := filepath.Join(cacheDir, dbAssetName)
	tagPath := filepath.Join(cacheDir, tagSidecarName)

	cached := fileExists(dbPath)
	offline := os.Getenv(EnvOffline) == "1"

	// readSidecar accepts both the v0 single-line tag (legacy binaries)
	// and the v1 JSON object. We only consume .Tag here — the sha256
	// and fetched_at fields feed the auto-update probe added in #197,
	// which is layered on top of this fast-path lookup.
	cachedTag := ""
	if s, err := readSidecar(tagPath); err == nil {
		cachedTag = s.Tag
	}

	isDev := isDevVersion(opts.AppVersion)

	// Fast path (zero API calls): cache exists, its tag matches the
	// binary's AppVersion, caller did not force. For dev builds we
	// accept any cached DB — "whatever you last fetched is fine,
	// don't nag" is the ergonomic choice for local iteration.
	if cached && !opts.Force {
		if isDev || cachedTag == opts.AppVersion {
			return dbPath, false, nil
		}
	}

	// Offline: we can't fetch, so either serve cache (only if dev or
	// matching tag, already handled above) or fail.
	if offline {
		if cached {
			return "", false, fmt.Errorf("db: cached DB is version %q but binary is %q and %s=1 prevents fetch; unset the env var or hand-place a matching DB at %s", cachedTag, opts.AppVersion, EnvOffline, dbPath)
		}
		return "", false, fmt.Errorf("db: no cached %s and %s=1; hand-place a DB at %s or unset the env var", dbAssetName, EnvOffline, dbPath)
	}

	// Resolve the release metadata. Dev builds fall back to /latest
	// with a WARN; tagged builds fetch their own tag's release.
	var meta releaseMeta
	var err error
	if isDev {
		slog.Warn("server.db_version_dev_fallback", "app_version", opts.AppVersion)
		meta, err = fetchLatestRelease(ctx, repo)
	} else {
		meta, err = fetchReleaseByTag(ctx, repo, opts.AppVersion)
	}
	if err != nil {
		// Version mismatch (or first fetch) + network/404 error:
		// fail loud. Serving a stale cache at a different version
		// than the binary expects risks schema/embedder drift, so the
		// first-fetch error generalises to the mismatch case.
		if cached {
			return "", false, fmt.Errorf("db: cached DB is version %q but binary is %q; could not fetch matching release: %w. Hints: check network reachability, pass -db <path> to pin a known-good file, or set %s=1 with a hand-placed file", cachedTag, opts.AppVersion, err, EnvOffline)
		}
		return "", false, fmt.Errorf("db: fetch release for %q: %w", opts.AppVersion, err)
	}

	// Dev fallback can still hit the zero-download short-circuit if
	// the latest release's tag happens to match the cached sidecar.
	if cached && !opts.Force && isDev && cachedTag == meta.Tag {
		return dbPath, false, nil
	}

	if err := fetchAndInstall(ctx, meta, dbPath, tagPath); err != nil {
		return "", false, fmt.Errorf("db: install %s: %w", dbAssetName, err)
	}
	return dbPath, cached, nil
}

// DefaultCacheDir resolves the cache root used by Bootstrap when the
// caller passes an empty CacheDir / DEADZONE_DB_CACHE is unset.
//
// Resolution order:
//
//  1. $DEADZONE_DB_CACHE if set.
//  2. Platform data dir (per-platform, see below).
//  3. ./.deadzone-cache/db as a last-resort fallback so Bootstrap can
//     still proceed when the home dir lookup fails.
//
// Per-platform data dirs:
//
//   - macOS:   ~/Library/Application Support/deadzone
//   - Linux:   $XDG_DATA_HOME/deadzone (falls back to ~/.local/share/deadzone)
//   - Windows: %LOCALAPPDATA%\deadzone (falls back to ~/AppData/Local/deadzone)
//
// We don't reuse os.UserCacheDir because the issue intentionally sites
// the DB under the persistent data dir, not the volatile cache dir.
// The DB is the user's installation, not a regenerable cache.
func DefaultCacheDir() string {
	if dir := os.Getenv(EnvCacheDir); dir != "" {
		return dir
	}
	var base string
	switch runtime.GOOS {
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, "Library", "Application Support")
		}
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			base = v
		} else if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, "AppData", "Local")
		}
	default:
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			base = v
		} else if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".local", "share")
		}
	}
	if base == "" {
		return filepath.Join(".deadzone-cache", "db")
	}
	return filepath.Join(base, "deadzone")
}

// isDevVersion recognises build strings that don't correspond to a
// published release tag:
//
//   - "" / "dev" — default when no -ldflags -X main.version is set
//   - anything ending in "-dirty" — justfile's build-release emits
//     this for a dirty working tree
//   - git-describe between-tags form ("v0.1.0-2-g1234567") — emitted
//     when the HEAD commit isn't itself tagged
//
// All three trigger the /releases/latest fallback so local dev stays
// ergonomic and CI's smoke build (which may not be at a tagged commit)
// keeps working.
func isDevVersion(v string) bool {
	if v == "" || v == DevVersion {
		return true
	}
	if strings.HasSuffix(v, "-dirty") {
		return true
	}
	return gitDescribeBetweenTagsRe.MatchString(v)
}

// releaseMeta is the trimmed view Bootstrap needs from the GitHub
// releases API: just the tag and the two asset URLs.
type releaseMeta struct {
	Tag       string
	DBURL     string
	SHA256URL string
}

// fetchLatestRelease hits /repos/{owner}/{repo}/releases/latest.
// Only used by the dev-version fallback.
func fetchLatestRelease(ctx context.Context, repo string) (releaseMeta, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", releasesAPIBase, repo)
	return decodeRelease(ctx, url, "")
}

// fetchReleaseByTag hits /repos/{owner}/{repo}/releases/tags/{tag}.
// A 404 surfaces as ErrNoReleaseForVersion so the caller can wrap
// with the actionable "downgrade the binary / hand-place a DB" hint.
func fetchReleaseByTag(ctx context.Context, repo, tag string) (releaseMeta, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/tags/%s", releasesAPIBase, repo, tag)
	return decodeRelease(ctx, url, tag)
}

// decodeRelease is the shared HTTP+JSON parser for both release
// endpoints. Passing the expected tag lets the 404 error message name
// the version the user was asking for.
func decodeRelease(ctx context.Context, url, expectedTag string) (releaseMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return releaseMeta{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := metadataHTTPClient.Do(req)
	if err != nil {
		return releaseMeta{}, fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		if expectedTag != "" {
			return releaseMeta{}, fmt.Errorf("%w %q (hint: hand-place a DB with -db <path>, or downgrade the binary to a version with a published DB)", ErrNoReleaseForVersion, expectedTag)
		}
		return releaseMeta{}, fmt.Errorf("%w (no latest release on %s)", ErrNoReleaseForVersion, url)
	}
	if resp.StatusCode != http.StatusOK {
		return releaseMeta{}, fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return releaseMeta{}, fmt.Errorf("decode response: %w", err)
	}
	if body.TagName == "" {
		return releaseMeta{}, errors.New("response has empty tag_name")
	}
	meta := releaseMeta{Tag: body.TagName}
	for _, a := range body.Assets {
		switch a.Name {
		case dbAssetName:
			meta.DBURL = a.BrowserDownloadURL
		case sha256AssetName:
			meta.SHA256URL = a.BrowserDownloadURL
		}
	}
	if meta.DBURL == "" {
		return releaseMeta{}, fmt.Errorf("release %s has no %s asset", body.TagName, dbAssetName)
	}
	if meta.SHA256URL == "" {
		return releaseMeta{}, fmt.Errorf("release %s has no %s asset", body.TagName, sha256AssetName)
	}
	return meta, nil
}

// fetchAndInstall does the heavy lifting: pull the sha256 sidecar,
// stream the DB into a temp file under the cache dir while hashing
// it, verify, atomic rename into place, best-effort tag sidecar.
//
// The temp file lives in the same directory as the destination so
// os.Rename is a true atomic-on-success move. On any failure between
// CreateTemp and Rename, the destination file remains untouched and
// the temp is cleaned up.
//
// Tag sidecar failure after a successful rename is logged as a WARN
// but not returned as an error (PR #110 review item 4): the DB is in
// place and serving. A missing/stale sidecar only means the next
// startup will re-fetch thinking the cache is stale, which is
// wasteful but correct.
func fetchAndInstall(ctx context.Context, meta releaseMeta, dbPath, tagPath string) error {
	wantHash, err := fetchSHA256(ctx, meta.SHA256URL)
	if err != nil {
		return fmt.Errorf("fetch sha256: %w", err)
	}

	cacheDir := filepath.Dir(dbPath)
	tmp, err := os.CreateTemp(cacheDir, dbAssetName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create tempfile in %s: %w", cacheDir, err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.DBURL, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	resp, err := assetHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", meta.DBURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: status %d", meta.DBURL, resp.StatusCode)
	}

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		return fmt.Errorf("stream %s: %w", meta.DBURL, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tempfile %s: %w", tmpPath, err)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != wantHash {
		return fmt.Errorf("sha256 mismatch on %s: got %s, want %s", meta.DBURL, got, wantHash)
	}

	if err := os.Rename(tmpPath, dbPath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, dbPath, err)
	}
	cleanup = false

	// We already have the verified sha in `got` from the streaming
	// hasher, so persisting it costs nothing and saves the next boot's
	// auto-update probe a 50 MB rehash. Sidecar write failure is
	// non-fatal: the DB is in place and serving; a missing/stale sidecar
	// only forces the next boot to re-fetch (wasteful but correct).
	side := sidecar{Tag: meta.Tag, SHA256: got, FetchedAt: time.Now().UTC()}
	if err := writeSidecar(tagPath, side); err != nil {
		slog.Warn("server.db_tag_sidecar_write_failed", "err", err.Error(), "path", tagPath)
	}
	return nil
}

// fetchSHA256 reads the sha256 sidecar uploaded alongside deadzone.db.
// Format is the standard GNU coreutils single-line "<hex>  filename"
// emitted by `dbrelease`. We only care about the hex digest; the
// filename is implicit.
func fetchSHA256(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	resp, err := assetHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	// 4 KiB is generous for a one-line sha256 file; cap to make a
	// hostile mirror responding with infinite bytes a clear OOM-free
	// error instead of an exhausted process.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", fmt.Errorf("read sha256 body: %w", err)
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return "", errors.New("sha256 file is empty")
	}
	hash := strings.ToLower(fields[0])
	if len(hash) != 64 {
		return "", fmt.Errorf("malformed sha256 %q (want 64 hex chars)", hash)
	}
	return hash, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
