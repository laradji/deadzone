package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dbBootstrapEnv lists the env vars Bootstrap reads. Every test
// neutralizes them via t.Setenv so a developer's local
// DEADZONE_DB_CACHE / DEADZONE_DB_OFFLINE / DEADZONE_DB_AUTOUPDATE
// doesn't bleed in.
var dbBootstrapEnv = []string{EnvCacheDir, EnvOffline, EnvAutoUpdate}

func clearBootstrapEnv(t *testing.T) {
	t.Helper()
	for _, k := range dbBootstrapEnv {
		t.Setenv(k, "")
	}
}

// fakeRelease bundles the bytes a httptest fixture needs to serve a
// single fake release: the DB asset payload, its sha256 hex, and the
// tag string the API endpoint reports.
type fakeRelease struct {
	tag    string
	dbBody []byte
	dbHash string
}

func newFakeRelease(t *testing.T, tag, content string) fakeRelease {
	t.Helper()
	body := []byte(content)
	sum := sha256.Sum256(body)
	return fakeRelease{tag: tag, dbBody: body, dbHash: hex.EncodeToString(sum[:])}
}

// fixtureServer wires together the API endpoints and asset endpoints
// behind a single httptest server so the fake "browser_download_url"
// values can point back at the same host.
//
// Three URL families are served:
//
//  1. /repos/<owner>/<repo>/releases/{tags/<t>,latest} — the JSON API
//     used by the version-bump fetch path (existing).
//  2. /dl/<tag>/<asset> — the synthetic "browser_download_url" the
//     JSON above advertises; lets the API path stay decoupled from
//     the production asset URL shape.
//  3. /<owner>/<repo>/releases/download/<tag>/<asset> — mirrors the
//     real assetDownloadBase URL shape so the auto-update probe
//     (#197) hits the fixture instead of github.com when withAPIBase
//     has wired the probe base accordingly.
//
// releases maps tag → fakeRelease so a single fixture can serve both
// /releases/tags/<t> and the synthetic "latest" pick. The tags slice
// preserves insertion order — latestTag returns the last-added tag so
// tests that simulate "newer release dropped" can append to it
// mid-test.
type fixtureServer struct {
	srv          *httptest.Server
	releases     map[string]fakeRelease
	tagOrder     []string
	apiCalls     atomic.Int32
	dbCalls      atomic.Int32
	sha256Calls  atomic.Int32
	probeCalls   atomic.Int32 // sha256 GETs on the /releases/download path (auto-update probe)
	probeDLCalls atomic.Int32 // db GETs on the /releases/download path (auto-update applied)
	failAPI      atomic.Bool  // when true, API endpoints return 500
	corruptSHA   atomic.Bool  // when true, sha256 comes back as zeroes (mismatch)
	missingDB    atomic.Bool  // when true, the JSON omits the deadzone.db asset
	probeHang    atomic.Bool  // when true, /releases/download/<tag>/.sha256 hangs past the probe budget
	probeSHA     atomic.Value // override remote sha as a string; empty/unset → use rel.dbHash
	probeBody    atomic.Value // override the bytes served at /releases/download/<tag>/deadzone.db
}

func (f *fixtureServer) latestTag() string {
	if len(f.tagOrder) == 0 {
		return ""
	}
	return f.tagOrder[len(f.tagOrder)-1]
}

func newFixtureServer(t *testing.T, releases ...fakeRelease) *fixtureServer {
	t.Helper()
	fs := &fixtureServer{releases: map[string]fakeRelease{}}
	for _, r := range releases {
		fs.releases[r.tag] = r
		fs.tagOrder = append(fs.tagOrder, r.tag)
	}
	mux := http.NewServeMux()
	serveRelease := func(w http.ResponseWriter, rel fakeRelease) {
		assets := []map[string]string{
			{"name": sha256AssetName, "browser_download_url": fs.srv.URL + "/dl/" + rel.tag + "/" + sha256AssetName},
		}
		if !fs.missingDB.Load() {
			assets = append(assets, map[string]string{
				"name":                 dbAssetName,
				"browser_download_url": fs.srv.URL + "/dl/" + rel.tag + "/" + dbAssetName,
			})
		}
		body := map[string]any{"tag_name": rel.tag, "assets": assets}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	mux.HandleFunc("/repos/laradji/deadzone/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fs.apiCalls.Add(1)
		if fs.failAPI.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		tag := fs.latestTag()
		rel, ok := fs.releases[tag]
		if !ok {
			http.Error(w, "no releases", http.StatusNotFound)
			return
		}
		serveRelease(w, rel)
	})
	mux.HandleFunc("/repos/laradji/deadzone/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		fs.apiCalls.Add(1)
		if fs.failAPI.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		tag := strings.TrimPrefix(r.URL.Path, "/repos/laradji/deadzone/releases/tags/")
		rel, ok := fs.releases[tag]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		serveRelease(w, rel)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		// Path format: /dl/<tag>/<asset>
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/dl/"), "/")
		if len(parts) != 2 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		tag, asset := parts[0], parts[1]
		rel, ok := fs.releases[tag]
		if !ok {
			http.Error(w, "unknown tag", http.StatusNotFound)
			return
		}
		switch asset {
		case dbAssetName:
			fs.dbCalls.Add(1)
			_, _ = w.Write(rel.dbBody)
		case sha256AssetName:
			fs.sha256Calls.Add(1)
			hash := rel.dbHash
			if fs.corruptSHA.Load() {
				hash = strings.Repeat("0", 64)
			}
			fmt.Fprintf(w, "%s  %s\n", hash, dbAssetName)
		default:
			http.Error(w, "unknown asset", http.StatusNotFound)
		}
	})
	// Mirror the production asset URL shape so the #197 probe can hit
	// this fixture: GET /<owner>/<repo>/releases/download/<tag>/<asset>.
	mux.HandleFunc("/laradji/deadzone/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/laradji/deadzone/releases/download/"), "/")
		if len(parts) != 2 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		tag, asset := parts[0], parts[1]
		rel, ok := fs.releases[tag]
		if !ok {
			http.Error(w, "unknown tag", http.StatusNotFound)
			return
		}
		switch asset {
		case sha256AssetName:
			fs.probeCalls.Add(1)
			if fs.probeHang.Load() {
				// Hang past the probe budget so the client times out.
				// 5×budget gives a comfortable margin for slow CI.
				select {
				case <-r.Context().Done():
				case <-time.After(5 * probeBudget):
				}
				return
			}
			hash := rel.dbHash
			if v, ok := fs.probeSHA.Load().(string); ok && v != "" {
				hash = v
			}
			fmt.Fprintf(w, "%s  %s\n", hash, dbAssetName)
		case dbAssetName:
			fs.probeDLCalls.Add(1)
			body := rel.dbBody
			if v, ok := fs.probeBody.Load().([]byte); ok && v != nil {
				body = v
			}
			_, _ = w.Write(body)
		default:
			http.Error(w, "unknown asset", http.StatusNotFound)
		}
	})
	fs.srv = httptest.NewServer(mux)
	t.Cleanup(fs.srv.Close)
	return fs
}

// withAPIBase swaps both releasesAPIBase AND assetDownloadBase to the
// fixture URL for the duration of the test. The two are coupled by
// convention: a test that mocks the GitHub API also mocks the asset
// CDN, otherwise the auto-update probe (which hits the CDN directly)
// would leak real github.com requests on every cache-hit test. The
// cleanup restores both so a failing test can't poison siblings.
func withAPIBase(t *testing.T, base string) {
	t.Helper()
	origAPI := releasesAPIBase
	origAsset := assetDownloadBase
	releasesAPIBase = base
	assetDownloadBase = base
	t.Cleanup(func() {
		releasesAPIBase = origAPI
		assetDownloadBase = origAsset
	})
}

// seedCache writes dbBody + tag sidecar into dir so the tag-match
// short-circuit tests can assert what happens on a populated cache.
func seedCache(t *testing.T, dir, dbBody, tag string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, dbAssetName), []byte(dbBody), 0o644); err != nil {
		t.Fatalf("seed cache db: %v", err)
	}
	if tag != "" {
		if err := os.WriteFile(filepath.Join(dir, tagSidecarName), []byte(tag+"\n"), 0o644); err != nil {
			t.Fatalf("seed cache tag: %v", err)
		}
	}
}

// TestBootstrap_FirstFetch covers the first-run happy path: empty
// cache, binary's AppVersion maps to a real release → DB lands in
// cache, sha256 verifies, tag sidecar records AppVersion, upgraded is
// false because no previous file existed.
func TestBootstrap_FirstFetch(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.0.0", "fake-db-content-v1")
	fix := newFixtureServer(t, rel)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	path, upgraded, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
		CacheDir:   cacheDir,
		AppVersion: "v1.0.0",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if upgraded {
		t.Errorf("upgraded=true on first fetch, want false")
	}
	if path != filepath.Join(cacheDir, dbAssetName) {
		t.Errorf("path = %q, want %q", path, filepath.Join(cacheDir, dbAssetName))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed db: %v", err)
	}
	if string(got) != string(rel.dbBody) {
		t.Errorf("installed body = %q, want %q", got, rel.dbBody)
	}
	side, err := readSidecar(filepath.Join(cacheDir, tagSidecarName))
	if err != nil {
		t.Fatalf("read tag sidecar: %v", err)
	}
	if side.Tag != rel.tag {
		t.Errorf("sidecar Tag = %q, want %q", side.Tag, rel.tag)
	}
	if side.SHA256 != rel.dbHash {
		t.Errorf("sidecar SHA256 = %q, want %q", side.SHA256, rel.dbHash)
	}
	if side.FetchedAt.IsZero() {
		t.Errorf("sidecar FetchedAt is zero, want a real timestamp")
	}
	if n := fix.dbCalls.Load(); n != 1 {
		t.Errorf("db asset hit %d times, want 1", n)
	}
}

// TestBootstrap_TagMatchZeroAPICalls pins the opt-out fast path:
// cache exists, sidecar tag == AppVersion, AND SkipAutoUpdateProbe is
// set → Bootstrap returns the cached path without any HTTP traffic at
// all. Pre-#197 this WAS the steady state; post-#197 the steady state
// includes a 78-byte probe, so this test specifically asserts the
// DEADZONE_DB_AUTOUPDATE=0 escape hatch still produces the legacy
// zero-network behaviour.
func TestBootstrap_TagMatchZeroAPICalls(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.0.0", "fake-db-content")
	fix := newFixtureServer(t, rel)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	seedCache(t, cacheDir, "cached-bytes", "v1.0.0")

	path, upgraded, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
		CacheDir:            cacheDir,
		AppVersion:          "v1.0.0",
		SkipAutoUpdateProbe: true,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if upgraded {
		t.Errorf("upgraded=true on tag match, want false")
	}
	if path != filepath.Join(cacheDir, dbAssetName) {
		t.Errorf("path = %q, want cached path", path)
	}
	if n := fix.apiCalls.Load(); n != 0 {
		t.Errorf("API hit %d times on tag match, want 0 (fast-path must be zero-network)", n)
	}
	if n := fix.dbCalls.Load(); n != 0 {
		t.Errorf("db asset hit %d times on tag match, want 0", n)
	}
	if n := fix.probeCalls.Load(); n != 0 {
		t.Errorf("probe hit %d times with SkipAutoUpdateProbe=true, want 0", n)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "cached-bytes" {
		t.Errorf("cached body was overwritten on tag match: %q", got)
	}
}

// TestBootstrap_BinaryUpgradeSwapsDB is the version-bump path: the
// binary was upgraded (AppVersion changed) so the cached sidecar
// tag no longer matches. Bootstrap must fetch the new tag's release
// and atomic-swap the DB, returning upgraded=true.
func TestBootstrap_BinaryUpgradeSwapsDB(t *testing.T) {
	clearBootstrapEnv(t)
	relOld := newFakeRelease(t, "v1.0.0", "old-content")
	relNew := newFakeRelease(t, "v1.1.0", "new-content")
	fix := newFixtureServer(t, relOld, relNew)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	seedCache(t, cacheDir, "old-bytes", "v1.0.0")

	path, upgraded, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
		CacheDir:   cacheDir,
		AppVersion: "v1.1.0",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !upgraded {
		t.Errorf("upgraded=false on binary upgrade, want true")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(relNew.dbBody) {
		t.Errorf("after upgrade body = %q, want %q", got, relNew.dbBody)
	}
	side, err := readSidecar(filepath.Join(cacheDir, tagSidecarName))
	if err != nil {
		t.Fatalf("read tag sidecar: %v", err)
	}
	if side.Tag != relNew.tag {
		t.Errorf("sidecar Tag = %q, want %q", side.Tag, relNew.tag)
	}
}

// TestBootstrap_VersionMismatchNetworkErrorFailsLoud covers the
// failure-to-degrade case: cached sidecar disagrees with AppVersion
// AND the API call fails. Previous iteration served the stale cache;
// PR #110 review called that out as unsafe because the on-disk
// schema/content doesn't match what the binary expects. Must error
// out with hints at the three recovery paths.
func TestBootstrap_VersionMismatchNetworkErrorFailsLoud(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.1.0", "new-content")
	fix := newFixtureServer(t, rel)
	fix.failAPI.Store(true)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	seedCache(t, cacheDir, "old-bytes", "v1.0.0")

	_, _, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
		CacheDir:   cacheDir,
		AppVersion: "v1.1.0",
	})
	if err == nil {
		t.Fatalf("Bootstrap returned nil err on version mismatch + network error, want loud failure")
	}
	// Error message must surface the three recovery hints the PR
	// review called for.
	msg := err.Error()
	for _, want := range []string{`"v1.0.0"`, `"v1.1.0"`, "-db", EnvOffline} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing hint %q", msg, want)
		}
	}
}

// TestBootstrap_OfflineNoCache covers DEADZONE_DB_OFFLINE=1 with
// nothing cached: must error, mention the env var, leave no partial.
func TestBootstrap_OfflineNoCache(t *testing.T) {
	clearBootstrapEnv(t)
	t.Setenv(EnvOffline, "1")

	cacheDir := t.TempDir()
	_, _, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
		CacheDir:   cacheDir,
		AppVersion: "v1.0.0",
	})
	if err == nil {
		t.Fatal("Bootstrap returned no error on offline + no cache")
	}
	if !strings.Contains(err.Error(), EnvOffline) {
		t.Errorf("error %q does not mention %s", err, EnvOffline)
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, dbAssetName)); !os.IsNotExist(statErr) {
		t.Errorf("expected no db file, stat err = %v", statErr)
	}
}

// TestBootstrap_OfflineMismatchedCacheFails covers the "offline +
// cached DB is wrong version for this binary" path: even though the
// DB exists, we can't verify it matches the binary's AppVersion and
// we're forbidden from asking the network, so we must fail. The
// cache's version is already known to differ (otherwise we'd have
// hit the zero-network fast path before the offline check).
func TestBootstrap_OfflineMismatchedCacheFails(t *testing.T) {
	clearBootstrapEnv(t)
	t.Setenv(EnvOffline, "1")

	cacheDir := t.TempDir()
	seedCache(t, cacheDir, "old-bytes", "v1.0.0")

	_, _, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
		CacheDir:   cacheDir,
		AppVersion: "v1.1.0",
	})
	if err == nil {
		t.Fatal("Bootstrap returned nil err on offline + mismatched cache")
	}
	msg := err.Error()
	for _, want := range []string{EnvOffline, "v1.0.0", "v1.1.0"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing hint %q", msg, want)
		}
	}
}

// TestBootstrap_SHAMismatch covers the corrupted-mirror case: API +
// asset reachable, but the sha256 of the downloaded bytes doesn't
// match the sidecar. Must error AND must leave no partial file.
func TestBootstrap_SHAMismatch(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.0.0", "fake-db-content")
	fix := newFixtureServer(t, rel)
	fix.corruptSHA.Store(true)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	if _, _, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
		CacheDir:   cacheDir,
		AppVersion: "v1.0.0",
	}); err == nil {
		t.Fatalf("Bootstrap returned no error on sha256 mismatch")
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, dbAssetName)); !os.IsNotExist(statErr) {
		t.Errorf("partial file left in cache after sha mismatch: stat err = %v", statErr)
	}
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), dbAssetName+".tmp-") {
			t.Errorf("tempfile %q not cleaned up", e.Name())
		}
	}
}

// TestBootstrap_ForceRefetches covers fetch-db --force: cached tag
// matches AppVersion, but Force=true must re-download anyway to
// recover from local corruption. upgraded=true because the cache
// existed and was replaced.
func TestBootstrap_ForceRefetches(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.0.0", "fresh-content")
	fix := newFixtureServer(t, rel)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	seedCache(t, cacheDir, "cached-bytes", "v1.0.0")

	path, upgraded, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
		CacheDir:   cacheDir,
		AppVersion: "v1.0.0",
		Force:      true,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !upgraded {
		t.Errorf("upgraded=false with Force, want true")
	}
	if n := fix.dbCalls.Load(); n != 1 {
		t.Errorf("db asset hit %d times with Force, want 1", n)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(rel.dbBody) {
		t.Errorf("after force body = %q, want %q", got, rel.dbBody)
	}
}

// TestBootstrap_DevFallsBackToLatest covers the dev-binary path:
// AppVersion="dev" triggers the /releases/latest fallback so local
// iteration stays ergonomic. fetchReleaseByTag must not be called.
func TestBootstrap_DevFallsBackToLatest(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.0.0", "latest-content")
	fix := newFixtureServer(t, rel)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	path, _, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
		CacheDir:   cacheDir,
		AppVersion: "dev",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(rel.dbBody) {
		t.Errorf("dev fallback body = %q, want %q", got, rel.dbBody)
	}
	side, err := readSidecar(filepath.Join(cacheDir, tagSidecarName))
	if err != nil {
		t.Fatalf("read tag sidecar: %v", err)
	}
	if side.Tag != rel.tag {
		t.Errorf("sidecar Tag = %q, want %q", side.Tag, rel.tag)
	}
}

// TestBootstrap_UnknownTagGives404Error covers the tag-404 path: the
// binary was built for a version that doesn't have a published DB
// yet. Error must wrap ErrNoReleaseForVersion and contain the
// actionable hint about -db / downgrading.
func TestBootstrap_UnknownTagGives404Error(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.0.0", "content")
	fix := newFixtureServer(t, rel)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	_, _, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
		CacheDir:   cacheDir,
		AppVersion: "v99.0.0",
	})
	if err == nil {
		t.Fatal("Bootstrap returned no error on unknown tag")
	}
	if !errors.Is(err, ErrNoReleaseForVersion) {
		t.Errorf("error %q does not wrap ErrNoReleaseForVersion", err)
	}
	if !strings.Contains(err.Error(), "-db") {
		t.Errorf("error %q missing -db hint", err)
	}
}

// TestBootstrap_MissingDBAsset covers a release that exists but was
// published without the deadzone.db asset. The error must name the
// missing asset so a publisher-side bug is obvious on first fetch
// instead of silently serving nothing.
func TestBootstrap_MissingDBAsset(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.0.0", "unused")
	fix := newFixtureServer(t, rel)
	fix.missingDB.Store(true)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	_, _, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
		CacheDir:   cacheDir,
		AppVersion: "v1.0.0",
	})
	if err == nil {
		t.Fatal("Bootstrap returned no error with missing deadzone.db asset")
	}
	if !strings.Contains(err.Error(), dbAssetName) {
		t.Errorf("error %q does not name the missing %s asset", err, dbAssetName)
	}
}

// TestIsDevVersion pins the dev-detection heuristic. The production
// behavior hinges on this: a false negative (tagged build classified
// as dev) is harmless (/latest fallback still works); a false
// positive (released build classified as dev) silently drifts DB
// choice away from the pinned version.
func TestIsDevVersion(t *testing.T) {
	tests := []struct {
		v    string
		want bool
	}{
		{"", true},
		{"dev", true},
		{"v0.1.0", false},
		{"v1.0.0", false},
		{"v0.1.0-rc1", false}, // legitimate pre-release tag
		{"v0.1.0-dirty", true},
		{"v0.1.0-2-g1234567", true},
		{"v0.1.0-2-g1234567-dirty", true},
		{"v0.1.0-12-gabcdef0", true},
	}
	for _, tc := range tests {
		t.Run(tc.v, func(t *testing.T) {
			if got := isDevVersion(tc.v); got != tc.want {
				t.Errorf("isDevVersion(%q) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

// TestDefaultCacheDir_HonorsEnv pins the env-var override at the head
// of the resolution chain.
func TestDefaultCacheDir_HonorsEnv(t *testing.T) {
	t.Setenv(EnvCacheDir, "/tmp/deadzone-test-cache")
	if got := DefaultCacheDir(); got != "/tmp/deadzone-test-cache" {
		t.Errorf("DefaultCacheDir = %q, want /tmp/deadzone-test-cache", got)
	}
}

// TestBootstrap_Matrix is the table-driven view of the
// cache × offline × dev-version axis. The fake server registers two
// releases (v1.0.0, v1.1.0); v1.1.0 is last-added so
// latestTag()=="v1.1.0" — the dev fallback resolves to it. Cache
// states: "" = empty cache, "v1.0.0" = matching tag, "v0.9.0" = stale
// tag (no such release on the server, so a fetch path keyed on
// appVersion still works for the binary-upgrade case).
//
// Each subtest's name maps to a specific branch in bootstrap.go:
//
//	hit-online-release    fast path: cached tag matches AppVersion
//	hit-offline-release   fast path short-circuits before the offline guard
//	miss-online-release   fetchReleaseByTag + fetchAndInstall (first fetch)
//	miss-offline-release  offline guard, no-cache branch
//	hit-online-dev        fast path via isDev clause (any cached tag)
//	miss-online-dev       isDev → fetchLatestRelease + install
//	stale-online-release  fast path miss → fetchReleaseByTag (binary upgrade refresh)
//	stale-offline-release offline guard, mismatched-cache branch (must fail loud)
//
// Note on "stale-offline-release": the issue body suggests this case
// should "serve stale", but the production contract (and the existing
// TestBootstrap_OfflineMismatchedCacheFails) deliberately fails loud —
// serving a stale DB across binary versions risks schema/embedder
// drift. The matrix asserts the actual contract.
func TestBootstrap_Matrix(t *testing.T) {
	cases := []struct {
		name string

		appVersion string // "v1.0.0" | "v1.1.0" | "dev"
		cachedTag  string // "" = no cache; otherwise sidecar value
		offline    bool
		// skipProbe pins the matrix to the legacy "zero-network on tag
		// match" contract regardless of the #197 probe default. Probe
		// behaviour is exercised separately in TestBootstrap_AutoUpdate_*.
		skipProbe bool

		wantErrSubstr string // "" = expect success
		wantBody      string // expected installed DB body when success
		wantAPIHits   int32  // expected GitHub API endpoint hits
		wantDBHits    int32  // expected db-asset download hits
		wantUpgraded  bool   // bootstrap's "upgraded" return value
	}{
		{
			name:        "hit-online-release",
			appVersion:  "v1.0.0",
			cachedTag:   "v1.0.0",
			skipProbe:   true,
			wantBody:    "cached-bytes",
			wantAPIHits: 0,
			wantDBHits:  0,
		},
		{
			name:        "hit-offline-release",
			appVersion:  "v1.0.0",
			cachedTag:   "v1.0.0",
			offline:     true,
			wantBody:    "cached-bytes",
			wantAPIHits: 0,
			wantDBHits:  0,
		},
		{
			name:        "miss-online-release",
			appVersion:  "v1.0.0",
			cachedTag:   "",
			wantBody:    "v1.0.0-content",
			wantAPIHits: 1,
			wantDBHits:  1,
		},
		{
			name:          "miss-offline-release",
			appVersion:    "v1.0.0",
			cachedTag:     "",
			offline:       true,
			wantErrSubstr: "no cached",
		},
		{
			name:        "hit-online-dev",
			appVersion:  "dev",
			cachedTag:   "v0.9.0",
			wantBody:    "cached-bytes",
			wantAPIHits: 0,
			wantDBHits:  0,
		},
		{
			name:        "miss-online-dev",
			appVersion:  "dev",
			cachedTag:   "",
			wantBody:    "v1.1.0-content",
			wantAPIHits: 1,
			wantDBHits:  1,
		},
		{
			name:         "stale-online-release",
			appVersion:   "v1.0.0",
			cachedTag:    "v0.9.0",
			wantBody:     "v1.0.0-content",
			wantAPIHits:  1,
			wantDBHits:   1,
			wantUpgraded: true,
		},
		{
			name:          "stale-offline-release",
			appVersion:    "v1.0.0",
			cachedTag:     "v0.9.0",
			offline:       true,
			wantErrSubstr: EnvOffline,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearBootstrapEnv(t)
			if tc.offline {
				t.Setenv(EnvOffline, "1")
			}

			// Fresh fixture per case so apiCalls / dbCalls counters
			// are isolated. Releases are added in tag order so
			// latestTag()==v1.1.0 (the dev fallback target).
			rel100 := newFakeRelease(t, "v1.0.0", "v1.0.0-content")
			rel110 := newFakeRelease(t, "v1.1.0", "v1.1.0-content")
			fix := newFixtureServer(t, rel100, rel110)
			withAPIBase(t, fix.srv.URL)

			cacheDir := t.TempDir()
			if tc.cachedTag != "" {
				seedCache(t, cacheDir, "cached-bytes", tc.cachedTag)
			}

			path, upgraded, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
				CacheDir:            cacheDir,
				AppVersion:          tc.appVersion,
				SkipAutoUpdateProbe: tc.skipProbe,
			})

			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrSubstr)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Errorf("error %q missing substring %q", err, tc.wantErrSubstr)
				}
				if got := fix.apiCalls.Load(); got != tc.wantAPIHits {
					t.Errorf("apiCalls = %d, want %d", got, tc.wantAPIHits)
				}
				if got := fix.dbCalls.Load(); got != tc.wantDBHits {
					t.Errorf("dbCalls = %d, want %d", got, tc.wantDBHits)
				}
				return
			}

			if err != nil {
				t.Fatalf("Bootstrap: %v", err)
			}
			if path != filepath.Join(cacheDir, dbAssetName) {
				t.Errorf("path = %q, want %q", path, filepath.Join(cacheDir, dbAssetName))
			}
			if upgraded != tc.wantUpgraded {
				t.Errorf("upgraded = %v, want %v", upgraded, tc.wantUpgraded)
			}
			if tc.wantBody != "" {
				body, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatalf("read installed db: %v", readErr)
				}
				if string(body) != tc.wantBody {
					t.Errorf("body = %q, want %q", body, tc.wantBody)
				}
			}
			if got := fix.apiCalls.Load(); got != tc.wantAPIHits {
				t.Errorf("apiCalls = %d, want %d", got, tc.wantAPIHits)
			}
			if got := fix.dbCalls.Load(); got != tc.wantDBHits {
				t.Errorf("dbCalls = %d, want %d", got, tc.wantDBHits)
			}
		})
	}
}
