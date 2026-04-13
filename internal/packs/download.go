package packs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
)

// Fetcher is the surface download.Run uses to pull asset bytes from a
// release URL. The production implementation is httpFetcher; tests
// substitute their own to avoid hitting the real internet.
type Fetcher interface {
	// Get returns a reader for the body at url. The reader's Close
	// must be called by the caller. A non-2xx status returns an error
	// — implementations are responsible for translating "404" into a
	// useful message containing the URL.
	Get(ctx context.Context, url string) (io.ReadCloser, error)
}

// FetcherFunc is the http.HandlerFunc-style adapter for Fetcher: it
// lets a plain function be used wherever a Fetcher is required, which
// is convenient in tests that need to redirect URLs through an
// httptest.Server. Production callers use NewHTTPFetcher and never
// need this adapter.
type FetcherFunc func(ctx context.Context, url string) (io.ReadCloser, error)

// Get satisfies the Fetcher interface by calling the underlying
// function.
func (f FetcherFunc) Get(ctx context.Context, url string) (io.ReadCloser, error) {
	return f(ctx, url)
}

// DownloadOptions are the inputs to the download subcommand.
type DownloadOptions struct {
	// ArtifactsDir is where downloaded *.db files are placed. Created
	// on demand if missing.
	ArtifactsDir string
	// ManifestPath is the source of truth for which assets to pull.
	ManifestPath string
	// Repo is the owner/name of the GitHub repository the release
	// lives in. Resolved upstream by cmd/packs (flag overrides
	// manifest `repo:` field which overrides DefaultRepo).
	Repo string
	// LibFilter, if non-empty, restricts the run to packs whose
	// lib_id matches the two-level filter (exact match or /-bounded
	// prefix). Empty = download every entry in the manifest.
	LibFilter string
	// Fetcher is injected by cmd/packs (production NewHTTPFetcher) or
	// by tests. Required.
	Fetcher Fetcher
}

// DownloadSummary is the operator-facing rollup the cmd logs at the
// end of a download run. Counts are by manifest entry.
type DownloadSummary struct {
	// Downloaded is the number of fresh asset downloads (no local
	// file existed).
	Downloaded int
	// Verified is the number of entries whose local file already
	// existed and matched the manifest sha256 — zero network calls.
	Verified int
	// Redownloaded is the number of entries whose local file existed
	// but had a sha256 mismatch (treated as disposable build output
	// and silently re-fetched).
	Redownloaded int
}

// RunDownload pulls every release asset referenced by the manifest into
// ArtifactsDir, verifying sha256 on the way down. Tampering semantics
// are asymmetric:
//
//   - Local file mismatch (artifacts/*.db modified by something else):
//     log a `packs.verified_redownload` event and re-fetch silently.
//     /artifacts/ is gitignored disposable build output, so silent
//     repair is the right behaviour.
//
//   - Server-side mismatch (downloaded bytes don't match manifest):
//     hard abort with a got/want error. This means either the release
//     asset is corrupted or the manifest is stale, and silently
//     overwriting a good local file with a bad one is the wrong fix.
//
// The function never moves a sha256-mismatched file into the canonical
// path: streaming writes go to a sibling .tmp first, the hash is
// verified before os.Rename, and a Ctrl-C mid-stream leaves only the
// .tmp file (which the next run will clean up).
func RunDownload(ctx context.Context, opts DownloadOptions) (DownloadSummary, error) {
	if opts.Fetcher == nil {
		return DownloadSummary{}, errors.New("download: Fetcher is required")
	}
	if opts.Repo == "" {
		return DownloadSummary{}, errors.New("download: Repo is required")
	}

	manifest, err := Load(opts.ManifestPath)
	if err != nil {
		return DownloadSummary{}, fmt.Errorf("download: %w", err)
	}

	if err := os.MkdirAll(opts.ArtifactsDir, 0o755); err != nil {
		return DownloadSummary{}, fmt.Errorf("download: create artifacts dir %s: %w", opts.ArtifactsDir, err)
	}

	target := manifest.FilterByLibID(opts.LibFilter)
	if opts.LibFilter != "" && len(target) == 0 {
		return DownloadSummary{}, fmt.Errorf("download: no manifest entries match -lib %q", opts.LibFilter)
	}

	var summary DownloadSummary
	for _, p := range target {
		assetPath := filepath.Join(opts.ArtifactsDir, p.Asset)
		needsDownload := true
		needsRedownloadLog := false

		if _, err := os.Stat(assetPath); err == nil {
			localHash, err := FileSHA256(assetPath)
			if err != nil {
				return summary, fmt.Errorf("download: hash existing %s: %w", assetPath, err)
			}
			if localHash == p.SHA256 {
				slog.Info("packs.verified",
					"lib_id", p.LibID,
					"asset", p.Asset,
					"sha256", p.SHA256,
				)
				summary.Verified++
				needsDownload = false
			} else {
				slog.Info("packs.verified_redownload",
					"lib_id", p.LibID,
					"asset", p.Asset,
					"want_sha256", p.SHA256,
					"got_sha256", localHash,
					"reason", "local_sha256_mismatch",
				)
				if err := os.Remove(assetPath); err != nil {
					return summary, fmt.Errorf("download: remove tampered local %s: %w", assetPath, err)
				}
				needsRedownloadLog = true
			}
		} else if !os.IsNotExist(err) {
			return summary, fmt.Errorf("download: stat %s: %w", assetPath, err)
		}

		if !needsDownload {
			// `.db` is verified locally. Fetch the `.state` sidecar
			// only if it is absent locally — the sidecar has no
			// sha256 in the manifest, so we can't re-verify an
			// existing local copy and "present means good enough".
			// This keeps the idempotent "verified-only" run's HTTP
			// count at zero.
			if err := ensureStateIfMissing(ctx, opts.Fetcher, opts.Repo, manifest.ReleaseTag, opts.ArtifactsDir, p); err != nil {
				slog.Warn("packs.state_download_failed",
					"lib_id", p.LibID,
					"err", err.Error(),
				)
			}
			continue
		}

		url := assetURL(opts.Repo, manifest.ReleaseTag, p.Asset)
		if err := streamToFileVerified(ctx, opts.Fetcher, url, assetPath, p.SHA256); err != nil {
			return summary, fmt.Errorf("download %s: %w", p.LibID, err)
		}

		slog.Info("packs.downloaded",
			"lib_id", p.LibID,
			"asset", p.Asset,
			"sha256", p.SHA256,
			"size", p.Size,
			"url", url,
		)
		if needsRedownloadLog {
			summary.Redownloaded++
		} else {
			summary.Downloaded++
		}

		// Fetch the sidecar alongside the `.db`. A sidecar fetch
		// failure is non-fatal: the `.db` is still usable by
		// consolidate/server, and `packs list` will just render
		// em-dashes for the missing metadata. Surface it as a warning
		// so an operator can spot the drift without a failed run.
		if err := ensureStateDownloaded(ctx, opts.Fetcher, opts.Repo, manifest.ReleaseTag, opts.ArtifactsDir, p); err != nil {
			slog.Warn("packs.state_download_failed",
				"lib_id", p.LibID,
				"err", err.Error(),
			)
		}
	}

	return summary, nil
}

// ensureStateIfMissing fetches the `.state` sidecar only when the
// local file is absent, so the verified-local happy path remains a
// zero-HTTP operation. Use ensureStateDownloaded when the sidecar is
// always expected to be (re)fetched.
func ensureStateIfMissing(ctx context.Context, fetcher Fetcher, repo, tag, artifactsDir string, p Pack) error {
	destPath := filepath.Join(artifactsDir, p.Asset+".state")
	if _, err := os.Stat(destPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat sidecar %s: %w", destPath, err)
	}
	return ensureStateDownloaded(ctx, fetcher, repo, tag, artifactsDir, p)
}

// ensureStateDownloaded fetches the `.state` sidecar for a pack into
// ArtifactsDir, overwriting any existing sidecar. It is a best-effort
// helper called after the "download fresh" branch — the sidecar has
// no sha256 in the manifest and no verification step, it's just small
// YAML metadata that travels with the `.db` on the rolling release.
func ensureStateDownloaded(ctx context.Context, fetcher Fetcher, repo, tag, artifactsDir string, p Pack) error {
	stateAsset := p.Asset + ".state"
	destPath := filepath.Join(artifactsDir, stateAsset)
	url := assetURL(repo, tag, stateAsset)

	body, err := fetcher.Get(ctx, url)
	if err != nil {
		return fmt.Errorf("fetch sidecar %s: %w", url, err)
	}
	defer body.Close()

	tmp := destPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create sidecar tmp %s: %w", tmp, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		return fmt.Errorf("stream sidecar %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close sidecar tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, destPath); err != nil {
		return fmt.Errorf("rename sidecar %s -> %s: %w", tmp, destPath, err)
	}
	cleanup = false

	slog.Info("packs.state_downloaded",
		"lib_id", p.LibID,
		"state_asset", stateAsset,
	)
	return nil
}

// streamToFileVerified fetches url and streams the body into <dest>.tmp,
// computing the sha256 in flight via io.MultiWriter, and ONLY renames
// the temp file into the canonical dest after the hash matches
// wantHash. The "verify before rename" ordering is the load-bearing
// invariant: a hash mismatch leaves the canonical path untouched, so
// a tampered release asset can never replace a known-good local file
// even transiently. A Ctrl-C mid-stream leaves only a .tmp file (the
// next run cleans it up by overwriting on its os.Create).
//
// Returns a wrapped error containing both the URL and the got/want
// pair on a hash mismatch, since "the file you tried to download is
// corrupt" is one of the few cases where the operator needs to take
// action (typically: re-run scrape + upload to refresh the manifest).
func streamToFileVerified(ctx context.Context, fetcher Fetcher, url, dest, wantHash string) error {
	body, err := fetcher.Get(ctx, url)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer body.Close()

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create tmp %s: %w", tmp, err)
	}
	// Best-effort cleanup if anything below this point fails (including
	// the hash mismatch path). Ignore the error from a no-op Remove
	// after a successful Rename — Rename will have removed the file.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), body); err != nil {
		_ = f.Close()
		return fmt.Errorf("stream %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close tmp %s: %w", tmp, err)
	}

	gotHash := hex.EncodeToString(hasher.Sum(nil))
	if gotHash != wantHash {
		// cleanup defer takes care of removing the .tmp; we never
		// wrote anything to dest, so there is no canonical file to
		// roll back. Error wording matches the operator-action
		// guidance in the docstring above.
		return fmt.Errorf("sha256 mismatch on %s: got %s, want %s", url, gotHash, wantHash)
	}

	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, dest, err)
	}
	cleanup = false
	return nil
}

// assetURL builds the canonical public download URL for a release
// asset. GitHub redirects this to objects.githubusercontent.com via
// 302; net/http's default client follows the redirect transparently.
// Kept as a helper so tests can swap in their own server with a
// matching path layout.
func assetURL(repo, tag, asset string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, asset)
}

// httpFetcher is the production Fetcher implementation. It's a thin
// wrapper around an http.Client so cmd/packs can pass a context-aware
// client with sane defaults; tests build their own against
// httptest.NewServer.
type httpFetcher struct {
	client *http.Client
}

// NewHTTPFetcher returns a production Fetcher backed by the supplied
// client. Pass http.DefaultClient unless you specifically need a
// non-default timeout or transport.
func NewHTTPFetcher(client *http.Client) Fetcher {
	if client == nil {
		client = http.DefaultClient
	}
	return &httpFetcher{client: client}
}

func (h *httpFetcher) Get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("new request %s: %w", url, err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}
