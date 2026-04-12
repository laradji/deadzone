// Command packs distributes per-lib artifact .db files via a rolling
// GitHub Release. It is the implementation of issue #30 and exposes
// three subcommands:
//
//	deadzone-packs upload   # push local artifacts/*.db to the release
//	deadzone-packs download # pull every manifest entry into ./artifacts
//	deadzone-packs list     # print the manifest as a table
//
// Run `deadzone-packs <sub> -h` for the per-subcommand flag set.
//
// The binary is intentionally split per-subcommand on the cmd line so
// it composes naturally with `xargs`, scripts, and CI runners that
// expect a single positional verb. Production callers always go
// through the justfile recipes (`just packs-upload`, etc.) which wrap
// the right combination of flags.
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

	"github.com/laradji/deadzone/internal/buildinfo"
	"github.com/laradji/deadzone/internal/logs"
	"github.com/laradji/deadzone/internal/packs"
)

// Build-time values overridden by `-ldflags -X main.version=…` at
// release build time (see justfile's build-release recipe).
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		slog.Error("packs fatal", "err", err.Error())
		os.Exit(1)
	}
}

// run dispatches to the correct subcommand handler. Each handler owns
// its own flag.FlagSet so the help text for `packs upload -h` only
// shows upload's flags, not the union of all subcommands.
func run(sub string, args []string) error {
	switch sub {
	case "upload":
		return runUpload(args)
	case "download":
		return runDownload(args)
	case "list":
		return runList(args)
	case "-h", "--help", "help":
		usage()
		return nil
	case "-version", "--version", "version":
		fmt.Println(buildinfo.Format("deadzone-packs", version, commit, date))
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: deadzone-packs <subcommand> [flags]

Subcommands:
  upload    Upload local artifacts/*.db to the rolling GitHub Release
  download  Download release assets referenced by the manifest into ./artifacts
  list      Print the manifest as a table
  version   Print version and exit

Run "deadzone-packs <subcommand> -h" for the flags supported by each.`)
}

// runUpload parses upload-specific flags, sets up logging, resolves
// the repo from (-repo flag) || manifest.repo || DefaultRepo, builds
// the production GHReleaser, and calls packs.RunUpload. The repo
// resolution chain is logged so a misconfigured run is visible in
// stderr without --verbose.
func runUpload(args []string) error {
	fs := flag.NewFlagSet("upload", flag.ExitOnError)
	artifactsDir := fs.String("artifacts", "./artifacts", "directory containing per-lib *.db artifact files")
	manifestPath := fs.String("manifest", "./artifacts/manifest.yaml", "path to artifacts/manifest.yaml")
	repo := fs.String("repo", "", "GitHub owner/name (overrides manifest.repo; default "+packs.DefaultRepo+")")
	verbose := fs.Bool("verbose", false, "enable Debug-level slog output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	slog.SetDefault(logs.New(os.Stderr, *verbose))

	resolvedRepo, source, err := resolveRepo(*manifestPath, *repo)
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

// runDownload mirrors runUpload's structure for the download path. The
// production Fetcher wraps http.DefaultClient — public release assets
// don't need auth, and DefaultClient transparently follows GitHub's
// 302 to objects.githubusercontent.com.
func runDownload(args []string) error {
	fs := flag.NewFlagSet("download", flag.ExitOnError)
	artifactsDir := fs.String("artifacts", "./artifacts", "directory to write downloaded *.db files into")
	manifestPath := fs.String("manifest", "./artifacts/manifest.yaml", "path to artifacts/manifest.yaml")
	repo := fs.String("repo", "", "GitHub owner/name (overrides manifest.repo; default "+packs.DefaultRepo+")")
	libFilter := fs.String("lib", "", "download only this lib_id (matches base or /base/version); empty = all")
	verbose := fs.Bool("verbose", false, "enable Debug-level slog output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	slog.SetDefault(logs.New(os.Stderr, *verbose))

	resolvedRepo, source, err := resolveRepo(*manifestPath, *repo)
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

// runList parses the small list flag set and writes the table to
// stdout. Logs still go to stderr — the table is the only thing on
// stdout so callers can pipe it into `column`, `awk`, or similar
// without slog noise.
func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	manifestPath := fs.String("manifest", "./artifacts/manifest.yaml", "path to artifacts/manifest.yaml")
	verbose := fs.Bool("verbose", false, "enable Debug-level slog output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	slog.SetDefault(logs.New(os.Stderr, *verbose))
	return packs.RunList(packs.ListOptions{ManifestPath: *manifestPath}, os.Stdout)
}

// resolveRepo implements the three-tier repo resolution: explicit
// -repo flag wins, then the manifest's `repo:` field if any, then the
// hardcoded default. Returns the chosen value and a "source" string
// for the structured log line so misconfiguration is visible.
//
// The manifest read here is best-effort: a missing or malformed
// manifest is NOT fatal because the upload subcommand will surface
// the same error in its real Load call. The double-load is cheap and
// keeps the resolveRepo helper independent of the subcommand context.
func resolveRepo(manifestPath, flagValue string) (string, string, error) {
	if flagValue != "" {
		return flagValue, "flag", nil
	}
	if manifestPath != "" {
		if m, err := packs.Load(manifestPath); err == nil && m.Repo != "" {
			return m.Repo, "manifest", nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			// A malformed manifest at this stage is a hard error
			// because the subsequent upload/download call WILL fail
			// the same way; surfacing it here gives the operator a
			// single error message instead of two.
			return "", "", fmt.Errorf("resolve repo: %w", err)
		}
	}
	return packs.DefaultRepo, "default", nil
}
