package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// dbBootstrapEnv lists the env vars Bootstrap reads. Every test
// neutralizes them via t.Setenv so a developer's local
// DEADZONE_DB_CACHE / DEADZONE_DB_OFFLINE doesn't bleed in.
var dbBootstrapEnv = []string{EnvCacheDir, EnvOffline, EnvNoAutoUpgrade}

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

// fixtureServer wires together the API endpoint and the asset endpoints
// behind a single httptest server so the fake "browser_download_url"
// values can point back at the same host. requests counts every hit so
// tests can assert "zero asset downloads" precisely.
type fixtureServer struct {
	srv         *httptest.Server
	rel         fakeRelease
	apiCalls    atomic.Int32
	dbCalls     atomic.Int32
	sha256Calls atomic.Int32
	failAPI     atomic.Bool // when true, /repos/.../latest returns 500
	corruptDB   atomic.Bool // when true, dbBody comes back with a single byte flipped
	corruptSHA  atomic.Bool // when true, sha256 comes back as zeroes (mismatch)
	missingDB   atomic.Bool // when true, the JSON omits the deadzone.db asset
}

func newFixtureServer(t *testing.T, rel fakeRelease) *fixtureServer {
	t.Helper()
	fs := &fixtureServer{rel: rel}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/laradji/deadzone/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fs.apiCalls.Add(1)
		if fs.failAPI.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		assets := []map[string]string{
			{"name": sha256AssetName, "browser_download_url": fs.srv.URL + "/dl/" + sha256AssetName},
		}
		if !fs.missingDB.Load() {
			assets = append(assets, map[string]string{
				"name":                 dbAssetName,
				"browser_download_url": fs.srv.URL + "/dl/" + dbAssetName,
			})
		}
		body := map[string]any{
			"tag_name": fs.rel.tag,
			"assets":   assets,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/dl/"+dbAssetName, func(w http.ResponseWriter, r *http.Request) {
		fs.dbCalls.Add(1)
		if fs.corruptDB.Load() {
			b := append([]byte(nil), fs.rel.dbBody...)
			if len(b) > 0 {
				b[0] ^= 0xff
			}
			_, _ = w.Write(b)
			return
		}
		_, _ = w.Write(fs.rel.dbBody)
	})
	mux.HandleFunc("/dl/"+sha256AssetName, func(w http.ResponseWriter, r *http.Request) {
		fs.sha256Calls.Add(1)
		hash := fs.rel.dbHash
		if fs.corruptSHA.Load() {
			hash = strings.Repeat("0", 64)
		}
		fmt.Fprintf(w, "%s  %s\n", hash, dbAssetName)
	})
	fs.srv = httptest.NewServer(mux)
	t.Cleanup(fs.srv.Close)
	return fs
}

// withAPIBase swaps the package-level releasesAPIBase for the duration
// of the test. The defer restores the production value so a failing
// test can't poison sibling tests in the same binary.
func withAPIBase(t *testing.T, base string) {
	t.Helper()
	orig := releasesAPIBase
	releasesAPIBase = base
	t.Cleanup(func() { releasesAPIBase = orig })
}

// TestBootstrap_FreshCache covers the first-fetch happy path: empty
// cache + reachable network → DB lands in cache, sha256 verifies, tag
// sidecar is written with the release tag, upgraded=false because no
// previous file existed.
func TestBootstrap_FreshCache(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.0.0", "fake-db-content-v1")
	fix := newFixtureServer(t, rel)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	path, upgraded, err := BootstrapWithOptions(context.Background(), BootstrapOptions{CacheDir: cacheDir})
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
	tag, err := os.ReadFile(filepath.Join(cacheDir, tagSidecarName))
	if err != nil {
		t.Fatalf("read tag sidecar: %v", err)
	}
	if strings.TrimSpace(string(tag)) != rel.tag {
		t.Errorf("tag sidecar = %q, want %q", tag, rel.tag)
	}
	if n := fix.dbCalls.Load(); n != 1 {
		t.Errorf("db asset hit %d times, want 1", n)
	}
}

// TestBootstrap_OfflineNoCache covers DEADZONE_DB_OFFLINE=1 with
// nothing cached: must error and must not leave a partial file. The
// error message must mention the env var so an offline operator knows
// the escape hatch they tripped.
func TestBootstrap_OfflineNoCache(t *testing.T) {
	clearBootstrapEnv(t)
	t.Setenv(EnvOffline, "1")

	cacheDir := t.TempDir()
	path, upgraded, err := BootstrapWithOptions(context.Background(), BootstrapOptions{CacheDir: cacheDir})
	if err == nil {
		t.Fatalf("Bootstrap returned no error; got path=%q upgraded=%v", path, upgraded)
	}
	if !strings.Contains(err.Error(), EnvOffline) {
		t.Errorf("error %q does not mention %s", err, EnvOffline)
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, dbAssetName)); !os.IsNotExist(statErr) {
		t.Errorf("expected no db file, stat err = %v", statErr)
	}
}

// TestBootstrap_SHAMismatch covers the corrupted-mirror case: API +
// asset reachable, but the sha256 of the downloaded bytes doesn't
// match the sidecar. Must error AND must leave no partial file in the
// cache (tempfile cleanup).
func TestBootstrap_SHAMismatch(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.0.0", "fake-db-content")
	fix := newFixtureServer(t, rel)
	fix.corruptSHA.Store(true)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	if _, _, err := BootstrapWithOptions(context.Background(), BootstrapOptions{CacheDir: cacheDir}); err == nil {
		t.Fatalf("Bootstrap returned no error on sha256 mismatch")
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, dbAssetName)); !os.IsNotExist(statErr) {
		t.Errorf("partial file left in cache after sha mismatch: stat err = %v", statErr)
	}
	// Tempfile pattern is dbAssetName+".tmp-*"; make sure none lingered.
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), dbAssetName+".tmp-") {
			t.Errorf("tempfile %q not cleaned up", e.Name())
		}
	}
}

// TestBootstrap_TagMatchSkipsAssetDownload covers the warm-cache path:
// the cached tag matches the latest release, so Bootstrap must skip the
// asset download entirely. The API call still happens (that's how we
// learn the tag matches) — assertion is that dbCalls == 0.
func TestBootstrap_TagMatchSkipsAssetDownload(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.0.0", "fake-db-content")
	fix := newFixtureServer(t, rel)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, dbAssetName), []byte("cached-bytes"), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, tagSidecarName), []byte(rel.tag+"\n"), 0o644); err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	path, upgraded, err := BootstrapWithOptions(context.Background(), BootstrapOptions{CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if upgraded {
		t.Errorf("upgraded=true on tag match, want false")
	}
	if path != filepath.Join(cacheDir, dbAssetName) {
		t.Errorf("path = %q, want cached path", path)
	}
	if n := fix.dbCalls.Load(); n != 0 {
		t.Errorf("db asset hit %d times on tag match, want 0", n)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "cached-bytes" {
		t.Errorf("cached body was overwritten on tag match: %q", got)
	}
}

// TestBootstrap_StaleUpgrade covers path 3: cached tag != latest →
// fetch new, atomic swap, upgraded=true, tag sidecar updated.
func TestBootstrap_StaleUpgrade(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.1.0", "new-content")
	fix := newFixtureServer(t, rel)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, dbAssetName), []byte("old-bytes"), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, tagSidecarName), []byte("v1.0.0\n"), 0o644); err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	path, upgraded, err := BootstrapWithOptions(context.Background(), BootstrapOptions{CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !upgraded {
		t.Errorf("upgraded=false on stale upgrade, want true")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(rel.dbBody) {
		t.Errorf("after upgrade body = %q, want %q", got, rel.dbBody)
	}
	tag, _ := os.ReadFile(filepath.Join(cacheDir, tagSidecarName))
	if strings.TrimSpace(string(tag)) != rel.tag {
		t.Errorf("tag sidecar = %q, want %q", tag, rel.tag)
	}
}

// TestBootstrap_StaleNetworkErrorServesCache covers the failure-mode
// row: stale cache + the API call fails → serve the stale cache, log a
// WARN, return upgraded=false with no error so server startup doesn't
// abort on a transient network blip.
func TestBootstrap_StaleNetworkErrorServesCache(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.1.0", "new-content")
	fix := newFixtureServer(t, rel)
	fix.failAPI.Store(true)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, dbAssetName), []byte("old-bytes"), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, tagSidecarName), []byte("v1.0.0\n"), 0o644); err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	path, upgraded, err := BootstrapWithOptions(context.Background(), BootstrapOptions{CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("Bootstrap returned err=%v on stale-cache+network-error, want nil (graceful degrade)", err)
	}
	if upgraded {
		t.Errorf("upgraded=true on network error, want false")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "old-bytes" {
		t.Errorf("cache was overwritten on network error: %q", got)
	}
}

// TestBootstrap_NoAutoUpgradeSkipsNetwork covers the env-var escape
// hatch: DEADZONE_DB_NO_AUTO_UPGRADE=1 with a cached file → skip the
// API call entirely and serve the cache.
func TestBootstrap_NoAutoUpgradeSkipsNetwork(t *testing.T) {
	clearBootstrapEnv(t)
	t.Setenv(EnvNoAutoUpgrade, "1")
	rel := newFakeRelease(t, "v1.1.0", "new-content")
	fix := newFixtureServer(t, rel)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, dbAssetName), []byte("cached-bytes"), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, tagSidecarName), []byte("v0.1.0\n"), 0o644); err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	path, upgraded, err := BootstrapWithOptions(context.Background(), BootstrapOptions{CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if upgraded {
		t.Errorf("upgraded=true with NO_AUTO_UPGRADE, want false")
	}
	if n := fix.apiCalls.Load(); n != 0 {
		t.Errorf("API hit %d times with NO_AUTO_UPGRADE, want 0", n)
	}
	if n := fix.dbCalls.Load(); n != 0 {
		t.Errorf("db asset hit %d times with NO_AUTO_UPGRADE, want 0", n)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "cached-bytes" {
		t.Errorf("cache body was modified: %q", got)
	}
}

// TestBootstrap_ForceRefetches covers fetch-db --force: cached tag
// matches latest, but Force=true must re-download anyway. The asset
// counter goes from 0 (the tag-match short-circuit) to 1.
func TestBootstrap_ForceRefetches(t *testing.T) {
	clearBootstrapEnv(t)
	rel := newFakeRelease(t, "v1.0.0", "fresh-content")
	fix := newFixtureServer(t, rel)
	withAPIBase(t, fix.srv.URL)

	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, dbAssetName), []byte("cached-bytes"), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, tagSidecarName), []byte(rel.tag+"\n"), 0o644); err != nil {
		t.Fatalf("seed tag: %v", err)
	}

	path, upgraded, err := BootstrapWithOptions(context.Background(), BootstrapOptions{
		CacheDir: cacheDir,
		Force:    true,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !upgraded {
		t.Errorf("upgraded=false with Force, want true (cache existed and was replaced)")
	}
	if n := fix.dbCalls.Load(); n != 1 {
		t.Errorf("db asset hit %d times with Force, want 1", n)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(rel.dbBody) {
		t.Errorf("after force body = %q, want %q", got, rel.dbBody)
	}
}

// TestDefaultCacheDir_HonorsEnv pins the env-var override at the head
// of the resolution chain. Platform-specific defaults are deliberately
// left untested at the unit level; covering them well requires
// per-OS test fixtures and the production behavior is exercised by
// integration runs.
func TestDefaultCacheDir_HonorsEnv(t *testing.T) {
	t.Setenv(EnvCacheDir, "/tmp/deadzone-test-cache")
	if got := DefaultCacheDir(); got != "/tmp/deadzone-test-cache" {
		t.Errorf("DefaultCacheDir = %q, want /tmp/deadzone-test-cache", got)
	}
}
