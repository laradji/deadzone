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
	// EnvAutoUpdate gates the boot-time freshness probe added in #197.
	// Default (unset, or any non-"0"/"false" value) is enabled.
	// Setting "0" / "false" opts the implicit server-boot path out;
	// `fetch-db` still probes regardless because the env var only
	// governs the implicit path — explicit `fetch-db` is by definition
	// a user-driven refresh.
	EnvAutoUpdate = "DEADZONE_DB_AUTOUPDATE"

	BootstrapDefaultRepo = "laradji/deadzone"

	// DevVersion is the sentinel AppVersion for local / unreleased
	// builds. Bootstrap falls back to /releases/latest on it.
	DevVersion = "dev"

	dbAssetName       = "deadzone.db"
	sha256AssetName   = "deadzone.db.sha256"
	tagSidecarName    = "deadzone.db.release"
	dbDownloadNewName = "deadzone.db.new"

	// probeBudget caps the wall-clock spent on the boot-time freshness
	// probe (#197). Sub-100-byte response, fixed budget — no env
	// override (issue ADR: "keep the surface small").
	probeBudget = 3 * time.Second
)

// Three HTTP clients with different timeouts (PR #110 review item 2,
// extended for #197):
//   - metadata: 30s, sized for the GitHub API JSON responses.
//   - asset:    15min, sized for the ~50 MB deadzone.db transfer.
//   - probe:    3s,  sized for the boot-time freshness probe (#197).
//     The probe's whole point is "fail fast and fall back to cache" —
//     reusing metadataHTTPClient (30s) would let an offline boot stall
//     ten times longer than the spec budget.
var (
	metadataHTTPClient = &http.Client{Timeout: 30 * time.Second}
	assetHTTPClient    = &http.Client{Timeout: 15 * time.Minute}
	probeHTTPClient    = &http.Client{Timeout: probeBudget}
)

// releasesAPIBase is the GitHub API root. Split out so tests can point
// Bootstrap at a local httptest server.
var releasesAPIBase = "https://api.github.com"

// assetDownloadBase is the asset-CDN root. The probe and the
// version-bump fallback download deadzone.db / deadzone.db.sha256
// directly from /<repo>/releases/download/<tag>/<asset>, bypassing the
// API JSON detour. This costs one extra round-trip in the unhappy
// path (a 404 here means the asset truly doesn't exist on the tag,
// which the issue treats as a soft-fail) but saves one in the happy
// path of every boot. Split out so tests can override.
var assetDownloadBase = "https://github.com"

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
	// SkipAutoUpdateProbe disables the boot-time freshness probe added
	// in #197 even when the cache is hot and online. The server path
	// sets this from DEADZONE_DB_AUTOUPDATE=0; fetch-db NEVER sets it
	// (the probe is precisely what `fetch-db` is for, no-flag).
	SkipAutoUpdateProbe bool
	// Caller is a free-form label that flows into the structured logs
	// the auto-update probe emits (`db.update_*` events). The two
	// production callers are "server" and "fetch-db"; tests leave it
	// empty.
	Caller string
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
	// and the v1 JSON object. The full sidecar (sha256, fetched_at)
	// feeds the auto-update probe.
	var cachedSidecar sidecar
	if s, err := readSidecar(tagPath); err == nil {
		cachedSidecar = s
	}
	cachedTag := cachedSidecar.Tag

	isDev := isDevVersion(opts.AppVersion)

	// Fast path (zero API calls): cache exists, its tag matches the
	// binary's AppVersion, caller did not force. For dev builds we
	// accept any cached DB — "whatever you last fetched is fine,
	// don't nag" is the ergonomic choice for local iteration.
	if cached && !opts.Force {
		if isDev || cachedTag == opts.AppVersion {
			// Auto-update probe (#197). Runs ONLY on a confirmed tag
			// match (skipping dev fallback — there's no specific
			// release to probe), and only when not opted out and not
			// in offline mode. All non-success outcomes other than a
			// hash-verified corrupt download are soft-fails: the
			// cached DB serves and the probe error is swallowed.
			if !isDev && cachedTag == opts.AppVersion && !opts.SkipAutoUpdateProbe && !offline {
				updated, err := probeAndMaybeSwap(ctx, repo, opts.AppVersion, dbPath, tagPath, cachedSidecar, opts.Caller)
				if err != nil {
					return "", false, err
				}
				return dbPath, updated, nil
			}
			if !isDev && cachedTag == opts.AppVersion && opts.SkipAutoUpdateProbe {
				slog.Info("db.update_check_skipped", "tag", cachedTag, "reason", "disabled_via_env", "caller", opts.Caller)
			}
			if !isDev && cachedTag == opts.AppVersion && offline {
				slog.Info("db.update_check_skipped", "tag", cachedTag, "reason", "offline_mode", "caller", opts.Caller)
			}
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

// probeAndMaybeSwap is the auto-update probe added in #197. It runs
// inside the fast-path branch when (a) the cache exists, (b) its tag
// matches the binary's AppVersion, (c) the caller hasn't opted out
// via SkipAutoUpdateProbe, and (d) we're not in offline mode.
//
// Soft-fail policy (issue ADR): every non-success outcome OTHER than
// a sha-verified corrupt download is treated as a recoverable
// transient — the cached DB stays and we return (false, nil). The
// only error path that propagates is the corrupt-download case:
// `deadzone.db.new` is deleted and the caller gets a non-nil error
// so boot does NOT proceed against an unverified DB.
//
// Sidecar legacy handling: a v0 sidecar carries no sha256, so we
// compute it once from the on-disk file and rewrite the sidecar
// before issuing the probe. Subsequent boots take the cheap path.
func probeAndMaybeSwap(ctx context.Context, repo, tag, dbPath, tagPath string, side sidecar, caller string) (bool, error) {
	localSHA := side.SHA256
	if localSHA == "" {
		// First boot post-upgrade onto a v0 sidecar. Compute the sha
		// once so the next boot takes the cheap path. A failure here
		// (e.g. the cache file vanished between fileExists and now) is
		// classified as parse_error and soft-fails the probe — boot
		// continues, the next attempt will retry.
		h, err := hashFile(dbPath)
		if err != nil {
			slog.Info("db.update_check_skipped", "tag", tag, "reason", "parse_error", "caller", caller, "err", err.Error())
			return false, nil
		}
		localSHA = h
		side.SHA256 = h
		if side.FetchedAt.IsZero() {
			// Best-effort: stamp the on-disk mtime so the field has
			// SOME signal even on the legacy upgrade path. Falls back
			// to "now" if stat fails.
			if info, statErr := os.Stat(dbPath); statErr == nil {
				side.FetchedAt = info.ModTime().UTC()
			} else {
				side.FetchedAt = time.Now().UTC()
			}
		}
		if err := writeSidecar(tagPath, side); err != nil {
			slog.Warn("server.db_tag_sidecar_write_failed", "err", err.Error(), "path", tagPath)
		}
	}

	slog.Debug("db.update_check_start", "tag", tag, "local_sha256", localSHA, "caller", caller)

	remoteSHA, reason, err := probeFetchSHA(ctx, repo, tag)
	if err != nil {
		slog.Info("db.update_check_skipped", "tag", tag, "reason", reason, "caller", caller, "err", err.Error())
		return false, nil
	}

	if remoteSHA == localSHA {
		slog.Debug("db.update_check_no_change", "tag", tag, "sha256", remoteSHA, "caller", caller)
		return false, nil
	}

	slog.Info("db.update_available", "tag", tag, "local_sha256", localSHA, "remote_sha256", remoteSHA, "caller", caller)

	start := time.Now()
	bytesDL, phase, err := probeDownloadAndSwap(ctx, repo, tag, dbPath, remoteSHA)
	if err != nil {
		slog.Error("db.update_failed", "tag", tag, "phase", phase, "err", err.Error(), "caller", caller)
		// Hash mismatch on the downloaded bytes is the ONE case the
		// issue says must abort boot. Any other download-phase failure
		// (network drop mid-stream, disk full) is recoverable on the
		// next boot, so we soft-fall-back to the cache. The phase
		// returned by probeDownloadAndSwap distinguishes the two.
		if phase == "verify" {
			return false, fmt.Errorf("db: auto-update %w", err)
		}
		return false, nil
	}

	side = sidecar{Tag: tag, SHA256: remoteSHA, FetchedAt: time.Now().UTC()}
	if err := writeSidecar(tagPath, side); err != nil {
		slog.Warn("server.db_tag_sidecar_write_failed", "err", err.Error(), "path", tagPath)
	}

	slog.Info("db.update_applied",
		"tag", tag,
		"old_sha256", localSHA,
		"new_sha256", remoteSHA,
		"bytes_downloaded", bytesDL,
		"duration_ms", time.Since(start).Milliseconds(),
		"caller", caller,
	)
	return true, nil
}

// probeFetchSHA does the cheap GET on the deadzone.db.sha256 asset.
// Returns (sha, "", nil) on success, ("", reason, err) on every
// failure mode the issue's structured-log table enumerates.
//
// The reason vocabulary maps to slog's `reason` field on
// db.update_check_skipped: network_timeout, network_error,
// parse_error.
func probeFetchSHA(ctx context.Context, repo, tag string) (sha, reason string, err error) {
	url := fmt.Sprintf("%s/%s/releases/download/%s/%s", assetDownloadBase, repo, tag, sha256AssetName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "parse_error", err
	}
	resp, err := probeHTTPClient.Do(req)
	if err != nil {
		// http.Client wraps deadline exceeded inside *url.Error whose
		// Timeout() reports true. Distinguishing this from a generic
		// network error matters because the issue's
		// db.update_check_skipped log surfaces "network_timeout"
		// specifically (operators want to know "your network is slow"
		// vs "your network is broken").
		if errors.Is(err, context.DeadlineExceeded) {
			return "", "network_timeout", err
		}
		var netErr interface{ Timeout() bool }
		if errors.As(err, &netErr) && netErr.Timeout() {
			return "", "network_timeout", err
		}
		return "", "network_error", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "network_error", fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	// 4 KiB is generous for a one-line sha256 file; cap defends
	// against a hostile mirror responding with infinite bytes.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", "network_error", fmt.Errorf("read sha256 body: %w", err)
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return "", "parse_error", errors.New("sha256 file is empty")
	}
	hash := strings.ToLower(fields[0])
	if len(hash) != 64 {
		return "", "parse_error", fmt.Errorf("malformed sha256 %q (want 64 hex chars)", hash)
	}
	return hash, "", nil
}

// probeDownloadAndSwap downloads deadzone.db to a deterministic
// .new path, sha-verifies against expectedSHA, and atomically renames
// over the live cache. The deterministic name (deadzone.db.new) was
// called out by the issue spec; the existing fetchAndInstall uses
// CreateTemp instead because it has no expected name to coordinate
// against.
//
// Returns (bytesWritten, phase, err) where phase ∈ {download, verify,
// rename, ""} so the caller can populate the structured-log
// `db.update_failed.phase` field. An empty phase means success.
func probeDownloadAndSwap(ctx context.Context, repo, tag, dbPath, expectedSHA string) (int64, string, error) {
	url := fmt.Sprintf("%s/%s/releases/download/%s/%s", assetDownloadBase, repo, tag, dbAssetName)
	cacheDir := filepath.Dir(dbPath)
	newPath := filepath.Join(cacheDir, dbDownloadNewName)

	// Truncate any leftover .new from a torn previous run before we
	// commit to overwriting it; otherwise a partial download that
	// died after streaming but before rename would silently corrupt
	// the next attempt.
	f, err := os.Create(newPath)
	if err != nil {
		return 0, "download", fmt.Errorf("create %s: %w", newPath, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = f.Close()
			_ = os.Remove(newPath)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "download", fmt.Errorf("new request: %w", err)
	}
	// Reuse the long-timeout asset client — the probe budget governs
	// only the sha sidecar GET, not the multi-MB download.
	resp, err := assetHTTPClient.Do(req)
	if err != nil {
		return 0, "download", fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "download", fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}

	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hasher), resp.Body)
	if err != nil {
		return n, "download", fmt.Errorf("stream %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		return n, "download", fmt.Errorf("close %s: %w", newPath, err)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != expectedSHA {
		// "verify" is the one phase the caller MUST treat as fatal —
		// the cache stays intact and we explicitly leave cleanup=true
		// so the corrupt .new file is removed.
		return n, "verify", fmt.Errorf("sha256 mismatch on %s: got %s, want %s", url, got, expectedSHA)
	}

	if err := os.Rename(newPath, dbPath); err != nil {
		return n, "rename", fmt.Errorf("rename %s -> %s: %w", newPath, dbPath, err)
	}
	cleanup = false
	return n, "", nil
}

// hashFile streams path through sha256 and returns the hex digest.
// Used by the auto-update probe when migrating a v0 sidecar (which
// has no recorded sha256) to v1.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
