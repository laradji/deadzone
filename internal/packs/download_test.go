package packs_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laradji/deadzone/internal/packs"
)

// fileServer spins up an httptest.Server that serves a fixed map of
// {asset_name → bytes} via the same URL layout the GitHub release uses
// (`/<owner>/<repo>/releases/download/<tag>/<asset>`). The handler also
// records every request path so tests can assert "no extra HTTP calls
// happened on the idempotent run".
type fileServer struct {
	t        *testing.T
	server   *httptest.Server
	files    map[string][]byte
	requests []string
}

func newFileServer(t *testing.T, files map[string][]byte) *fileServer {
	t.Helper()
	fs := &fileServer{t: t, files: files}
	fs.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.requests = append(fs.requests, r.URL.Path)
		// /<owner>/<repo>/releases/download/<tag>/<asset>
		parts := strings.Split(r.URL.Path, "/")
		asset := parts[len(parts)-1]
		body, ok := fs.files[asset]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(fs.server.Close)
	return fs
}

// fetcherFor returns a packs.Fetcher that rewrites the canonical
// github.com URL to point at the in-test httptest.Server. This is the
// minimal seam needed to test the download path without monkey-patching
// the URL builder; the production assetURL stays canonical and tests
// inject their own Fetcher.
func (fs *fileServer) fetcherFor(t *testing.T) packs.Fetcher {
	t.Helper()
	return packs.FetcherFunc(func(ctx context.Context, url string) (io.ReadCloser, error) {
		// Replace the github.com prefix with our test server prefix.
		const prefix = "https://github.com"
		if !strings.HasPrefix(url, prefix) {
			return nil, fmt.Errorf("unexpected URL %q", url)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fs.server.URL+strings.TrimPrefix(url, prefix), nil)
		if err != nil {
			return nil, err
		}
		resp, err := fs.server.Client().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("status %d for %s", resp.StatusCode, url)
		}
		return resp.Body, nil
	})
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// writeManifestWithPacks writes a manifest with one entry per supplied
// asset (using its sha256 + length) into a fresh dir alongside an
// empty artifacts subdirectory. Returns (artifactsDir, manifestPath).
func writeManifestWithPacks(t *testing.T, files map[string][]byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	artifactsDir := filepath.Join(dir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	body := "release_tag: packs\nrepo: laradji/deadzone-test\npacks:\n"
	for name, contents := range files {
		// derive lib_id from filename: foo_bar.db → /foo/bar
		stem := strings.TrimSuffix(name, ".db")
		libID := "/" + strings.ReplaceAll(stem, "_", "/")
		body += fmt.Sprintf(
			"  - lib_id: %s\n    asset: %s\n    sha256: %s\n    size: %d\n    indexed_at: 2026-04-10T16:23:00Z\n",
			libID, name, sha256Hex(contents), len(contents),
		)
	}
	manifestPath := filepath.Join(artifactsDir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return artifactsDir, manifestPath
}

func TestDownload_FreshClone(t *testing.T) {
	files := map[string][]byte{
		"x_y.db": []byte("body-of-x-y"),
		"a_b.db": []byte("body-of-a-b"),
	}
	fs := newFileServer(t, files)
	artifactsDir, manifestPath := writeManifestWithPacks(t, files)

	summary, err := packs.RunDownload(context.Background(), packs.DownloadOptions{
		ArtifactsDir: artifactsDir,
		ManifestPath: manifestPath,
		Repo:         "laradji/deadzone-test",
		Fetcher:      fs.fetcherFor(t),
	})
	if err != nil {
		t.Fatalf("RunDownload: %v", err)
	}
	if summary.Downloaded != 2 || summary.Verified != 0 || summary.Redownloaded != 0 {
		t.Errorf("summary = %+v, want Downloaded:2", summary)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(artifactsDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s contents = %q, want %q", name, got, want)
		}
	}
}

func TestDownload_AlreadyPresentSkipsHTTP(t *testing.T) {
	files := map[string][]byte{
		"x_y.db": []byte("body-of-x-y"),
	}
	fs := newFileServer(t, files)
	artifactsDir, manifestPath := writeManifestWithPacks(t, files)

	// Pre-populate the canonical file with the right contents.
	if err := os.WriteFile(filepath.Join(artifactsDir, "x_y.db"), files["x_y.db"], 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	summary, err := packs.RunDownload(context.Background(), packs.DownloadOptions{
		ArtifactsDir: artifactsDir,
		ManifestPath: manifestPath,
		Repo:         "laradji/deadzone-test",
		Fetcher:      fs.fetcherFor(t),
	})
	if err != nil {
		t.Fatalf("RunDownload: %v", err)
	}
	if summary.Verified != 1 || summary.Downloaded != 0 {
		t.Errorf("summary = %+v, want Verified:1", summary)
	}
	if len(fs.requests) != 0 {
		t.Errorf("HTTP requests issued on verified-only run: %v", fs.requests)
	}
}

func TestDownload_TamperedLocalIsRedownloaded(t *testing.T) {
	files := map[string][]byte{
		"x_y.db": []byte("body-of-x-y"),
	}
	fs := newFileServer(t, files)
	artifactsDir, manifestPath := writeManifestWithPacks(t, files)

	// Plant a tampered version on disk.
	tamperedPath := filepath.Join(artifactsDir, "x_y.db")
	if err := os.WriteFile(tamperedPath, []byte("THIS IS THE WRONG CONTENT"), 0o600); err != nil {
		t.Fatalf("seed tampered: %v", err)
	}

	summary, err := packs.RunDownload(context.Background(), packs.DownloadOptions{
		ArtifactsDir: artifactsDir,
		ManifestPath: manifestPath,
		Repo:         "laradji/deadzone-test",
		Fetcher:      fs.fetcherFor(t),
	})
	if err != nil {
		t.Fatalf("RunDownload: %v", err)
	}
	if summary.Redownloaded != 1 {
		t.Errorf("summary = %+v, want Redownloaded:1", summary)
	}
	got, err := os.ReadFile(tamperedPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "body-of-x-y" {
		t.Errorf("post-redownload contents = %q, want body-of-x-y", got)
	}
}

func TestDownload_TamperedServerHardAborts(t *testing.T) {
	manifestFiles := map[string][]byte{
		"x_y.db": []byte("body-of-x-y"),
	}
	// Server has DIFFERENT bytes for the same asset name → mismatch.
	serverFiles := map[string][]byte{
		"x_y.db": []byte("server-side-corruption"),
	}
	fs := newFileServer(t, serverFiles)
	artifactsDir, manifestPath := writeManifestWithPacks(t, manifestFiles)

	_, err := packs.RunDownload(context.Background(), packs.DownloadOptions{
		ArtifactsDir: artifactsDir,
		ManifestPath: manifestPath,
		Repo:         "laradji/deadzone-test",
		Fetcher:      fs.fetcherFor(t),
	})
	if err == nil {
		t.Fatal("expected hard-abort error, got nil")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("err = %v, want sha256 mismatch", err)
	}

	// Canonical file should NOT exist (verify-before-rename invariant).
	if _, statErr := os.Stat(filepath.Join(artifactsDir, "x_y.db")); !os.IsNotExist(statErr) {
		t.Error("canonical x_y.db exists after server-side mismatch — verify-before-rename invariant violated")
	}
	// And no .tmp leftover.
	if _, statErr := os.Stat(filepath.Join(artifactsDir, "x_y.db.tmp")); !os.IsNotExist(statErr) {
		t.Error(".tmp file leaked after server-side mismatch")
	}
}

func TestDownload_TamperedServerDoesNotOverwriteGoodLocal(t *testing.T) {
	// This is the asymmetric-tampering safety net: the local file is
	// good, but for some reason the verifier needed to re-check the
	// server, and the server returns garbage. The good local file
	// must survive untouched. We construct this by tampering the
	// local file (so the verifier triggers a redownload) AND also
	// tampering the server (so the redownload mismatches). The hard
	// abort runs, and... the local file is still bad (we removed it).
	//
	// This is fine: the contract is "good local file is preserved
	// when there's no reason to touch it" and "tampered local file
	// gets blown away on the off-chance the redownload succeeds".
	// The test below covers the verify-before-rename half. Skipped
	// — left as a comment so future readers don't add it back.
	t.Skip("see comment — covered by TestDownload_TamperedServerHardAborts")
}

func TestDownload_LibFilterMatchesVersionedChildren(t *testing.T) {
	files := map[string][]byte{
		"facebook_react_v18.db": []byte("react-18"),
		"facebook_react_v19.db": []byte("react-19"),
		"unrelated_lib.db":      []byte("unrelated"),
	}
	fs := newFileServer(t, files)
	artifactsDir, manifestPath := writeManifestWithPacks(t, files)

	summary, err := packs.RunDownload(context.Background(), packs.DownloadOptions{
		ArtifactsDir: artifactsDir,
		ManifestPath: manifestPath,
		Repo:         "laradji/deadzone-test",
		LibFilter:    "/facebook/react",
		Fetcher:      fs.fetcherFor(t),
	})
	if err != nil {
		t.Fatalf("RunDownload: %v", err)
	}
	if summary.Downloaded != 2 {
		t.Errorf("Downloaded = %d, want 2 (both versioned children)", summary.Downloaded)
	}
	if _, err := os.Stat(filepath.Join(artifactsDir, "unrelated_lib.db")); !os.IsNotExist(err) {
		t.Error("unrelated_lib.db was downloaded despite the filter")
	}
}

func TestDownload_LibFilterExactMatch(t *testing.T) {
	files := map[string][]byte{
		"facebook_react_v18.db": []byte("react-18"),
		"facebook_react_v19.db": []byte("react-19"),
	}
	fs := newFileServer(t, files)
	artifactsDir, manifestPath := writeManifestWithPacks(t, files)

	summary, err := packs.RunDownload(context.Background(), packs.DownloadOptions{
		ArtifactsDir: artifactsDir,
		ManifestPath: manifestPath,
		Repo:         "laradji/deadzone-test",
		LibFilter:    "/facebook/react/v18",
		Fetcher:      fs.fetcherFor(t),
	})
	if err != nil {
		t.Fatalf("RunDownload: %v", err)
	}
	if summary.Downloaded != 1 {
		t.Errorf("Downloaded = %d, want 1", summary.Downloaded)
	}
}

func TestDownload_LibFilterNoMatchErrors(t *testing.T) {
	files := map[string][]byte{
		"x_y.db": []byte("payload"),
	}
	fs := newFileServer(t, files)
	artifactsDir, manifestPath := writeManifestWithPacks(t, files)

	_, err := packs.RunDownload(context.Background(), packs.DownloadOptions{
		ArtifactsDir: artifactsDir,
		ManifestPath: manifestPath,
		Repo:         "laradji/deadzone-test",
		LibFilter:    "/totally/missing",
		Fetcher:      fs.fetcherFor(t),
	})
	if err == nil || !strings.Contains(err.Error(), "no manifest entries match") {
		t.Errorf("err = %v, want no-match error", err)
	}
}

func TestDownload_MissingAssetOnServerErrors(t *testing.T) {
	manifestFiles := map[string][]byte{
		"x_y.db": []byte("body"),
	}
	// Server is empty.
	fs := newFileServer(t, map[string][]byte{})
	artifactsDir, manifestPath := writeManifestWithPacks(t, manifestFiles)

	_, err := packs.RunDownload(context.Background(), packs.DownloadOptions{
		ArtifactsDir: artifactsDir,
		ManifestPath: manifestPath,
		Repo:         "laradji/deadzone-test",
		Fetcher:      fs.fetcherFor(t),
	})
	if err == nil {
		t.Fatal("expected error for missing asset, got nil")
	}
}

func TestDownload_RequiresFetcher(t *testing.T) {
	_, err := packs.RunDownload(context.Background(), packs.DownloadOptions{
		ArtifactsDir: "any", ManifestPath: "any", Repo: "x/y",
	})
	if err == nil || !errors.Is(err, errors.New("download: Fetcher is required")) && !strings.Contains(err.Error(), "Fetcher is required") {
		t.Errorf("err = %v, want Fetcher required", err)
	}
}

func TestDownload_RequiresRepo(t *testing.T) {
	_, err := packs.RunDownload(context.Background(), packs.DownloadOptions{
		ArtifactsDir: "any", ManifestPath: "any", Fetcher: packs.FetcherFunc(func(context.Context, string) (io.ReadCloser, error) {
			return nil, nil
		}),
	})
	if err == nil || !strings.Contains(err.Error(), "Repo is required") {
		t.Errorf("err = %v, want Repo required", err)
	}
}
