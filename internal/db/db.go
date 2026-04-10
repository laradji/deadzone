// Package db manages the local turso database that backs deadzone's
// vector-based semantic search. Documents are stored as TEXT alongside a
// fixed-dimension F32_BLOB embedding column; queries are ranked by
// vector_distance_cos against the query's embedding.
package db

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	_ "turso.tech/database/tursogo"
)

// Dim is the fixed embedding dimension stored in the docs.embedding column.
// Must stay in sync with internal/embed.Dim. Duplicated here to keep the
// package graph one-way (db does not import embed).
const Dim = 64

// Doc represents a documentation snippet stored in the docs table.
type Doc struct {
	LibID   string
	Title   string
	Content string
}

// Open opens (or creates) a local turso database at path and ensures the
// docs schema is in place. tursogo's DSN is a bare path — the "file:"
// prefix used by libSQL is NOT stripped and would create a file literally
// named "file:<path>".
func Open(path string) (*sql.DB, error) {
	d, err := sql.Open("turso", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// tursogo is BETA; serialize connections defensively to avoid any
	// potential driver-level races. Harmless for the MCP server's
	// one-query-at-a-time workload.
	// TODO: revisit once tursogo reaches a stable release.
	d.SetMaxOpenConns(1)

	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS docs (
		id INTEGER PRIMARY KEY,
		lib_id TEXT NOT NULL,
		title TEXT NOT NULL,
		content TEXT NOT NULL,
		embedding F32_BLOB(64) NOT NULL
	)`); err != nil {
		return nil, fmt.Errorf("create docs table: %w", err)
	}
	if _, err := d.Exec(`CREATE INDEX IF NOT EXISTS idx_docs_lib_id ON docs(lib_id)`); err != nil {
		return nil, fmt.Errorf("create lib_id index: %w", err)
	}

	return d, nil
}

// Insert stores a Doc along with its precomputed embedding. The embedding
// must have exactly Dim components.
func Insert(db *sql.DB, doc Doc, embedding []float32) error {
	if len(embedding) != Dim {
		return fmt.Errorf("insert doc: embedding length %d, want %d", len(embedding), Dim)
	}
	_, err := db.Exec(
		`INSERT INTO docs(lib_id, title, content, embedding) VALUES (?, ?, ?, vector(?))`,
		doc.LibID, doc.Title, doc.Content, formatVector(embedding),
	)
	if err != nil {
		return fmt.Errorf("insert doc: %w", err)
	}
	return nil
}

// SearchByEmbedding returns the top-k Docs ranked by cosine distance to
// queryVec (lower = more similar). If libID is non-empty, results are
// filtered to that library. k defaults to 10 if <= 0.
func SearchByEmbedding(db *sql.DB, queryVec []float32, libID string, k int) ([]Doc, error) {
	if len(queryVec) != Dim {
		return nil, fmt.Errorf("search: query vector length %d, want %d", len(queryVec), Dim)
	}
	if k <= 0 {
		k = 10
	}

	q := formatVector(queryVec)

	var (
		rows *sql.Rows
		err  error
	)
	if libID != "" {
		rows, err = db.Query(
			`SELECT lib_id, title, content
			 FROM docs
			 WHERE lib_id = ?
			 ORDER BY vector_distance_cos(embedding, vector(?)) ASC
			 LIMIT ?`,
			libID, q, k,
		)
	} else {
		rows, err = db.Query(
			`SELECT lib_id, title, content
			 FROM docs
			 ORDER BY vector_distance_cos(embedding, vector(?)) ASC
			 LIMIT ?`,
			q, k,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var results []Doc
	for rows.Next() {
		var d Doc
		if err := rows.Scan(&d.LibID, &d.Title, &d.Content); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, d)
	}
	return results, rows.Err()
}

// formatVector encodes a []float32 as a JSON array literal understood by
// turso's vector() constructor: "[0.1,0.2,0.3]". Safe to pass as a bound
// TEXT parameter to `vector(?)` — verified by TestFormatVector_Roundtrip.
func formatVector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 8)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
