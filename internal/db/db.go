package db

import (
	"database/sql"
	"fmt"

	_ "github.com/tursodatabase/go-libsql"
)

// Doc represents a documentation snippet stored in the FTS5 index.
type Doc struct {
	Lib     string
	Title   string
	Content string
}

// Open opens (or creates) a local libSQL database at path and initializes the FTS5 docs table.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("libsql", "file:"+path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	_, err = db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(
		lib, title, content, tokenize="unicode61"
	)`)
	if err != nil {
		return nil, fmt.Errorf("create fts5 table: %w", err)
	}

	return db, nil
}

// Insert adds a Doc into the FTS5 index.
func Insert(db *sql.DB, doc Doc) error {
	_, err := db.Exec(`INSERT INTO docs(lib, title, content) VALUES (?, ?, ?)`,
		doc.Lib, doc.Title, doc.Content)
	if err != nil {
		return fmt.Errorf("insert doc: %w", err)
	}
	return nil
}

// Search queries the FTS5 index. If lib is non-empty, results are filtered to that library.
func Search(db *sql.DB, query, lib string) ([]Doc, error) {
	var (
		rows *sql.Rows
		err  error
	)

	if lib != "" {
		rows, err = db.Query(
			`SELECT lib, title, content FROM docs WHERE docs MATCH ? AND lib = ? ORDER BY rank`,
			query, lib,
		)
	} else {
		rows, err = db.Query(
			`SELECT lib, title, content FROM docs WHERE docs MATCH ? ORDER BY rank`,
			query,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var results []Doc
	for rows.Next() {
		var d Doc
		if err := rows.Scan(&d.Lib, &d.Title, &d.Content); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, d)
	}
	return results, rows.Err()
}
