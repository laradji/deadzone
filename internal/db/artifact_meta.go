package db

import (
	"database/sql"
	"fmt"

	_ "turso.tech/database/tursogo"
)

// ArtifactMeta is the identity payload of a per-lib artifact: which
// library it carries and which embedder produced its vectors. The
// per-pack manifest committed to git records exactly these three fields
// alongside the sha256, so a future cmd/packs upload can recreate the
// manifest entry from a freshly-built artifact without having to spin
// up an embedder.
type ArtifactMeta struct {
	LibID        string
	EmbedderKind string
	ModelVersion string
}

// ReadArtifactMeta opens path with a bare sql.Open, reads the three
// identity keys from the meta table, and closes. Unlike OpenArtifact it
// does NOT validate the embedder meta against a Meta passed by the
// caller and does NOT cross-check the schema version, so callers can
// inspect an artifact without instantiating an embedder (and paying the
// ~90MB MiniLM model download on first run).
//
// The trade-off: ReadArtifactMeta only tells you what the artifact
// claims to be. The integrity contract that those vectors actually
// match the embedder is enforced exactly once, by db.Open / OpenArtifact,
// at the moment a real consumer (consolidate, server) loads the file.
// cmd/packs is intentionally upstream of that check — it just shovels
// bytes around.
//
// Returns ErrArtifactLibIDMissing when the file exists but has no
// lib_id row, mirroring OpenArtifact's behaviour for the same case so
// callers using errors.Is don't have to learn a second sentinel.
func ReadArtifactMeta(path string) (ArtifactMeta, error) {
	raw, err := sql.Open("turso", path)
	if err != nil {
		return ArtifactMeta{}, fmt.Errorf("read artifact meta %s: open: %w", path, err)
	}
	defer raw.Close()
	// Match the rest of the package: tursogo is BETA, serialize.
	raw.SetMaxOpenConns(1)

	rows, err := raw.Query(`SELECT key, value FROM meta WHERE key IN (?, ?, ?)`,
		metaKeyLibID, metaKeyEmbedderKind, metaKeyModelVersion)
	if err != nil {
		return ArtifactMeta{}, fmt.Errorf("read artifact meta %s: query: %w", path, err)
	}
	defer rows.Close()

	values := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return ArtifactMeta{}, fmt.Errorf("read artifact meta %s: scan: %w", path, err)
		}
		values[k] = v
	}
	if err := rows.Err(); err != nil {
		return ArtifactMeta{}, fmt.Errorf("read artifact meta %s: rows: %w", path, err)
	}

	libID, hasLibID := values[metaKeyLibID]
	if !hasLibID {
		return ArtifactMeta{}, fmt.Errorf("read artifact meta %s: %w", path, ErrArtifactLibIDMissing)
	}
	// Embedder meta is intentionally optional at this layer: a brand-new
	// artifact built by a future scraper version that drops one of these
	// keys would still be uploadable, just with empty fields in the
	// manifest. The downstream Open call enforces the real contract.
	return ArtifactMeta{
		LibID:        libID,
		EmbedderKind: values[metaKeyEmbedderKind],
		ModelVersion: values[metaKeyModelVersion],
	}, nil
}
