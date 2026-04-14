// bootstrap.go owns the consumer-side fetch + auto-upgrade flow for
// deadzone.db, mirroring internal/ort/ort.go's symmetric flow for the
// onnxruntime shared library. See issue #108 for the full design and
// behavior matrix.
//
// Three trigger paths share this implementation:
//
//  1. First run, no cached DB → fetch latest release's deadzone.db into
//     cache, serve.
//  2. Subsequent run, cached DB tag matches latest → use cache, skip
//     the asset download (still hits the API to compare tags; "no
//     network" in the issue means "no asset transfer").
//  3. Subsequent run, cached DB tag != latest → fetch new, atomic swap,
//     serve new version.
//
// Env-var escape hatches (matching DEADZONE_ORT_CACHE / DEADZONE_HUGOT_CACHE):
//
//   DEADZONE_DB_CACHE             — override the cache directory
//   DEADZONE_DB_OFFLINE=1         — never make a network call; fail loud
//                                   on first run if nothing cached
//   DEADZONE_DB_NO_AUTO_UPGRADE=1 — skip the staleness check; first-run
//                                   fetch still happens

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
	"runtime"
	"strings"
	"time"
)

// Cache / asset constants. The asset names match what `deadzone
// dbrelease` uploads; changing either side requires changing both.
const (
	EnvCacheDir      = "DEADZONE_DB_CACHE"
	EnvOffline       = "DEADZONE_DB_OFFLINE"
	EnvNoAutoUpgrade = "DEADZONE_DB_NO_AUTO_UPGRADE"

	BootstrapDefaultRepo = "laradji/deadzone"

	dbAssetName     = "deadzone.db"
	sha256AssetName = "deadzone.db.sha256"
	tagSidecarName  = "deadzone.db.release"
)

// httpClient bounds the worst-case download. The 50–200 MB DB on a
// slow residential link can take a while; 15 minutes is the same upper
// bound the ORT bootstrap uses (rounded up because the DB is larger).
var httpClient = &http.Client{Timeout: 15 * time.Minute}

// releasesAPIBase is the GitHub API root. Split out so tests can point
// Bootstrap at a local httptest server.
var releasesAPIBase = "https://api.github.com"

// BootstrapOptions lets callers (mainly the fetch-db subcommand and
// tests) override what Bootstrap pulls from env / defaults. Zero value
// matches Bootstrap's behavior exactly.
type BootstrapOptions struct {
	// CacheDir overrides DEADZONE_DB_CACHE / the platform default.
	CacheDir string
	// Repo overrides BootstrapDefaultRepo (owner/name).
	Repo string
	// Force re-fetches even when the cached tag matches the latest
	// release. fetch-db --force uses this; the server path leaves it
	// false so a no-op startup stays a no-op.
	Force bool
}

// Bootstrap ensures a deadzone.db is present at the default cache
// location and returns its path. Handles first-fetch, the staleness
// check, and the auto-upgrade swap, bounded by DEADZONE_DB_OFFLINE and
// DEADZONE_DB_NO_AUTO_UPGRADE.
//
// Returns (path, upgraded, error) where upgraded is true when this call
// just replaced a stale cache. First-fetch returns upgraded=false (no
// previous file existed to "upgrade" from).
func Bootstrap(ctx context.Context) (string, bool, error) {
	return BootstrapWithOptions(ctx, BootstrapOptions{})
}

// BootstrapWithOptions is the explicit-options form of Bootstrap. The
// fetch-db subcommand uses it for -force; tests use it to inject a
// CacheDir + Repo without touching env vars.
func BootstrapWithOptions(ctx context.Context, opts BootstrapOptions) (string, bool, error) {
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
	noUpgrade := os.Getenv(EnvNoAutoUpgrade) == "1"

	// Cached + (noUpgrade or offline) and not -force → short-circuit.
	// This is the "no network" row of the matrix.
	if cached && !opts.Force && (noUpgrade || offline) {
		return dbPath, false, nil
	}
	// Not cached + offline → fail loud. We can't satisfy the request
	// without a network call and the operator opted out of one.
	if !cached && offline {
		return "", false, fmt.Errorf("db: no cached %s and %s=1; hand-place a DB at %s or unset the env var", dbAssetName, EnvOffline, dbPath)
	}

	latest, err := fetchLatestRelease(ctx, repo)
	if err != nil {
		// Cached file present → degrade to "serve stale". Only the
		// first-run no-cache path can promote a metadata error to a
		// startup error.
		if cached {
			slog.Warn("server.db_upgrade_failed", "err", err.Error())
			return dbPath, false, nil
		}
		return "", false, fmt.Errorf("db: fetch latest release: %w", err)
	}

	// Tag-match short circuit (skips the heavy asset download).
	if cached && !opts.Force {
		cachedTag, _ := os.ReadFile(tagPath)
		if strings.TrimSpace(string(cachedTag)) == latest.Tag {
			return dbPath, false, nil
		}
	}

	if err := fetchAndInstall(ctx, latest, dbPath, tagPath); err != nil {
		if cached {
			slog.Warn("server.db_upgrade_failed", "err", err.Error())
			return dbPath, false, nil
		}
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
// Per-platform data dirs (matching the issue spec):
//
//   - macOS:   ~/Library/Application Support/deadzone
//   - Linux:   $XDG_DATA_HOME/deadzone (falls back to ~/.local/share/deadzone)
//   - Windows: %LOCALAPPDATA%\deadzone (falls back to ~/AppData/Local/deadzone)
//
// We don't reuse os.UserCacheDir because the issue intentionally sites
// the DB under the persistent data dir (Application Support / data),
// not the volatile cache dir (Caches). The DB is the user's
// installation, not a regenerable cache.
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

// releaseMeta is the trimmed view Bootstrap needs from the GitHub
// releases API: just the tag and the two asset URLs.
type releaseMeta struct {
	Tag       string
	DBURL     string
	SHA256URL string
}

// fetchLatestRelease hits /repos/{owner}/{repo}/releases/latest and
// pulls out the tag plus the two asset download URLs. Both assets must
// be present on the release; a release missing either is a packaging
// bug on the publishing side and is surfaced as an error here so the
// user sees it immediately, not after a partial install.
func fetchLatestRelease(ctx context.Context, repo string) (releaseMeta, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", releasesAPIBase, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return releaseMeta{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return releaseMeta{}, fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
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

// fetchAndInstall does the heavy lifting: pull the sha256 sidecar, then
// stream the DB into a temp file under the cache dir while hashing it,
// verify, atomic rename into place, write the tag sidecar.
//
// The temp file lives in the same directory as the destination so
// os.Rename is a true atomic-on-success move. On any failure between
// CreateTemp and Rename, the destination file remains untouched and
// the temp is cleaned up.
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
	resp, err := httpClient.Do(req)
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

	if err := os.WriteFile(tagPath, []byte(meta.Tag+"\n"), 0o644); err != nil {
		// The DB itself is in place; surface the sidecar failure but
		// don't unwind the install. Worst case: next startup re-fetches
		// thinking the cache is stale, which is wasteful but correct.
		return fmt.Errorf("write tag sidecar %s: %w", tagPath, err)
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
	resp, err := httpClient.Do(req)
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
