package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
)

// ConsolidateResult is the per-run summary returned by Consolidate. The
// counts are best-effort accounting for the operator log line; they have
// no bearing on the on-disk state, which is governed entirely by the
// transaction's commit/rollback.
type ConsolidateResult struct {
	// Artifacts is the number of artifact files merged into main.
	Artifacts int
	// DocsMerged is the total number of docs rows inserted into main
	// across every merged artifact (i.e. the new row count for the
	// libs that participated; rows displaced by the per-lib DELETE
	// are not counted as "merged").
	DocsMerged int
	// LibsMerged is the number of libs rows inserted into main. Each
	// artifact contributes either 0 (degenerate empty artifact) or 1.
	LibsMerged int
}

// Consolidate merges every per-lib artifact in artifactsDir into main.
// Each lib lives in its own subdirectory (artifacts/<slug>/artifact.db
// + state.yaml, see #101), so consolidation globs the nested shape
// rather than the old flat *.db layout. Artifacts are keyed on
// (lib_id, version) after #113 — two versions of the same base lib
// live in two sibling folders and merge as two independent (lib_id,
// version) slots without clobbering each other. Each artifact replaces
// (not appends to) the rows in main for its (lib_id, version) slot,
// in both the docs and libs tables, atomically.
//
// The operation runs in two passes:
//
//  1. Validation. Every artifact is opened with main's Meta so any
//     embedder mismatch (ErrEmbedderMismatch), schema mismatch
//     (ErrSchemaMismatch), or missing artifact lib_id
//     (ErrArtifactLibIDMissing) surfaces *before* any write touches
//     main. Failures here leave main byte-identical.
//
//  2. Merge. A single transaction is begun on main; for each validated
//     artifact, the existing rows for its lib_id are deleted from
//     docs and libs, and the artifact's rows are streamed in via the
//     transaction. The transaction commits at the end of the loop. If
//     any step in pass 2 fails, the transaction rolls back and main is
//     restored to its pre-call state.
//
// Reading and writing happen on different *sql.DB connection pools
// (one per artifact, one for main), so the merge does not contend
// with the per-pool MaxOpenConns=1 cap that tursogo serializes on.
//
// Artifact files are processed in lexicographic order so that the
// merged-on-disk doc IDs are stable across runs — useful for
// debugging diff'ing the same corpus on two machines.
func Consolidate(main *DB, artifactsDir string) (ConsolidateResult, error) {
	var result ConsolidateResult

	matches, err := filepath.Glob(filepath.Join(artifactsDir, "*", "artifact.db"))
	if err != nil {
		return result, fmt.Errorf("glob artifacts dir %s: %w", artifactsDir, err)
	}
	sort.Strings(matches)

	type entry struct {
		path    string
		libID   string
		version string
	}
	entries := make([]entry, 0, len(matches))

	// Pass 1 — validation. Opening each artifact with libID="" both
	// re-runs Open's meta/schema cross-check against main.Meta and
	// reads the artifact's recorded (lib_id, version). We close
	// immediately so the conn pool is free for pass 2.
	for _, path := range matches {
		a, err := OpenArtifact(path, main.Meta, "", "")
		if err != nil {
			return result, fmt.Errorf("validate artifact %s: %w", filepath.Base(path), err)
		}
		entries = append(entries, entry{path: path, libID: a.ArtifactLibID, version: a.ArtifactVersion})
		_ = a.Close()
	}

	if len(entries) == 0 {
		return result, nil
	}

	// Pass 2 — merge. database/sql Begin yields a tx that pins the
	// single tursogo connection; every read and write below issues
	// against tx, and the inner Query against the artifact runs on
	// the artifact's own (separate) pool, so we never re-enter the
	// main connection from two stacks at once.
	tx, err := main.Begin()
	if err != nil {
		return result, fmt.Errorf("begin main tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, e := range entries {
		merged, libRow, err := mergeArtifactInto(tx, e.path, e.libID, e.version, main.Meta)
		if err != nil {
			return result, fmt.Errorf("merge artifact %s: %w", filepath.Base(e.path), err)
		}
		result.Artifacts++
		result.DocsMerged += merged
		if libRow {
			result.LibsMerged++
		}
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit main tx: %w", err)
	}
	committed = true
	return result, nil
}

// mergeArtifactInto reopens one artifact, deletes any existing rows in
// main for its (lib_id, version) slot (across both docs and libs), and
// streams the artifact's rows in via the supplied transaction. Returns
// the number of docs inserted and whether a libs row was inserted.
//
// The DELETE is keyed on (lib_id, version), not lib_id alone: a merge
// of /foo v1.14 must not wipe out /foo v1.13's rows in main. Each
// artifact owns exactly one (lib_id, version) slot by construction
// (OpenArtifact enforces it via meta) so two versions of the same
// base lib are two independent artifacts that merge cleanly as two
// (lib_id, version) rows.
//
// The artifact is opened with the (lib_id, version) pair captured
// during pass 1; any drift between validation and merge (e.g. an
// artifact file rewritten between the two) surfaces here as
// ErrArtifactLibIDMismatch and rolls back the whole consolidation.
func mergeArtifactInto(tx *sql.Tx, path, libID, version string, meta Meta) (int, bool, error) {
	a, err := OpenArtifact(path, meta, libID, version)
	if err != nil {
		return 0, false, err
	}
	defer a.Close()

	// docs: drop the old per-(lib, version) slice in main, then
	// stream the artifact's rows back in. vector_extract / vector()
	// round-trips the F32_BLOB through the same JSON-array form
	// formatVector uses on the insert path, so we don't need to
	// teach this function about the on-disk encoding.
	if _, err := tx.Exec(`DELETE FROM docs WHERE lib_id = ? AND version = ?`, libID, version); err != nil {
		return 0, false, fmt.Errorf("delete docs for %q version %q: %w", libID, version, err)
	}
	docRows, err := a.Query(`SELECT lib_id, version, title, content, vector_extract(embedding) FROM docs`)
	if err != nil {
		return 0, false, fmt.Errorf("select artifact docs: %w", err)
	}
	docsInserted := 0
	for docRows.Next() {
		var rowLibID, rowVersion, title, content, vecJSON string
		if err := docRows.Scan(&rowLibID, &rowVersion, &title, &content, &vecJSON); err != nil {
			docRows.Close()
			return 0, false, fmt.Errorf("scan artifact doc: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO docs(lib_id, version, title, content, embedding) VALUES (?, ?, ?, ?, vector(?))`,
			rowLibID, rowVersion, title, content, vecJSON,
		); err != nil {
			docRows.Close()
			return 0, false, fmt.Errorf("insert doc into main: %w", err)
		}
		docsInserted++
	}
	if err := docRows.Err(); err != nil {
		docRows.Close()
		return 0, false, fmt.Errorf("iterate artifact docs: %w", err)
	}
	docRows.Close()

	// libs: same dance, keyed on (lib_id, version). Most artifacts
	// hold exactly one libs row (the pair they advertise via meta),
	// but the loop is generic in case a future scraper writes
	// additional bookkeeping rows.
	if _, err := tx.Exec(`DELETE FROM libs WHERE lib_id = ? AND version = ?`, libID, version); err != nil {
		return 0, false, fmt.Errorf("delete libs for %q version %q: %w", libID, version, err)
	}
	libRows, err := a.Query(`SELECT lib_id, version, doc_count, vector_extract(embedding) FROM libs`)
	if err != nil {
		return 0, false, fmt.Errorf("select artifact libs: %w", err)
	}
	libRowInserted := false
	for libRows.Next() {
		var rowLibID, rowVersion, vecJSON string
		var docCount int
		if err := libRows.Scan(&rowLibID, &rowVersion, &docCount, &vecJSON); err != nil {
			libRows.Close()
			return 0, false, fmt.Errorf("scan artifact lib: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO libs(lib_id, version, doc_count, embedding) VALUES (?, ?, ?, vector(?))`,
			rowLibID, rowVersion, docCount, vecJSON,
		); err != nil {
			libRows.Close()
			return 0, false, fmt.Errorf("insert lib into main: %w", err)
		}
		libRowInserted = true
	}
	if err := libRows.Err(); err != nil {
		libRows.Close()
		return 0, false, fmt.Errorf("iterate artifact libs: %w", err)
	}
	libRows.Close()

	return docsInserted, libRowInserted, nil
}
