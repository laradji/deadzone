package ort

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// TestBootstrap exercises the full download → verify → extract → cache
// pipeline against an httptest-served fake release. The pinned SHA256
// map is swapped at test start so the archive we generate in-memory
// matches the "pinned" digest Bootstrap checks against — this proves
// the verification path runs and accepts only the right bytes, without
// depending on the live GitHub release (which would break CI whenever
// GitHub is slow or the caller is air-gapped).
func TestBootstrap(t *testing.T) {
	rel, ok := pinnedReleases[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		t.Skipf("no pinned release for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	archive, digest := buildFakeArchive(t, rel)

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/v"+Version+"/"+strings.ReplaceAll(rel.Archive, "{version}", Version)) {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	withOverrides(t, srv.URL, rel.Archive, digest, rel.LibName, rel.archiveKind)

	cacheDir := t.TempDir()

	// Ensure neither env var leaks in from the dev machine. Both
	// would short-circuit the very code path we're trying to test.
	t.Setenv(EnvLibPath, "")
	t.Setenv(EnvCacheDir, "")

	// First call: must download.
	got, err := Bootstrap(cacheDir)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	libPath := filepath.Join(got, rel.LibName)
	if _, err := os.Stat(libPath); err != nil {
		t.Fatalf("expected library at %s: %v", libPath, err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("first Bootstrap issued %d HTTP requests, want 1", n)
	}

	// Second call: must reuse the on-disk cache and NOT hit the
	// network. This is the acceptance criterion from #73 ("Second
	// run uses cached library").
	got2, err := Bootstrap(cacheDir)
	if err != nil {
		t.Fatalf("Bootstrap (cached): %v", err)
	}
	if got2 != got {
		t.Errorf("cached Bootstrap returned %q, want %q", got2, got)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("cached Bootstrap issued %d HTTP requests, want 1 (no new download)", n)
	}
}

// TestBootstrap_SHAMismatch pins the "SHA256 mismatch → clear error"
// acceptance criterion from #73. A corrupted download, a mirror
// serving the wrong bytes, or a pinned-version bump that forgot to
// refresh the hash all surface through this path; returning OK on a
// bad hash would silently load arbitrary bytes into CGO.
func TestBootstrap_SHAMismatch(t *testing.T) {
	rel, ok := pinnedReleases[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		t.Skipf("no pinned release for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	archive, _ := buildFakeArchive(t, rel)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	// Register a hash that does NOT match the archive bytes.
	wrong := "0000000000000000000000000000000000000000000000000000000000000000"
	withOverrides(t, srv.URL, rel.Archive, wrong, rel.LibName, rel.archiveKind)

	t.Setenv(EnvLibPath, "")
	t.Setenv(EnvCacheDir, "")
	_, err := Bootstrap(t.TempDir())
	if err == nil {
		t.Fatal("Bootstrap on corrupt archive returned nil, want sha256 mismatch error")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("error does not mention sha256 mismatch: %v", err)
	}
}

// TestBootstrap_EnvShortcut pins the air-gap acceptance criterion:
// DEADZONE_ORT_LIB_PATH must bypass the download entirely and return
// the operator-provided directory verbatim. Any HTTP request at all
// would break `--no-network` installs.
func TestBootstrap_EnvShortcut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected HTTP request to %s when %s is set", r.URL, EnvLibPath)
		http.Error(w, "should not be called", http.StatusTeapot)
	}))
	defer srv.Close()

	old := releaseBaseURL
	releaseBaseURL = srv.URL
	t.Cleanup(func() { releaseBaseURL = old })

	preset := t.TempDir()
	t.Setenv(EnvLibPath, preset)

	got, err := Bootstrap(t.TempDir())
	if err != nil {
		t.Fatalf("Bootstrap with %s set: %v", EnvLibPath, err)
	}
	if got != preset {
		t.Errorf("Bootstrap returned %q, want preset %q", got, preset)
	}
}

// TestBootstrap_UnsupportedPlatform verifies that a GOOS/GOARCH with
// no pinned release surfaces ErrUnsupportedPlatform rather than a
// generic 404 from GitHub or a silent stall. Without this, Windows
// arm64 users (today's unlisted platform) would get a confusing
// "HTTP 404" instead of the "set DEADZONE_ORT_LIB_PATH" guidance.
func TestBootstrap_UnsupportedPlatform(t *testing.T) {
	old := pinnedReleases
	pinnedReleases = map[string]release{}
	t.Cleanup(func() { pinnedReleases = old })

	t.Setenv(EnvLibPath, "")
	t.Setenv(EnvCacheDir, "")

	_, err := Bootstrap(t.TempDir())
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("Bootstrap on unsupported platform: got %v, want ErrUnsupportedPlatform", err)
	}
}

// TestDefaultCacheDir_EnvOverride guards the CI convention: pointing
// DEADZONE_ORT_CACHE at a workspace-local path so actions/cache can
// persist the download across runs. Drift here silently re-downloads
// the 30 MB archive on every CI job.
func TestDefaultCacheDir_EnvOverride(t *testing.T) {
	t.Setenv(EnvCacheDir, "/tmp/custom-ort-cache")
	if got := DefaultCacheDir(); got != "/tmp/custom-ort-cache" {
		t.Errorf("DefaultCacheDir() = %q, want /tmp/custom-ort-cache", got)
	}
}

// withOverrides installs a single release mapping for the current
// platform plus a test-owned releaseBaseURL, restoring both when the
// test completes. Centralising it keeps the per-test boilerplate
// short and prevents one test from leaking overrides into another.
func withOverrides(t *testing.T, baseURL, archiveTmpl, sha256Hex, libName, kind string) {
	t.Helper()
	oldURL := releaseBaseURL
	oldMap := pinnedReleases
	releaseBaseURL = baseURL
	pinnedReleases = map[string]release{
		runtime.GOOS + "/" + runtime.GOARCH: {
			Archive:     archiveTmpl,
			SHA256:      sha256Hex,
			LibName:     libName,
			archiveKind: kind,
		},
	}
	t.Cleanup(func() {
		releaseBaseURL = oldURL
		pinnedReleases = oldMap
	})
}

// buildFakeArchive produces a minimal release archive — either a
// gzip-wrapped tarball or a zip — that mirrors the upstream layout
// closely enough to exercise the extractor. The returned digest is
// the sha256 of the archive bytes, which the caller pins into
// pinnedReleases so Bootstrap accepts the archive as authentic.
func buildFakeArchive(t *testing.T, rel release) ([]byte, string) {
	t.Helper()
	dummy := []byte("not a real shared library — test fixture")
	var buf bytes.Buffer
	switch rel.archiveKind {
	case "tgz":
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		// lib/<libName> as a regular file, plus a sibling symlink
		// to exercise the TypeSymlink branch. The "top-level dir"
		// mirrors the upstream layout (onnxruntime-osx-arm64-<v>/),
		// even though extractor only looks at path.Base.
		top := "onnxruntime-fixture/"
		write := func(name string, body []byte) {
			h := &tar.Header{
				Name:     top + "lib/" + name,
				Mode:     0o644,
				Size:     int64(len(body)),
				Typeflag: tar.TypeReg,
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatalf("tar header: %v", err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatalf("tar write: %v", err)
			}
		}
		// Versioned sibling first (holds the real bytes), then the
		// symlink alias that most callers open by name.
		var versioned string
		if strings.HasSuffix(rel.LibName, ".dylib") {
			versioned = strings.TrimSuffix(rel.LibName, ".dylib") + "." + Version + ".dylib"
		} else {
			versioned = rel.LibName + "." + Version
		}
		write(versioned, dummy)
		if err := tw.WriteHeader(&tar.Header{
			Name:     top + "lib/" + rel.LibName,
			Mode:     0o777,
			Typeflag: tar.TypeSymlink,
			Linkname: versioned,
		}); err != nil {
			t.Fatalf("tar symlink header: %v", err)
		}
		// Noise entries that the extractor must ignore — a
		// non-library file next to the dylib, and a file outside
		// lib/ entirely. If the filter regresses and grabs these,
		// the cache fills up with garbage.
		if err := tw.WriteHeader(&tar.Header{
			Name: top + "LICENSE", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar noise header: %v", err)
		}
		if _, err := tw.Write([]byte("MIT\n")); err != nil {
			t.Fatalf("tar noise write: %v", err)
		}
		if err := tw.Close(); err != nil {
			t.Fatalf("tar close: %v", err)
		}
		if err := gz.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
	case "zip":
		zw := zip.NewWriter(&buf)
		w, err := zw.Create("onnxruntime-fixture/lib/" + rel.LibName)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write(dummy); err != nil {
			t.Fatalf("zip write: %v", err)
		}
		nw, err := zw.Create("onnxruntime-fixture/LICENSE")
		if err != nil {
			t.Fatalf("zip noise create: %v", err)
		}
		if _, err := nw.Write([]byte("MIT\n")); err != nil {
			t.Fatalf("zip noise write: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("zip close: %v", err)
		}
	default:
		t.Fatalf("unknown archive kind %q", rel.archiveKind)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}
