// DISABLED — per-artifact distribution paused while operator drives
// deadzone.db releases manually. Will be re-enabled when CI takes over
// distribution at scale. See issue #101.
//
// The per-artifact download flow (walk manifest.packs, HTTP GET each
// asset, verify sha256, place next to state.yaml) is preserved — not
// deleted — for the eventual revival. Reviving it is a one-banner
// delete plus re-enabling the dispatch in cmd/deadzone/packs.go.
//
// The Fetcher interface used to live here; it has moved to fetcher.go
// so the interface stays live for future callers.

package packs

import (
	"context"
)

// DownloadOptions is kept as a type so disabled-but-present callers
// compile; see file banner.
type DownloadOptions struct {
	ArtifactsDir string
	ManifestPath string
	Repo         string
	LibFilter    string
	Fetcher      Fetcher
}

// DownloadSummary is kept as a type so disabled-but-present callers
// compile; see file banner.
type DownloadSummary struct {
	Downloaded   int
	Verified     int
	Redownloaded int
}

// RunDownload always returns errPerArtifactDisabled. Revival path: drop
// this stub and uncomment the original body preserved below.
func RunDownload(_ context.Context, _ DownloadOptions) (DownloadSummary, error) {
	return DownloadSummary{}, errPerArtifactDisabled
}

/*
// Original implementation — DISABLED; see #101. Preserved verbatim so
// the revival patch is a one-banner-delete + restore.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

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
				slog.Info("packs.verified", "lib_id", p.LibID, "asset", p.Asset, "sha256", p.SHA256)
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
			if err := ensureStateIfMissing(ctx, opts.Fetcher, opts.Repo, manifest.ReleaseTag, opts.ArtifactsDir, p); err != nil {
				slog.Warn("packs.state_download_failed", "lib_id", p.LibID, "err", err.Error())
			}
			continue
		}

		url := assetURL(opts.Repo, manifest.ReleaseTag, p.Asset)
		if err := streamToFileVerified(ctx, opts.Fetcher, url, assetPath, p.SHA256); err != nil {
			return summary, fmt.Errorf("download %s: %w", p.LibID, err)
		}

		slog.Info("packs.downloaded", "lib_id", p.LibID, "asset", p.Asset, "sha256", p.SHA256, "size", p.Size, "url", url)
		if needsRedownloadLog {
			summary.Redownloaded++
		} else {
			summary.Downloaded++
		}

		if err := ensureStateDownloaded(ctx, opts.Fetcher, opts.Repo, manifest.ReleaseTag, opts.ArtifactsDir, p); err != nil {
			slog.Warn("packs.state_download_failed", "lib_id", p.LibID, "err", err.Error())
		}
	}

	return summary, nil
}

func ensureStateIfMissing(ctx context.Context, fetcher Fetcher, repo, tag, artifactsDir string, p Pack) error {
	destPath := filepath.Join(artifactsDir, p.Asset+".state")
	if _, err := os.Stat(destPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat sidecar %s: %w", destPath, err)
	}
	return ensureStateDownloaded(ctx, fetcher, repo, tag, artifactsDir, p)
}

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

	slog.Info("packs.state_downloaded", "lib_id", p.LibID, "state_asset", stateAsset)
	return nil
}

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
		return fmt.Errorf("sha256 mismatch on %s: got %s, want %s", url, gotHash, wantHash)
	}

	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, dest, err)
	}
	cleanup = false
	return nil
}

func assetURL(repo, tag, asset string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, asset)
}
*/
