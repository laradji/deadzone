package main

// DISABLED — see issue #101. Per-artifact pack distribution is paused
// while the operator drives deadzone.db releases manually via
// `deadzone dbrelease`. The upload/download/list subcommands used to
// wrap the per-lib flow in internal/packs/upload.go etc.; those are
// disabled (see banners there) and every dispatch entry here returns a
// clear error pointing at dbrelease. The original runPacksUpload /
// runPacksDownload / runPacksList / resolvePacksRepo helpers are
// preserved verbatim in the commented block at the bottom for the
// eventual revival.

import (
	"errors"
	"fmt"
	"os"
)

var errPacksSubDisabled = errors.New("per-artifact pack distribution disabled — see #101; use 'deadzone dbrelease' for the deadzone.db release flow")

// runPacks is the `deadzone packs` entry point. It now short-circuits
// every subcommand to the disabled error; the help path still works so
// an operator running `deadzone packs` sees the explanation rather
// than a silent no-op.
func runPacks(args []string) error {
	if len(args) == 0 {
		packsUsage()
		os.Exit(2)
	}
	sub := args[0]
	switch sub {
	case "upload", "download", "list":
		return errPacksSubDisabled
	case "-h", "--help", "help":
		packsUsage()
		return nil
	default:
		packsUsage()
		return fmt.Errorf("unknown packs subcommand %q", sub)
	}
}

func packsUsage() {
	fmt.Fprintln(os.Stderr, `Usage: deadzone packs <subcommand> [flags]

Subcommands:
  upload    DISABLED (#101) — use 'deadzone dbrelease' to ship deadzone.db
  download  DISABLED (#101) — per-artifact distribution paused
  list      DISABLED (#101) — manifest now records a single release entry

The per-artifact flow is expected to return when CI takes over
distribution at scale. Until then, ship the consolidated deadzone.db to
the tagged GitHub Release via:

  just dbrelease v0.1.0
  # or: deadzone dbrelease -tag v0.1.0 -db deadzone.db`)
}

/*
// Original pre-#101 implementation — DISABLED; see issue #101. The
// runPacksUpload / runPacksDownload / runPacksList / resolvePacksRepo
// helpers below are preserved verbatim for the eventual revival of
// per-artifact distribution. They reference Manifest.Packs / Pack /
// DefaultReleaseTag which were removed in the manifest-schema rewrite;
// reviving the dispatch means restoring those alongside the rest of
// the internal/packs upload/download/list files.

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"time"

	"github.com/laradji/deadzone/internal/logs"
	"github.com/laradji/deadzone/internal/packs"
)

func runPacksUpload(args []string) error {
	fs := flag.NewFlagSet("packs upload", flag.ExitOnError)
	artifactsDir := fs.String("artifacts", "./artifacts", "directory containing per-lib *.db artifact files")
	manifestPath := fs.String("manifest", "./artifacts/manifest.yaml", "path to artifacts/manifest.yaml")
	repo := fs.String("repo", "", "GitHub owner/name (overrides manifest.repo; default "+packs.DefaultRepo+")")
	verbose := fs.Bool("verbose", false, "enable Debug-level slog output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	slog.SetDefault(logs.New(os.Stderr, *verbose))

	resolvedRepo, source, err := resolvePacksRepo(*manifestPath, *repo)
	if err != nil {
		return err
	}
	slog.Info("packs.upload.start",
		"artifacts_dir", *artifactsDir,
		"manifest_path", *manifestPath,
		"repo", resolvedRepo,
		"repo_source", source,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	start := time.Now()
	summary, err := packs.RunUpload(ctx, packs.UploadOptions{
		ArtifactsDir: *artifactsDir,
		ManifestPath: *manifestPath,
		Repo:         resolvedRepo,
		Releaser:     packs.NewGHReleaser(),
	})
	if err != nil {
		return err
	}
	slog.Info("packs.upload.done",
		"uploaded", summary.Uploaded,
		"skipped", summary.Skipped,
		"preserved", summary.Preserved,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

func runPacksDownload(args []string) error {
	fs := flag.NewFlagSet("packs download", flag.ExitOnError)
	artifactsDir := fs.String("artifacts", "./artifacts", "directory to write downloaded *.db files into")
	manifestPath := fs.String("manifest", "./artifacts/manifest.yaml", "path to artifacts/manifest.yaml")
	repo := fs.String("repo", "", "GitHub owner/name (overrides manifest.repo; default "+packs.DefaultRepo+")")
	libFilter := fs.String("lib", "", "download only this lib_id (matches base or /base/version); empty = all")
	verbose := fs.Bool("verbose", false, "enable Debug-level slog output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	slog.SetDefault(logs.New(os.Stderr, *verbose))

	resolvedRepo, source, err := resolvePacksRepo(*manifestPath, *repo)
	if err != nil {
		return err
	}
	slog.Info("packs.download.start",
		"artifacts_dir", *artifactsDir,
		"manifest_path", *manifestPath,
		"repo", resolvedRepo,
		"repo_source", source,
		"lib_filter", *libFilter,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	start := time.Now()
	summary, err := packs.RunDownload(ctx, packs.DownloadOptions{
		ArtifactsDir: *artifactsDir,
		ManifestPath: *manifestPath,
		Repo:         resolvedRepo,
		LibFilter:    *libFilter,
		Fetcher:      packs.NewHTTPFetcher(http.DefaultClient),
	})
	if err != nil {
		return err
	}
	slog.Info("packs.download.done",
		"downloaded", summary.Downloaded,
		"verified", summary.Verified,
		"redownloaded", summary.Redownloaded,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

func runPacksList(args []string) error {
	fs := flag.NewFlagSet("packs list", flag.ExitOnError)
	manifestPath := fs.String("manifest", "./artifacts/manifest.yaml", "path to artifacts/manifest.yaml")
	artifactsDir := fs.String("artifacts", "./artifacts", "directory to read local *.db.state sidecars from for the extended columns")
	verbose := fs.Bool("verbose", false, "enable Debug-level slog output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	slog.SetDefault(logs.New(os.Stderr, *verbose))
	return packs.RunList(packs.ListOptions{
		ManifestPath: *manifestPath,
		ArtifactsDir: *artifactsDir,
	}, os.Stdout)
}

func resolvePacksRepo(manifestPath, flagValue string) (string, string, error) {
	if flagValue != "" {
		return flagValue, "flag", nil
	}
	if manifestPath != "" {
		if m, err := packs.Load(manifestPath); err == nil && m.Repo != "" {
			return m.Repo, "manifest", nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("resolve repo: %w", err)
		}
	}
	return packs.DefaultRepo, "default", nil
}
*/
