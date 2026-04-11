package packs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/laradji/deadzone/internal/db"
)

// Releaser is the surface upload.Run uses to talk to GitHub Releases.
// It exists as an interface for one reason only: tests substitute a
// fakeReleaser to assert behaviour like "second upload of unchanged
// artifact makes zero gh calls" without spinning up a real `gh`. The
// production implementation is GHReleaser at the bottom of this file.
type Releaser interface {
	// EnsureRelease guarantees that a release with tag exists for the
	// given owner/repo. It is safe (and cheap) to call multiple times;
	// upload.Run only ever calls it once per run, but the contract
	// allows the implementation to no-op on a second call.
	EnsureRelease(ctx context.Context, repo, tag string) error

	// Upload uploads a single file as a release asset, clobbering any
	// existing asset with the same name. The asset name on the release
	// is filepath.Base(file).
	Upload(ctx context.Context, repo, tag, file string) error
}

// UploadOptions are the inputs to the upload subcommand.
type UploadOptions struct {
	// ArtifactsDir is the directory containing per-lib *.db files. The
	// upload walk picks up every *.db in this dir at the top level,
	// not recursively.
	ArtifactsDir string
	// ManifestPath is the location of artifacts/manifest.yaml. The
	// file must already exist (the placeholder shape with empty
	// `packs: []` is fine); upload.Run does not create it from scratch
	// because the maintainer should commit the placeholder explicitly
	// in the same change that wires up cmd/packs.
	ManifestPath string
	// Repo is the owner/name of the GitHub repository the release lives
	// in. Resolved upstream by cmd/packs (flag overrides manifest
	// `repo:` field which overrides DefaultRepo). Empty here is a
	// programming error and produces a clear failure.
	Repo string
	// Releaser is injected by cmd/packs (production GHReleaser) or by
	// tests (fakeReleaser). Required.
	Releaser Releaser
}

// UploadSummary is the operator-facing rollup the cmd logs at the end
// of an upload run. Counts are by lib_id (one entry per artifact file
// processed), not by gh API call.
type UploadSummary struct {
	// Uploaded is the number of artifacts that were either new or had
	// a different sha256 than the manifest's existing entry, and were
	// successfully pushed to the release.
	Uploaded int
	// Skipped is the number of artifacts whose sha256 already matched
	// the manifest's existing entry. These cause zero gh calls.
	Skipped int
	// Preserved is the number of manifest entries whose corresponding
	// .db file is NOT present in ArtifactsDir at upload time. The
	// entries stay in the manifest unchanged so a single-lib refresh
	// doesn't show up as "deleted N libs" in the diff.
	Preserved int
}

// RunUpload walks ArtifactsDir, uploads any artifact whose sha256 has
// changed (or which is new) to the rolling release, and rewrites the
// manifest to match. The function is the implementation of the
// `deadzone-packs upload` subcommand.
//
// Algorithm:
//
//  1. Load the manifest. The release_tag is read from there; the file
//     must exist (placeholder is fine).
//  2. Glob *.db files, sort lexicographically for determinism.
//  3. For each file: compute sha256, read lib_id + embedder identity
//     via the lightweight db.ReadArtifactMeta (no embedder needed),
//     and look up the existing manifest entry. Matching sha256 → skip
//     and preserve verbatim. Otherwise build a new Pack with a fresh
//     IndexedAt and queue it.
//  4. If anything is queued, EnsureRelease once, then Upload each
//     queued pack in order.
//  5. Merge queued packs into the manifest (Replace by lib_id),
//     leaving unseen entries untouched.
//  6. Save the manifest atomically.
//
// On any error after step 4, the manifest is NOT rewritten — the
// caller can retry the run after fixing the underlying issue and the
// idempotent skip in step 3 keeps the retry cheap.
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
		path string
		pack Pack
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
			// Identical content already on the release; preserve the
			// existing entry verbatim (including IndexedAt) so re-running
			// upload is a true byte-level no-op on the manifest.
			slog.Info("packs.upload.skipped",
				"lib_id", meta.LibID,
				"asset", assetName,
				"sha256", hash,
				"reason", "sha256_unchanged",
			)
			summary.Skipped++
			continue
		}

		toUpload = append(toUpload, pending{
			path: path,
			pack: Pack{
				LibID:               meta.LibID,
				Asset:               assetName,
				SHA256:              hash,
				Size:                fi.Size(),
				IndexedAt:           time.Now().UTC(),
				ScrapedWithEmbedder: meta.EmbedderKind,
				ScrapedWithModel:    meta.ModelVersion,
			},
		})
	}

	// Count entries that stay in the manifest because their local file
	// is absent from ArtifactsDir. This is the "single-lib refresh"
	// happy path: the maintainer ran scrape -lib X, only that artifact
	// is on disk, but the manifest still references the other libs.
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
			slog.Info("packs.upload.uploaded",
				"lib_id", item.pack.LibID,
				"asset", item.pack.Asset,
				"sha256", item.pack.SHA256,
				"size", item.pack.Size,
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

// GHReleaser is the production Releaser, implemented by shelling out to
// the `gh` CLI. It is intentionally thin: every method is a single
// exec.Cmd with stderr captured for diagnostics, and the upload path
// auto-creates the release on first use by detecting "release not
// found" in stderr.
//
// `gh` is already on every contributor's PATH per #22 and handles auth
// (2FA, token refresh, org scopes) transparently — reimplementing this
// in pure Go would be a worse `gh`.
type GHReleaser struct {
	// ensured tracks (repo, tag) pairs we've already verified during
	// this run, so a multi-file upload only calls `gh release view`
	// once. Reset at the start of each cmd invocation by virtue of
	// being a per-instance map.
	ensured map[string]bool
}

// NewGHReleaser returns a fresh production releaser. Always pass to
// upload.Run via opts.Releaser; never reuse across processes.
func NewGHReleaser() *GHReleaser {
	return &GHReleaser{ensured: map[string]bool{}}
}

// EnsureRelease checks via `gh release view` whether a release exists
// for the tag, and runs `gh release create` if it doesn't. The result
// is cached per-instance so a 10-file upload run only pays the network
// roundtrip once.
//
// On the create path the release is given a minimal title and an empty
// notes body — the maintainer is free to edit these on the GitHub UI
// later, but the rolling-tag model means the same release object lives
// forever, so a one-line note covers it.
func (g *GHReleaser) EnsureRelease(ctx context.Context, repo, tag string) error {
	cacheKey := repo + "\x00" + tag
	if g.ensured[cacheKey] {
		return nil
	}
	// `gh release view` exits 0 with the release info on stdout if it
	// exists, and exits 1 with a "release not found" message on stderr
	// if it doesn't. We don't need the body, just the exit code.
	view := exec.CommandContext(ctx, "gh", "release", "view", tag, "--repo", repo)
	var stderr bytes.Buffer
	view.Stderr = &stderr
	if err := view.Run(); err == nil {
		g.ensured[cacheKey] = true
		return nil
	} else if !isReleaseNotFound(stderr.String()) {
		return fmt.Errorf("gh release view %s/%s: %w (%s)", repo, tag, err, strings.TrimSpace(stderr.String()))
	}

	slog.Info("packs.upload.creating_release",
		"repo", repo,
		"tag", tag,
	)
	create := exec.CommandContext(ctx, "gh", "release", "create", tag,
		"--repo", repo,
		"--title", "Deadzone pack artifacts",
		"--notes", "Rolling release of per-lib documentation artifacts. See artifacts/manifest.yaml in the repo for the canonical sha256 list.",
	)
	var createErr bytes.Buffer
	create.Stderr = &createErr
	if err := create.Run(); err != nil {
		return fmt.Errorf("gh release create %s/%s: %w (%s)", repo, tag, err, strings.TrimSpace(createErr.String()))
	}
	g.ensured[cacheKey] = true
	return nil
}

// Upload runs `gh release upload <tag> <file> --clobber --repo <repo>`.
// --clobber lets the same asset name be re-uploaded over an older copy,
// which is the rolling-release model from #30.
func (g *GHReleaser) Upload(ctx context.Context, repo, tag, file string) error {
	cmd := exec.CommandContext(ctx, "gh", "release", "upload", tag, file,
		"--clobber",
		"--repo", repo,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh release upload %s/%s %s: %w (%s)", repo, tag, file, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// isReleaseNotFound recognises the stderr message gh emits when a
// release tag isn't on the repo yet. The exact wording is "release not
// found" (lowercase) as of gh 2.x; we lowercase the comparison so a
// future capitalization tweak doesn't silently break the auto-create
// path.
func isReleaseNotFound(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "release not found")
}
