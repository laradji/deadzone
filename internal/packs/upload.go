// DISABLED — per-artifact distribution paused while operator drives
// deadzone.db releases manually. Will be re-enabled when CI takes over
// distribution at scale. See issue #101.
//
// The per-artifact upload flow (walk artifacts/*/artifact.db, diff
// against the manifest, shell out to `gh release upload --clobber` per
// changed lib, rewrite the manifest) predates the #101 decision to
// ship only the consolidated deadzone.db. The code here is intentionally
// preserved — not deleted — because the same flow is expected to return
// when CI takes over distribution. Reviving it is a one-banner delete
// plus re-enabling the dispatch in cmd/deadzone/packs.go.
//
// The Releaser interface used to live here; it has moved to releaser.go
// so `deadzone dbrelease` can reuse it. The Fetcher interface moved
// analogously to fetcher.go.

package packs

import (
	"context"
	"errors"
)

// errPerArtifactDisabled is the operator-facing message returned by
// every disabled per-artifact entry point. Kept as a package-level
// var so dispatch callers can share the exact wording.
var errPerArtifactDisabled = errors.New("per-artifact pack distribution disabled — see #101; use 'deadzone dbrelease' for the deadzone.db release flow")

// UploadOptions is kept as a type so disabled-but-present callers
// compile; see file banner.
type UploadOptions struct {
	ArtifactsDir string
	ManifestPath string
	Repo         string
	Releaser     Releaser
}

// UploadSummary is kept as a type so disabled-but-present callers
// compile; see file banner.
type UploadSummary struct {
	Uploaded  int
	Skipped   int
	Preserved int
}

// RunUpload always returns errPerArtifactDisabled. Revival path: drop
// this stub and uncomment the original body preserved below.
func RunUpload(_ context.Context, _ UploadOptions) (UploadSummary, error) {
	return UploadSummary{}, errPerArtifactDisabled
}

/*
// Original implementation — DISABLED; see #101. Preserved verbatim
// (minus interface moves documented in the banner above) so the
// revival patch is a one-banner-delete + restore.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/laradji/deadzone/internal/db"
)

func RunUpload(ctx context.Context, opts UploadOptions) (UploadSummary, error) {
	if opts.Releaser == nil {
		return UploadSummary{}, errors.New("upload: Releaser is required")
	}
	if opts.Repo == "" {
		return UploadSummary{}, errors.New("upload: Repo is required")
	}

	manifest, err := Load(opts.ManifestPath)
	if err != nil {
		return UploadSummary{}, fmt.Errorf("upload: %w", err)
	}

	matches, err := filepath.Glob(filepath.Join(opts.ArtifactsDir, "*.db"))
	if err != nil {
		return UploadSummary{}, fmt.Errorf("upload: glob %s: %w", opts.ArtifactsDir, err)
	}
	sort.Strings(matches)

	type pending struct {
		path      string
		statePath string
		pack      Pack
	}
	var (
		toUpload []pending
		summary  UploadSummary
	)
	seenLibIDs := map[string]struct{}{}

	for _, path := range matches {
		hash, err := FileSHA256(path)
		if err != nil {
			return UploadSummary{}, fmt.Errorf("upload: %w", err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			return UploadSummary{}, fmt.Errorf("upload: stat %s: %w", path, err)
		}
		meta, err := db.ReadArtifactMeta(path)
		if err != nil {
			return UploadSummary{}, fmt.Errorf("upload: read artifact meta %s: %w", path, err)
		}
		seenLibIDs[meta.LibID] = struct{}{}

		assetName := filepath.Base(path)

		if existing, _, found := manifest.Find(meta.LibID); found && existing.SHA256 == hash {
			slog.Info("packs.upload.skipped",
				"lib_id", meta.LibID,
				"asset", assetName,
				"sha256", hash,
				"reason", "sha256_unchanged",
			)
			summary.Skipped++
			continue
		}

		statePath := StatePath(path)
		if _, err := os.Stat(statePath); err != nil {
			if os.IsNotExist(err) {
				return summary, fmt.Errorf(
					"upload: missing sidecar %s — re-run `just scrape %s` to regenerate",
					statePath, meta.LibID)
			}
			return summary, fmt.Errorf("upload: stat sidecar %s: %w", statePath, err)
		}

		toUpload = append(toUpload, pending{
			path:      path,
			statePath: statePath,
			pack: Pack{
				LibID:     meta.LibID,
				Asset:     assetName,
				SHA256:    hash,
				Size:      fi.Size(),
				IndexedAt: time.Now().UTC(),
			},
		})
	}

	for _, p := range manifest.Packs {
		if _, present := seenLibIDs[p.LibID]; !present {
			summary.Preserved++
		}
	}

	if len(toUpload) > 0 {
		if err := opts.Releaser.EnsureRelease(ctx, opts.Repo, manifest.ReleaseTag); err != nil {
			return summary, fmt.Errorf("upload: ensure release %s/%s: %w", opts.Repo, manifest.ReleaseTag, err)
		}
		for _, item := range toUpload {
			if err := opts.Releaser.Upload(ctx, opts.Repo, manifest.ReleaseTag, item.path); err != nil {
				return summary, fmt.Errorf("upload: upload %s: %w", item.path, err)
			}
			if err := opts.Releaser.Upload(ctx, opts.Repo, manifest.ReleaseTag, item.statePath); err != nil {
				return summary, fmt.Errorf("upload: upload sidecar %s: %w", item.statePath, err)
			}
			slog.Info("packs.upload.uploaded",
				"lib_id", item.pack.LibID,
				"asset", item.pack.Asset,
				"sha256", item.pack.SHA256,
				"size", item.pack.Size,
				"state_asset", filepath.Base(item.statePath),
			)
			manifest.Replace(item.pack)
			summary.Uploaded++
		}

		if err := manifest.Save(opts.ManifestPath); err != nil {
			return summary, fmt.Errorf("upload: save manifest: %w", err)
		}
	}

	return summary, nil
}
*/
