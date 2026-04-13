package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/laradji/deadzone/internal/logs"
	"github.com/laradji/deadzone/internal/packs"
)

// runPacks is the `deadzone packs` entry point. It owns the second
// level of subcommand routing (upload / download / list) — the same
// `os.Args` switch + `flag.NewFlagSet` per sub pattern the old
// cmd/packs/main.go already used, preserved verbatim as a nested
// router under the top-level `deadzone` binary.
//
// Ordering matters: packs subs produce human output on stdout (list)
// vs. structured logs on stderr (upload/download), so callers can pipe
// `deadzone packs list` into awk/column without slog noise.
func runPacks(args []string) error {
	if len(args) == 0 {
		packsUsage()
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "upload":
		return runPacksUpload(rest)
	case "download":
		return runPacksDownload(rest)
	case "list":
		return runPacksList(rest)
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
  upload    Upload local artifacts/*.db to the rolling GitHub Release
  download  Download release assets referenced by the manifest into ./artifacts
  list      Print the manifest as a table

Run "deadzone packs <subcommand> -h" for the flags supported by each.`)
}

// runPacksUpload parses upload-specific flags, sets up logging, resolves
// the repo from (-repo flag) || manifest.repo || DefaultRepo, builds
// the production GHReleaser, and calls packs.RunUpload. The repo
// resolution chain is logged so a misconfigured run is visible in
// stderr without --verbose.
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

// runPacksDownload mirrors runPacksUpload's structure for the download
// path. The production Fetcher wraps http.DefaultClient — public
// release assets don't need auth, and DefaultClient transparently
// follows GitHub's 302 to objects.githubusercontent.com.
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

// runPacksList parses the small list flag set and writes the table to
// stdout. Logs still go to stderr — the table is the only thing on
// stdout so callers can pipe it into `column`, `awk`, or similar
// without slog noise.
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

// resolvePacksRepo implements the three-tier repo resolution: explicit
// -repo flag wins, then the manifest's `repo:` field if any, then the
// hardcoded default. Returns the chosen value and a "source" string
// for the structured log line so misconfiguration is visible.
//
// The manifest read here is best-effort: a missing manifest is NOT
// fatal because the upload subcommand will surface the same error in
// its real Load call. A malformed manifest IS fatal — surfacing it
// here gives the operator a single error message instead of two.
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
