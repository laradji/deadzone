package packs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
)

// emDash is the placeholder rendered in state-derived columns when the
// `.state` sidecar is missing locally. Using an em-dash (rather than
// "N/A" or a blank) keeps the table visibly aligned and makes missing
// metadata obvious at a glance.
const emDash = "—"

// ListOptions is the input to the list subcommand.
type ListOptions struct {
	// ManifestPath is the manifest to print. The other ListOptions
	// fields are reserved for future filtering — kept as a struct so
	// adding `-lib` later doesn't break the call signature.
	ManifestPath string
	// ArtifactsDir is the local directory where `.state` sidecars are
	// read from. Empty = use the manifest's directory (this is the
	// production behaviour — sidecars live next to the manifest in
	// `./artifacts/`).
	ArtifactsDir string
}

// RunList prints the manifest as a tabwriter table to w. The output is
// optimized for human eyeballing on a terminal: columns are LIB_ID,
// ASSET, SIZE, SHA256 (12-char prefix), INDEXED_AT (RFC3339), plus
// EMBEDDER, DOCS, and UPDATED_AT pulled from the per-artifact `.state`
// sidecar when present locally (em-dash when not).
//
// Stdout vs. stderr separation: cmd/packs sends list output to stdout
// while logs go to stderr (same convention as the rest of the binaries
// — stderr is structured, stdout is human-facing).
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
			// A corrupt sidecar shouldn't blow up the whole list; show
			// em-dash and continue. The operator still sees a row.
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
