// Package ort bootstraps the libonnxruntime shared library that hugot's
// ORT backend dlopen's at session creation time.
//
// The library is fetched from the upstream Microsoft release archive,
// SHA256-verified against a pinned digest, extracted into a per-user
// cache, and re-used on every subsequent run. This removes the "install
// onnxruntime by hand, set LD_LIBRARY_PATH" step that otherwise gates
// every first-time `deadzone serve` on a fresh machine.
//
// The version is pinned deliberately (see Version). Bumping it is a
// three-line change — Version + the SHA256 map — and should be a
// deliberate step, not a passive "upgrade to latest" on every build.
//
// Air-gapped / corp-proxy escape hatch: set DEADZONE_ORT_LIB_PATH to the
// directory containing a pre-positioned libonnxruntime.{dylib,so,dll}
// and Bootstrap returns that path without touching the network.
package ort

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Version is the pinned onnxruntime release this binary expects. The
// SHA256 table in pinnedReleases must carry an entry for the same
// version. Bumping it is a deliberate step — re-measure on a
// representative corpus before merging, and refresh the hashes from
// `gh api repos/microsoft/onnxruntime/releases/tags/v<N>`.
const Version = "1.25.1"

// EnvLibPath names the env var that points at a directory containing a
// pre-positioned libonnxruntime shared library. When set, Bootstrap
// skips the download and returns the directory verbatim — the escape
// hatch for air-gapped machines and pinned-mirror corp environments.
const EnvLibPath = "DEADZONE_ORT_LIB_PATH"

// EnvCacheDir overrides the default cache root used by DefaultCacheDir.
// CI uses it to pin the cache to a workspace-local path that
// actions/cache can persist across runs.
const EnvCacheDir = "DEADZONE_ORT_CACHE"

// releaseBaseURL is the GitHub release download endpoint. Split out so
// tests can point Bootstrap at a local fixture server.
var releaseBaseURL = "https://github.com/microsoft/onnxruntime/releases/download"

// httpClient is used for the download. The explicit timeout is a last-
// resort safety net; the real timeout is the io.Copy stall budget
// enforced by the OS-level socket keepalive. 10 minutes is long enough
// for slow residential links fetching the ~8-30 MB tarball but short
// enough that a wedged CI runner fails loud instead of hanging the job.
var httpClient = &http.Client{Timeout: 10 * time.Minute}

// release describes one platform-specific artifact in the upstream
// onnxruntime release. Everything it carries is pinned — a bump to
// Version must refresh every SHA256.
type release struct {
	// Archive is the filename under
	// releaseBaseURL/v<Version>/<Archive>. Template fields are filled
	// via strings.Replace with {version} → Version.
	Archive string
	// SHA256 is the hex digest of the archive bytes, as published in
	// the GitHub release metadata (`.assets[].digest`).
	SHA256 string
	// LibName is the file basename that hugot's getDefaultLibraryPaths
	// expects inside the returned directory. Must match exactly or
	// options.WithOnnxLibraryPath rejects the directory.
	LibName string
	// archiveKind is how to unpack Archive — "tgz" for .tgz, "zip"
	// for .zip.
	archiveKind string
}

// pinnedReleases maps runtime.GOOS+"/"+runtime.GOARCH to the release
// that was validated against Version. Platforms that Microsoft no
// longer publishes (e.g. osx-x86_64 was dropped at 1.19) are absent
// — Bootstrap returns a clear error on those platforms pointing at
// EnvLibPath.
var pinnedReleases = map[string]release{
	"darwin/arm64": {
		Archive:     "onnxruntime-osx-arm64-{version}.tgz",
		SHA256:      "18987ec3187b5f29ba798109750f6135060560ad4e0a52678fcc753ee8fb3091",
		LibName:     "libonnxruntime.dylib",
		archiveKind: "tgz",
	},
	"linux/amd64": {
		Archive:     "onnxruntime-linux-x64-{version}.tgz",
		SHA256:      "eb566a49cfc49ef0642f809b69340b5bb656c7c4905ba873526d226f2c005816",
		LibName:     "libonnxruntime.so",
		archiveKind: "tgz",
	},
	"linux/arm64": {
		Archive:     "onnxruntime-linux-aarch64-{version}.tgz",
		SHA256:      "daa71b56b00c4ab34798a3d96ca41a32ece4d3e302dc2386d3cca83fd4491214",
		LibName:     "libonnxruntime.so",
		archiveKind: "tgz",
	},
	"windows/amd64": {
		Archive:     "onnxruntime-win-x64-{version}.zip",
		SHA256:      "33f2e8a63774811f99a5fc224cac32f4eed8c27643d46c6cc685319fa8f18019",
		LibName:     "onnxruntime.dll",
		archiveKind: "zip",
	},
}

// ErrUnsupportedPlatform is returned when the current GOOS/GOARCH has
// no entry in pinnedReleases. Callers can recover by pointing
// DEADZONE_ORT_LIB_PATH at a hand-installed copy of the library.
var ErrUnsupportedPlatform = errors.New("ort: no pinned onnxruntime build for this platform — set DEADZONE_ORT_LIB_PATH")

// Bootstrap returns the directory containing libonnxruntime for the
// current platform, downloading + extracting the pinned release into
// cacheDir on first call and re-using the on-disk copy on every
// subsequent call.
//
// Resolution order:
//
//  1. DEADZONE_ORT_LIB_PATH — if set, return verbatim. The directory
//     must already contain the right libonnxruntime file; Bootstrap
//     does not validate it (hugot's options.WithOnnxLibraryPath does,
//     and produces a better error message).
//  2. Cached extraction at cacheDir/v<Version>/. If the expected
//     library file exists there, return it.
//  3. Download + SHA256-verify + extract.
//
// cacheDir may be empty, in which case DefaultCacheDir() is used.
func Bootstrap(cacheDir string) (string, error) {
	if p := os.Getenv(EnvLibPath); p != "" {
		return p, nil
	}

	rel, ok := pinnedReleases[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("%w (GOOS=%s GOARCH=%s)", ErrUnsupportedPlatform, runtime.GOOS, runtime.GOARCH)
	}

	if cacheDir == "" {
		cacheDir = DefaultCacheDir()
	}

	// Versioned subdir so multiple deadzone binaries pinned to
	// different ORT versions can coexist in the same cache without
	// one overwriting the other's extracted library.
	versionDir := filepath.Join(cacheDir, "v"+Version)
	libFile := filepath.Join(versionDir, rel.LibName)

	if _, err := os.Stat(libFile); err == nil {
		return versionDir, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("ort: stat cached library: %w", err)
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("ort: create cache dir %q: %w", cacheDir, err)
	}

	// Extract into a sibling scratch dir and rename atomically. A
	// partial extraction left over from a crashed previous run would
	// otherwise masquerade as a populated cache and fail at dlopen
	// time with a much harder-to-diagnose error.
	scratch, err := os.MkdirTemp(cacheDir, "v"+Version+".scratch-*")
	if err != nil {
		return "", fmt.Errorf("ort: create scratch dir: %w", err)
	}
	defer os.RemoveAll(scratch)

	archiveName := strings.ReplaceAll(rel.Archive, "{version}", Version)
	url := releaseBaseURL + "/v" + Version + "/" + archiveName

	if err := downloadAndExtract(url, rel, scratch); err != nil {
		return "", err
	}

	if _, err := os.Stat(filepath.Join(scratch, rel.LibName)); err != nil {
		return "", fmt.Errorf("ort: expected %s in archive but not found after extraction: %w", rel.LibName, err)
	}

	if err := os.Rename(scratch, versionDir); err != nil {
		// A racing bootstrap (two processes starting at once) may
		// have populated versionDir between our Stat and Rename.
		// If so, and the library is there, defer to their work.
		if _, statErr := os.Stat(libFile); statErr == nil {
			return versionDir, nil
		}
		return "", fmt.Errorf("ort: publish extracted library: %w", err)
	}

	return versionDir, nil
}

// DefaultCacheDir resolves the cache root used by Bootstrap when the
// caller passes an empty cacheDir.
//
// Resolution:
//
//  1. $DEADZONE_ORT_CACHE if set.
//  2. os.UserCacheDir() + /deadzone/ort — the platform default.
//  3. ./.deadzone-cache/ort as a last-resort fallback so Bootstrap
//     can still proceed when UserCacheDir fails.
func DefaultCacheDir() string {
	if dir := os.Getenv(EnvCacheDir); dir != "" {
		return dir
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(".deadzone-cache", "ort")
	}
	return filepath.Join(base, "deadzone", "ort")
}

// Supported reports whether the current platform has a pinned release.
// Callers that want to offer a friendlier first-run error (e.g.
// "install onnxruntime manually and set DEADZONE_ORT_LIB_PATH") can
// branch on this before calling Bootstrap.
func Supported() bool {
	_, ok := pinnedReleases[runtime.GOOS+"/"+runtime.GOARCH]
	return ok
}

// PinnedRelease returns the GitHub-release archive URL, hex SHA256, and
// expected library filename for the onnxruntime build pinned to Version
// on the given GOOS+GOARCH. ok is false when no entry exists.
//
// Out-of-band stagers (the OCI image build, air-gapped install scripts)
// use this to fetch the same archive Bootstrap would, without
// duplicating the URL / SHA256 / filename in their own configs. archiveKind
// stays unexported because callers operating outside Bootstrap should
// not branch on it — the on-disk shape after extraction is what matters,
// not the wrapper format.
func PinnedRelease(goos, goarch string) (url, sha256, libName string, ok bool) {
	rel, found := pinnedReleases[goos+"/"+goarch]
	if !found {
		return "", "", "", false
	}
	archive := strings.ReplaceAll(rel.Archive, "{version}", Version)
	return releaseBaseURL + "/v" + Version + "/" + archive, rel.SHA256, rel.LibName, true
}

// SupportedPlatforms returns "goos/goarch" keys with a pinned release,
// in deterministic alphabetical order so callers (CI, doc generation)
// get stable output across runs.
func SupportedPlatforms() []string {
	out := make([]string, 0, len(pinnedReleases))
	for k := range pinnedReleases {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// downloadAndExtract streams url through a sha256 hasher while
// buffering it into a temp file, verifies the digest, then unpacks the
// archive into destDir. The lib files (versioned binary + unversioned
// symlink) are flattened to the top of destDir so hugot's
// getDefaultLibraryPaths finds LibName directly under the returned
// directory.
func downloadAndExtract(url string, rel release, destDir string) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("ort: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ort: download %s: HTTP %d", url, resp.StatusCode)
	}

	// Buffer to disk so we can rewind after the SHA256 check. The
	// archives are 8-30 MB — small enough to hold on tmpfs, but
	// holding them in memory wastes RSS when other packages are
	// simultaneously loading the ONNX model.
	tmp, err := os.CreateTemp(destDir, "download-*.archive")
	if err != nil {
		return fmt.Errorf("ort: create download tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("ort: stream download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("ort: close download tempfile: %w", err)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != rel.SHA256 {
		return fmt.Errorf("ort: sha256 mismatch for %s: got %s, want %s", url, got, rel.SHA256)
	}

	switch rel.archiveKind {
	case "tgz":
		return extractTgz(tmpPath, rel.LibName, destDir)
	case "zip":
		return extractZip(tmpPath, rel.LibName, destDir)
	default:
		return fmt.Errorf("ort: unknown archive kind %q", rel.archiveKind)
	}
}

// extractTgz walks the tarball and copies out every file under
// */lib/ that matches the library's versioned or unversioned form.
// Symlinks are preserved; everything else (headers, cmake files,
// testdata, dSYM bundles) is discarded.
func extractTgz(archivePath, libName, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("ort: open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("ort: gzip reader: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("ort: tar next: %w", err)
		}
		base := path.Base(hdr.Name)
		if !isLibArtifact(base, libName) {
			continue
		}
		// Guard against absolute paths or traversal in a malicious
		// archive, even though we trust the Microsoft release. base
		// is already sanitized by path.Base, but reject overt
		// nastiness before writing.
		if strings.Contains(base, "..") || strings.ContainsRune(base, filepath.Separator) {
			return fmt.Errorf("ort: refusing suspicious archive entry %q", hdr.Name)
		}
		dest := filepath.Join(destDir, base)
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			_ = os.Remove(dest)
			if err := os.Symlink(hdr.Linkname, dest); err != nil {
				return fmt.Errorf("ort: create symlink %s -> %s: %w", dest, hdr.Linkname, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := writeReg(dest, tr, hdr.FileInfo().Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

// extractZip mirrors extractTgz for Windows zip archives. ZIP entries
// lack the TypeSymlink / TypeReg distinction of tar; we only see
// regular files and treat them accordingly.
func extractZip(archivePath, libName, destDir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("ort: open zip: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		base := path.Base(f.Name)
		if !isLibArtifact(base, libName) {
			continue
		}
		if strings.Contains(base, "..") || strings.ContainsRune(base, filepath.Separator) {
			return fmt.Errorf("ort: refusing suspicious archive entry %q", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("ort: open zip entry %s: %w", f.Name, err)
		}
		dest := filepath.Join(destDir, base)
		if err := writeReg(dest, rc, f.Mode()); err != nil {
			rc.Close()
			return err
		}
		rc.Close()
	}
	return nil
}

// isLibArtifact recognizes the library filename itself and every
// versioned sibling / symlink alias that sits next to it. We keep
// "libonnxruntime.1.25.1.dylib" (the real Mach-O), "libonnxruntime.dylib"
// (the symlink), and the corresponding Linux ".so.<version>" forms —
// dropping one of these would leave the other with a dangling link
// target.
func isLibArtifact(name, libName string) bool {
	if name == libName {
		return true
	}
	// Linux: libonnxruntime.so.1.25.1 alongside libonnxruntime.so.
	if strings.HasPrefix(name, libName+".") {
		return true
	}
	// Darwin: libonnxruntime.1.25.1.dylib alongside libonnxruntime.dylib.
	if strings.HasPrefix(libName, "libonnxruntime.") && strings.HasSuffix(libName, ".dylib") {
		if strings.HasPrefix(name, "libonnxruntime.") && strings.HasSuffix(name, ".dylib") {
			return true
		}
	}
	return false
}

func writeReg(dest string, r io.Reader, mode fs.FileMode) error {
	_ = os.Remove(dest)
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return fmt.Errorf("ort: create %s: %w", dest, err)
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return fmt.Errorf("ort: write %s: %w", dest, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("ort: close %s: %w", dest, err)
	}
	return nil
}
