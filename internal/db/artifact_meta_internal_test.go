package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "turso.tech/database/tursogo"
)

// TestReadArtifactMeta verifies the lightweight reader correctly extracts
// the three identity fields from a hand-built artifact file. We bypass
// db.Open / OpenArtifact entirely so the test runs without an embedder
// (proving the helper's main value proposition: cmd/packs can read an
// artifact without paying the ~90MB MiniLM model download cost).
func TestReadArtifactMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact.db")
	writeFakeMeta(t, path, map[string]string{
		metaKeyLibID:        "/modelcontextprotocol/go-sdk",
		metaKeyEmbedderKind: "hugot",
		metaKeyModelVersion: "sentence-transformers/all-MiniLM-L6-v2",
		// schema_version + embedding_dim are intentionally NOT written;
		// ReadArtifactMeta must not require them.
	})

	got, err := ReadArtifactMeta(path)
	if err != nil {
		t.Fatalf("ReadArtifactMeta: %v", err)
	}
	want := ArtifactMeta{
		LibID:        "/modelcontextprotocol/go-sdk",
		EmbedderKind: "hugot",
		ModelVersion: "sentence-transformers/all-MiniLM-L6-v2",
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestReadArtifactMeta_PartialMetaIsTolerated covers the case where the
// artifact's meta table has lib_id but is missing the embedder identity
// keys. The reader should still succeed (with empty fields), so a future
// scraper version that drops one of those keys doesn't break uploads.
// The downstream Open call enforces the real embedder contract; this
// helper is intentionally permissive.
func TestReadArtifactMeta_PartialMetaIsTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact.db")
	writeFakeMeta(t, path, map[string]string{
		metaKeyLibID: "/x/y",
	})

	got, err := ReadArtifactMeta(path)
	if err != nil {
		t.Fatalf("ReadArtifactMeta: %v", err)
	}
	if got.LibID != "/x/y" {
		t.Errorf("LibID = %q, want %q", got.LibID, "/x/y")
	}
	if got.EmbedderKind != "" {
		t.Errorf("EmbedderKind = %q, want empty", got.EmbedderKind)
	}
	if got.ModelVersion != "" {
		t.Errorf("ModelVersion = %q, want empty", got.ModelVersion)
	}
}

// TestReadArtifactMeta_MissingLibIDFails covers the structural-corruption
// path: an artifact whose meta table exists but has no lib_id row is
// malformed by construction (every artifact gets its lib_id stamped at
// creation time, see OpenArtifact). The reader returns the same sentinel
// the rest of the package uses so callers can handle both code paths
// with one errors.Is check.
func TestReadArtifactMeta_MissingLibIDFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact.db")
	writeFakeMeta(t, path, map[string]string{
		metaKeyEmbedderKind: "hugot",
		metaKeyModelVersion: "v1",
	})

	_, err := ReadArtifactMeta(path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrArtifactLibIDMissing) {
		t.Errorf("err = %v, want ErrArtifactLibIDMissing", err)
	}
}

// writeFakeMeta builds a minimal artifact file by hand: a fresh turso
// database containing only a meta table with the supplied rows. No
// docs, no libs, no embedder. The file is created at path; t.TempDir
// guarantees cleanup.
func writeFakeMeta(t *testing.T, path string, rows map[string]string) {
	t.Helper()
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)

	if _, err := raw.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create meta: %v", err)
	}
	for k, v := range rows {
		if _, err := raw.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatalf("insert %s: %v", k, err)
		}
	}
}
