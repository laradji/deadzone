package packs

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// ListOptions is the input to the list subcommand.
type ListOptions struct {
	// ManifestPath is the manifest to print. The other ListOptions
	// fields are reserved for future filtering — kept as a struct so
	// adding `-lib` later doesn't break the call signature.
	ManifestPath string
}

// RunList prints the manifest as a tabwriter table to w. The output is
// optimized for human eyeballing on a terminal: columns are LIB_ID,
// ASSET, SIZE, SHA256 (12-char prefix), INDEXED_AT (RFC3339).
//
// Stdout vs. stderr separation: cmd/packs sends list output to stdout
// while logs go to stderr (same convention as the rest of the binaries
// — stderr is structured, stdout is human-facing).
func RunList(opts ListOptions, w io.Writer) error {
	manifest, err := Load(opts.ManifestPath)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LIB_ID\tASSET\tSIZE\tSHA256\tINDEXED_AT")
	for _, p := range manifest.Packs {
		short := p.SHA256
		if len(short) > 12 {
			short = short[:12]
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
			p.LibID,
			p.Asset,
			p.Size,
			short,
			p.IndexedAt.UTC().Format("2006-01-02T15:04:05Z"),
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
