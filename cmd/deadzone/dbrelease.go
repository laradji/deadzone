package main

// dbrelease is the `deadzone dbrelease` subcommand introduced in #101.
// It wraps the manual "ship deadzone.db to a tagged GitHub Release"
// flow the operator drives from their laptop until CI takes over
// distribution at scale.
//
// Assumptions (§F of #101):
//   - The tag already exists on the origin remote (operator pushed it).
//   - CI's release.yml has already run for that tag and created the
//     release object with the per-platform binary tarballs attached.
//   - deadzone.db exists locally at --db (default ./deadzone.db).
//
// Steps:
//  1. sha256 ./deadzone.db and write ./deadzone.db.sha256 next to it.
//  2. Open the DB via a bare sql.Open (no embedder load) and read the
//     embedder identity + lib/doc counts via SELECTs against meta/libs/docs.
//  3. EnsureRelease, then Upload both deadzone.db and deadzone.db.sha256
//     with --clobber.
//  4. Rewrite artifacts/manifest.yaml with the new Release record and
//     log a one-line summary reminding the operator to commit.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	_ "turso.tech/database/tursogo"

	"github.com/laradji/deadzone/internal/logs"
	"github.com/laradji/deadzone/internal/packs"
)

var (
	dbreleaseDBPath       string
	dbreleaseTag          string
	dbreleaseRepo         string
	dbreleaseManifestPath string
	dbreleaseVerbose      bool
)

var dbreleaseCmd = &cobra.Command{
	Use:   "dbrelease",
	Short: "Upload ./deadzone.db to a tagged GitHub Release (operator-driven)",
	Long: `Ship deadzone.db to a tagged GitHub Release. The tag must already
exist on origin (this command does not push tags); CI's release.yml must
already have run for that tag so the release object exists.

Writes deadzone.db + deadzone.db.sha256 to the release, then rewrites
artifacts/manifest.yaml with the new record — remember to commit that.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDBRelease()
	},
}

func init() {
	dbreleaseCmd.Flags().StringVar(&dbreleaseDBPath, "db", "deadzone.db",
		"path to the consolidated deadzone.db")
	dbreleaseCmd.Flags().StringVar(&dbreleaseTag, "tag", "",
		"release tag to upload to (required; the tag must already exist on origin)")
	dbreleaseCmd.Flags().StringVar(&dbreleaseRepo, "repo", "",
		"GitHub owner/name (default: from git remote via `gh`, falling back to "+packs.DefaultRepo+")")
	dbreleaseCmd.Flags().StringVar(&dbreleaseManifestPath, "manifest", "./artifacts/manifest.yaml",
		"manifest to update")
	dbreleaseCmd.Flags().BoolVar(&dbreleaseVerbose, "verbose", false,
		"enable Debug-level slog output")
	// Cobra's required check fires on flag.Changed, so the runtime
	// TrimSpace guard in runDBRelease still has to catch `--tag ""`.
	_ = dbreleaseCmd.MarkFlagRequired("tag")
	rootCmd.AddCommand(dbreleaseCmd)
}

func runDBRelease() error {
	slog.SetDefault(logs.New(os.Stderr, dbreleaseVerbose))

	if strings.TrimSpace(dbreleaseTag) == "" {
		return errors.New("dbrelease: --tag is required")
	}

	fi, err := os.Stat(dbreleaseDBPath)
	if err != nil {
		return fmt.Errorf("dbrelease: stat %s: %w", dbreleaseDBPath, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("dbrelease: --db %s is a directory", dbreleaseDBPath)
	}

	resolvedRepo := strings.TrimSpace(dbreleaseRepo)
	if resolvedRepo == "" {
		resolvedRepo = resolveRepoFromGit(dbreleaseManifestPath)
	}
	slog.Info("dbrelease.start",
		"db_path", dbreleaseDBPath,
		"tag", dbreleaseTag,
		"repo", resolvedRepo,
		"manifest_path", dbreleaseManifestPath,
	)

	hash, err := fileSHA256(dbreleaseDBPath)
	if err != nil {
		return fmt.Errorf("dbrelease: sha256 %s: %w", dbreleaseDBPath, err)
	}

	shaPath := dbreleaseDBPath + ".sha256"
	shaLine := fmt.Sprintf("%s  %s\n", hash, filepath.Base(dbreleaseDBPath))
	if err := os.WriteFile(shaPath, []byte(shaLine), 0o644); err != nil {
		return fmt.Errorf("dbrelease: write %s: %w", shaPath, err)
	}

	stats, err := readDBStats(dbreleaseDBPath)
	if err != nil {
		return fmt.Errorf("dbrelease: read db stats: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	releaser := packs.NewGHReleaser()
	if err := releaser.EnsureRelease(ctx, resolvedRepo, dbreleaseTag); err != nil {
		return fmt.Errorf("dbrelease: %w", err)
	}
	if err := releaser.Upload(ctx, resolvedRepo, dbreleaseTag, dbreleaseDBPath); err != nil {
		return fmt.Errorf("dbrelease: upload db: %w", err)
	}
	if err := releaser.Upload(ctx, resolvedRepo, dbreleaseTag, shaPath); err != nil {
		return fmt.Errorf("dbrelease: upload sha256: %w", err)
	}

	// Manifest rewrite happens AFTER the successful upload so a failed
	// run leaves the committed manifest unchanged and the operator can
	// retry without polluting git state.
	m := &packs.Manifest{
		Release: packs.ReleaseRecord{
			Tag:       dbreleaseTag,
			Asset:     filepath.Base(dbreleaseDBPath),
			SHA256:    hash,
			Size:      fi.Size(),
			IndexedAt: time.Now().UTC(),
			Embedder: packs.EmbedderRecord{
				Kind:  stats.embedderKind,
				Model: stats.modelVersion,
				Dim:   stats.embeddingDim,
			},
			LibCount: stats.libCount,
			DocCount: stats.docCount,
		},
	}
	if err := m.Save(dbreleaseManifestPath); err != nil {
		return fmt.Errorf("dbrelease: save manifest: %w", err)
	}

	slog.Info("dbrelease.done",
		"tag", dbreleaseTag,
		"asset", filepath.Base(dbreleaseDBPath),
		"sha256", hash,
		"size", fi.Size(),
		"lib_count", stats.libCount,
		"doc_count", stats.docCount,
		"manifest_path", dbreleaseManifestPath,
	)
	fmt.Fprintf(os.Stderr, "dbrelease: wrote %s — remember to commit it.\n", dbreleaseManifestPath)
	return nil
}

// dbStats bundles the bits dbrelease pulls out of deadzone.db for the
// manifest record. Populated by readDBStats via a bare sql.Open so the
// subcommand does not load an embedder (the operator might be on a
// machine with no model cache and we don't want to stall the release
// behind a 131 MB download).
type dbStats struct {
	embedderKind string
	modelVersion string
	embeddingDim int
	libCount     int
	docCount     int
}

// readDBStats opens path via tursogo's bare sql driver, reads the
// embedder identity from the meta table, and counts rows in libs and
// docs. Mirrors db.ReadArtifactMeta's approach of staying upstream of
// db.Open so the 90MB+ model cache isn't a prerequisite for shipping a
// release.
func readDBStats(path string) (dbStats, error) {
	raw, err := sql.Open("turso", path)
	if err != nil {
		return dbStats{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)

	var stats dbStats
	rows, err := raw.Query(`SELECT key, value FROM meta WHERE key IN (?, ?, ?)`,
		"embedder_kind", "embedding_dim", "model_version")
	if err != nil {
		return dbStats{}, fmt.Errorf("query meta: %w", err)
	}
	values := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return dbStats{}, fmt.Errorf("scan meta: %w", err)
		}
		values[k] = v
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return dbStats{}, fmt.Errorf("iter meta: %w", err)
	}
	rows.Close()

	stats.embedderKind = values["embedder_kind"]
	stats.modelVersion = values["model_version"]
	if dimStr := values["embedding_dim"]; dimStr != "" {
		dim, err := strconv.Atoi(dimStr)
		if err != nil {
			return dbStats{}, fmt.Errorf("parse embedding_dim %q: %w", dimStr, err)
		}
		stats.embeddingDim = dim
	}

	if err := raw.QueryRow(`SELECT count(*) FROM libs`).Scan(&stats.libCount); err != nil {
		return dbStats{}, fmt.Errorf("count libs: %w", err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM docs`).Scan(&stats.docCount); err != nil {
		return dbStats{}, fmt.Errorf("count docs: %w", err)
	}
	return stats, nil
}

// fileSHA256 is a small streaming hasher kept local to dbrelease — the
// packs package's FileSHA256 works but pulling it in here would also
// drag in the (disabled) per-artifact upload flow's sibling code.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// resolveRepoFromGit falls back to `gh repo view --json nameWithOwner`
// when --repo is unset, giving the operator a sensible default on a
// typical clone. On any failure we drop to packs.DefaultRepo — the
// GHReleaser call will surface a clearer error if the ultimate target
// is wrong. The Debug log on the fallback path names the underlying
// error so an operator running `--verbose` can tell apart "no gh on
// PATH" from "not in a git checkout" from "gh auth missing".
func resolveRepoFromGit(_ string) string {
	out, err := exec.Command("gh", "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner").Output()
	if err != nil {
		slog.Debug("dbrelease.resolve_repo_fallback", "default", packs.DefaultRepo, "err", err)
		return packs.DefaultRepo
	}
	repo := strings.TrimSpace(string(out))
	if repo == "" {
		slog.Debug("dbrelease.resolve_repo_fallback", "default", packs.DefaultRepo, "reason", "empty gh output")
		return packs.DefaultRepo
	}
	return repo
}
