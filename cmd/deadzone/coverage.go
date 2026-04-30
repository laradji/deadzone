package main

// coverage is the `deadzone coverage` subcommand introduced in #152.
// It renders a public-facing markdown view of what (lib_id, version)
// pairs are indexed in the consolidated deadzone.db, together with
// each pair's doc count. The output is committed to docs/coverage.md
// alongside artifacts/manifest.yaml so contributors can audit
// coverage on the repo browser without sqlite3 in hand.
//
// The command:
//   - opens the DB via a bare sql.Open (no embedder load) — same
//     pattern as dbrelease's readDBStats: callers run this from a
//     fresh checkout without ORT/tokenizers, against a deadzone.db
//     they just downloaded or built;
//   - reads the latest release tag from artifacts/manifest.yaml when
//     present (for the header), falling back to "(unreleased)" when
//     the file is missing or carries the zero-value record;
//   - GROUPs docs by (lib_id, version), ORDERed by doc_count desc;
//   - renders via text/template into a stable shape the bot commit
//     can diff cleanly.

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	_ "turso.tech/database/tursogo"

	"github.com/laradji/deadzone/internal/logs"
	"github.com/laradji/deadzone/internal/packs"
)

var (
	coverageDBPath       string
	coverageOutputPath   string
	coverageManifestPath string
	coverageVerbose      bool
)

var coverageCmd = &cobra.Command{
	Use:   "coverage",
	Short: "Render docs/coverage.md from the consolidated deadzone.db",
	Long: `Render a public-facing markdown coverage map listing every
(lib_id, version) pair indexed in the consolidated database, sorted by
doc count. Writes to docs/coverage.md by default — committed alongside
artifacts/manifest.yaml on every release so contributors can see
what's already indexed before filing a library-add issue.

The command opens the DB with a bare driver call (no embedder load),
so it runs cleanly on a fresh checkout with no model cache.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runCoverage()
	},
}

func init() {
	coverageCmd.Flags().StringVar(&coverageDBPath, "db", "./deadzone.db",
		"path to the consolidated deadzone.db")
	coverageCmd.Flags().StringVar(&coverageOutputPath, "output", "./docs/coverage.md",
		"output markdown file path")
	coverageCmd.Flags().StringVar(&coverageManifestPath, "manifest", "./artifacts/manifest.yaml",
		"manifest to read the release tag from (best-effort; missing file falls back to '(unreleased)')")
	coverageCmd.Flags().BoolVar(&coverageVerbose, "verbose", false,
		"enable Debug-level slog output")
	rootCmd.AddCommand(coverageCmd)
}

// coverageRow is one (lib_id, version, doc_count) tuple read from the
// consolidated DB. Mirrors the columns rendered in the output table.
type coverageRow struct {
	LibID    string
	Version  string
	DocCount int
}

// coverageData is the payload handed to the markdown template.
// ReleaseTag is "" when no manifest was found or the manifest's
// release record is the zero-value form — the template renders that
// as "(unreleased)" so a fresh checkout's `just coverage` produces
// a sensible file before the first dbrelease ever runs.
type coverageData struct {
	ReleaseTag  string
	GeneratedAt time.Time
	PairCount   int
	DocTotal    int
	Rows        []coverageRow
}

func runCoverage() error {
	slog.SetDefault(logs.New(os.Stderr, coverageVerbose))

	if _, err := os.Stat(coverageDBPath); err != nil {
		return fmt.Errorf("coverage: stat %s: %w", coverageDBPath, err)
	}

	releaseTag := readReleaseTag(coverageManifestPath)

	rows, err := readCoverageRows(coverageDBPath)
	if err != nil {
		return fmt.Errorf("coverage: %w", err)
	}

	docTotal := 0
	for _, r := range rows {
		docTotal += r.DocCount
	}

	data := coverageData{
		ReleaseTag:  releaseTag,
		GeneratedAt: time.Now().UTC(),
		PairCount:   len(rows),
		DocTotal:    docTotal,
		Rows:        rows,
	}

	md := renderCoverage(data)
	if err := writeAtomic(coverageOutputPath, []byte(md)); err != nil {
		return fmt.Errorf("coverage: write %s: %w", coverageOutputPath, err)
	}

	slog.Info("coverage.done",
		"db_path", coverageDBPath,
		"output_path", coverageOutputPath,
		"release_tag", releaseTag,
		"pair_count", data.PairCount,
		"doc_total", data.DocTotal,
	)
	return nil
}

// readReleaseTag is best-effort: a missing or zero-value manifest is
// not a coverage failure (local dev runs the command long before the
// first release exists). The "(unreleased)" fallback is applied at
// render time, not here, so the empty-string value travels through
// the data struct unchanged for testability.
func readReleaseTag(path string) string {
	m, err := packs.Load(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("coverage.manifest_load_failed", "path", path, "err", err.Error())
		}
		return ""
	}
	return m.Release.Tag
}

// readCoverageRows opens the DB through tursogo's bare sql driver
// (no db.OpenReader, no embedder) and aggregates docs by (lib_id,
// version). The ORDER BY is doc_count DESC then (lib_id, version) ASC
// so identically-counted rows still produce a deterministic diff
// across runs.
func readCoverageRows(path string) ([]coverageRow, error) {
	raw, err := sql.Open("turso", path)
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)

	rows, err := raw.Query(`
		SELECT lib_id, version, COUNT(*) AS n
		FROM   docs
		GROUP  BY lib_id, version
		ORDER  BY n DESC, lib_id ASC, version ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query docs: %w", err)
	}
	defer rows.Close()

	var out []coverageRow
	for rows.Next() {
		var r coverageRow
		if err := rows.Scan(&r.LibID, &r.Version, &r.DocCount); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter docs: %w", err)
	}
	return out, nil
}

// coverageTemplate is the exact markdown shape committed to
// docs/coverage.md. String-concatenated rather than backtick-quoted
// so the markdown's own backticks (around `deadzone coverage` etc.)
// stay readable. Whitespace is intentional: trailing newline after
// every row, single blank line between sections.
const coverageTemplate = "# Library coverage\n" +
	"\n" +
	"> Auto-generated by `deadzone coverage`. Do not hand-edit — run `just coverage` to refresh.\n" +
	"\n" +
	"- **Release:** {{.ReleaseDisplay}}\n" +
	"- **Generated:** {{.GeneratedDisplay}}\n" +
	"- **Totals:** {{.PairCount}} (lib_id, version) pairs · {{.DocTotal}} docs\n" +
	"\n" +
	"| lib_id | version | doc_count |\n" +
	"|---|---|---|\n" +
	"{{range .Rows}}| {{.LibID}} | {{.Version}} | {{.DocCount}} |\n{{end}}"

// renderCoverage produces the final markdown bytes. Pure function
// over coverageData so the golden-file test can exercise it without
// touching disk or the DB.
func renderCoverage(d coverageData) string {
	type view struct {
		coverageData
		ReleaseDisplay   string
		GeneratedDisplay string
	}
	rel := strings.TrimSpace(d.ReleaseTag)
	if rel == "" {
		rel = "(unreleased)"
	}
	v := view{
		coverageData:     d,
		ReleaseDisplay:   rel,
		GeneratedDisplay: d.GeneratedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	tpl := template.Must(template.New("coverage").Parse(coverageTemplate))
	var sb strings.Builder
	if err := tpl.Execute(&sb, v); err != nil {
		// The template is a package-level constant; a runtime Execute
		// error here means the data struct disagrees with the template
		// — which is a programmer bug, not a user-facing failure.
		panic(fmt.Sprintf("coverage: render: %v", err))
	}
	return sb.String()
}

// writeAtomic writes data to path via a temp file + rename so a
// concurrent reader (e.g. the bot's `git add` running in CI) never
// observes a partially-written file. Creates parent dirs as needed
// — handy on a fresh clone where docs/ may not yet exist.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".coverage.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}
