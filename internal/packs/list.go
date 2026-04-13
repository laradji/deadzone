// DISABLED — per-artifact distribution paused while operator drives
// deadzone.db releases manually. Will be re-enabled when CI takes over
// distribution at scale. See issue #101.
//
// The per-artifact list flow (pretty-print manifest.packs as a
// tabwriter table with sidecar-derived columns) is preserved — not
// deleted — for the eventual revival.

package packs

import "io"

// ListOptions is kept as a type so disabled-but-present callers
// compile; see file banner.
type ListOptions struct {
	ManifestPath string
	ArtifactsDir string
}

// RunList always returns errPerArtifactDisabled. Revival path: drop
// this stub and uncomment the original body preserved below.
func RunList(_ ListOptions, _ io.Writer) error {
	return errPerArtifactDisabled
}

/*
// Original implementation — DISABLED; see #101. Preserved verbatim so
// the revival patch is a one-banner-delete + restore.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
)

const emDash = "—"

func RunList(opts ListOptions, w io.Writer) error {
	manifest, err := Load(opts.ManifestPath)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	artifactsDir := opts.ArtifactsDir
	if artifactsDir == "" {
		artifactsDir = filepath.Dir(opts.ManifestPath)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LIB_ID\tASSET\tSIZE\tSHA256\tINDEXED_AT\tEMBEDDER\tDOCS\tUPDATED_AT")
	for _, p := range manifest.Packs {
		short := p.SHA256
		if len(short) > 12 {
			short = short[:12]
		}

		embedder, docs, updatedAt := emDash, emDash, emDash
		statePath := filepath.Join(artifactsDir, p.Asset+".state")
		if s, err := LoadState(statePath); err == nil {
			embedder = fmt.Sprintf("%s %s", s.Embedder.Kind, s.Embedder.Model)
			docs = fmt.Sprintf("%d", s.DocCount)
			if !s.UpdatedAt.IsZero() {
				updatedAt = s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
		} else if !os.IsNotExist(err) {
			embedder = emDash
		}

		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			p.LibID,
			p.Asset,
			p.Size,
			short,
			p.IndexedAt.UTC().Format("2006-01-02T15:04:05Z"),
			embedder,
			docs,
			updatedAt,
		)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("list: flush: %w", err)
	}
	if len(manifest.Packs) == 0 {
		fmt.Fprintln(w, "(no packs in manifest)")
	}
	return nil
}
*/
