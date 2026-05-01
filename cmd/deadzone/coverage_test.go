package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "turso.tech/database/tursogo"
)

// TestRenderCoverage_Golden pins the exact byte shape of the
// rendered markdown against checked-in golden files. The test data
// is stable (no time.Now, no DB) so a diff in the output must
// originate from a template change — at which point set
// UPDATE_GOLDEN=1 to refresh every testdata/coverage.*.golden.md in
// one run and review the diff in code review.
//
// Three cases are pinned:
//
//   - populated: the canonical release-time output (3 rows, mix of
//     versioned + unversioned), exercising row formatting.
//   - empty:     a release tag set but zero indexed pairs — guards the
//     edge where dbrelease bumps the manifest before the
//     first scrape lands. Output truncates after the
//     header separator line.
//   - mixed:     both unversioned-only ("") and versioned ("0.1", "0.2")
//     rows for the same lib_id, plus rows ordered across the
//     doc_count DESC tiebreak so a regression in ORDER BY or
//     in the empty-version cell would be caught here.
func TestRenderCoverage_Golden(t *testing.T) {
	fixed := time.Date(2026, 4, 30, 11, 24, 0, 0, time.UTC)
	cases := []struct {
		name   string
		data   coverageData
		golden string
	}{
		{
			name: "populated",
			data: coverageData{
				ReleaseTag:  "v0.3.0",
				GeneratedAt: fixed,
				Rows: []coverageRow{
					{LibID: "/anomalyco/opencode", Version: "", DocCount: 203},
					{LibID: "/opentofu/opentofu", Version: "1.10", DocCount: 200},
					{LibID: "/opentofu/opentofu", Version: "1.11", DocCount: 195},
				},
			},
			golden: "coverage.golden.md",
		},
		{
			name: "empty",
			data: coverageData{
				ReleaseTag:  "v0.3.0",
				GeneratedAt: fixed,
				Rows:        nil,
			},
			golden: "coverage.empty.golden.md",
		},
		{
			name: "mixed",
			data: coverageData{
				ReleaseTag:  "v0.3.0",
				GeneratedAt: fixed,
				Rows: []coverageRow{
					{LibID: "/golang/go", Version: "", DocCount: 500},
					{LibID: "/laradji/turso", Version: "0.1", DocCount: 320},
					{LibID: "/anthropics/sdk", Version: "", DocCount: 145},
					{LibID: "/laradji/turso", Version: "0.2", DocCount: 80},
				},
			},
			golden: "coverage.mixed.golden.md",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderCoverage(tc.data)
			golden := filepath.Join("testdata", tc.golden)
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatalf("mkdir testdata: %v", err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden %s: %v (run with UPDATE_GOLDEN=1 to (re)create it)", golden, err)
			}
			if got != string(want) {
				t.Errorf("render mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, string(want))
			}
		})
	}
}

// TestRenderCoverage_Unreleased exercises the "no manifest yet" path
// where ReleaseTag is empty. The header must still render with a
// human-readable placeholder so a fresh checkout's `just coverage`
// produces a valid file before the first dbrelease ever runs.
func TestRenderCoverage_Unreleased(t *testing.T) {
	data := coverageData{
		ReleaseTag:  "",
		GeneratedAt: time.Date(2026, 4, 30, 11, 24, 0, 0, time.UTC),
		Rows:        nil,
	}
	got := renderCoverage(data)
	if !strings.Contains(got, "**Release:** (unreleased)") {
		t.Errorf("empty ReleaseTag must render as (unreleased); got:\n%s", got)
	}
	if !strings.HasSuffix(got, "|---|---|---|\n") {
		t.Errorf("zero-row output must end at the header separator; got:\n%s", got)
	}
}

// TestRenderCoverage_TrailingNewline pins the AC requirement that
// the file ends with a newline so editors that auto-fix newline-at-
// EOF do not produce churn diffs on every coverage run.
func TestRenderCoverage_TrailingNewline(t *testing.T) {
	data := coverageData{
		ReleaseTag:  "v0.3.0",
		GeneratedAt: time.Date(2026, 4, 30, 11, 24, 0, 0, time.UTC),
		Rows:        []coverageRow{{LibID: "/a/b", Version: "", DocCount: 1}},
	}
	got := renderCoverage(data)
	if !strings.HasSuffix(got, "\n") {
		tailStart := len(got) - 10
		if tailStart < 0 {
			tailStart = 0
		}
		t.Errorf("output must end with a newline, got tail %q", got[tailStart:])
	}
}

// TestReadCoverageRows verifies the SQL aggregation and ordering
// against a hand-built fixture DB. We bypass db.Open to avoid
// pulling in the embedder — the F32_BLOB column width is
// arbitrary because coverage never reads the embedding back.
func TestReadCoverageRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatalf("open turso: %v", err)
	}
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { raw.Close() })

	if _, err := raw.Exec(`CREATE TABLE docs (
		id INTEGER PRIMARY KEY,
		lib_id TEXT NOT NULL,
		version TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL,
		content TEXT NOT NULL,
		embedding F32_BLOB(4) NOT NULL
	)`); err != nil {
		t.Fatalf("create docs: %v", err)
	}

	insert := func(libID, version string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, err := raw.Exec(
				`INSERT INTO docs(lib_id, version, title, content, embedding) VALUES (?, ?, ?, ?, vector(?))`,
				libID, version, "t", "c", "[0.1,0.2,0.3,0.4]",
			); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}
	// Counts chosen so the doc_count DESC + lib_id ASC + version ASC
	// ordering is unambiguous (no ties on doc_count between rows of
	// different (lib_id, version)).
	insert("/b/y", "v1", 5)
	insert("/a/x", "", 3)
	insert("/b/y", "v2", 1)

	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	rows, err := readCoverageRows(path)
	if err != nil {
		t.Fatalf("readCoverageRows: %v", err)
	}

	want := []coverageRow{
		{LibID: "/b/y", Version: "v1", DocCount: 5},
		{LibID: "/a/x", Version: "", DocCount: 3},
		{LibID: "/b/y", Version: "v2", DocCount: 1},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows mismatch\n got: %+v\nwant: %+v", rows, want)
	}
}

// TestWriteAtomic_CreatesParentDir guards the "fresh checkout, no
// docs/ yet" path: writeAtomic must mkdir-p the parent so coverage
// can be the first writer to populate docs/.
func TestWriteAtomic_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "deeper", "nested", "coverage.md")
	if err := writeAtomic(target, []byte("ok\n")); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "ok\n" {
		t.Errorf("content = %q, want %q", got, "ok\n")
	}
}

