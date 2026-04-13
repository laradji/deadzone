package packs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laradji/deadzone/internal/packs"
)

// validReleaseYAML is a fixture body for a well-formed release record.
const validReleaseYAML = `release:
  tag: v0.1.0
  asset: deadzone.db
  sha256: 9f2e8c4b1a0d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f
  size: 234567890
  indexed_at: 2026-04-13T20:00:00Z
  embedder:
    kind: hugot
    model: nomic-ai/nomic-embed-text-v1.5
    dim: 768
  lib_count: 11
  doc_count: 1234
`

func writeManifest(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func TestLoad_ValidRelease(t *testing.T) {
	m, err := packs.Load(writeManifest(t, validReleaseYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := m.Release
	if r.Tag != "v0.1.0" {
		t.Errorf("Tag = %q", r.Tag)
	}
	if r.Asset != "deadzone.db" {
		t.Errorf("Asset = %q", r.Asset)
	}
	if r.Size != 234567890 {
		t.Errorf("Size = %d", r.Size)
	}
	wantTime := time.Date(2026, 4, 13, 20, 0, 0, 0, time.UTC)
	if !r.IndexedAt.Equal(wantTime) {
		t.Errorf("IndexedAt = %v, want %v", r.IndexedAt, wantTime)
	}
	if r.Embedder.Kind != "hugot" || r.Embedder.Dim != 768 {
		t.Errorf("Embedder = %+v", r.Embedder)
	}
	if r.LibCount != 11 || r.DocCount != 1234 {
		t.Errorf("LibCount/DocCount = %d/%d", r.LibCount, r.DocCount)
	}
}

func TestLoad_EmptyReleaseIsValid(t *testing.T) {
	m, err := packs.Load(writeManifest(t, "release:\n  tag: \"\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Release.Tag != "" {
		t.Errorf("Tag = %q, want empty", m.Release.Tag)
	}
}

func TestLoad_RejectsPartialRelease(t *testing.T) {
	// Non-zero size but empty tag → schema violation caught at load.
	body := `release:
  tag: ""
  asset: deadzone.db
  size: 100
`
	_, err := packs.Load(writeManifest(t, body))
	if err == nil || !strings.Contains(err.Error(), "release.tag is required") {
		t.Fatalf("got %v, want release.tag error", err)
	}
}

func TestLoad_RejectsBadSHA256(t *testing.T) {
	bad := strings.Replace(validReleaseYAML, "9f2e8c4b1a0d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f", "deadbeef", 1)
	_, err := packs.Load(writeManifest(t, bad))
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("got %v, want sha256 validation error", err)
	}
}

func TestLoad_RejectsZeroSize(t *testing.T) {
	bad := strings.Replace(validReleaseYAML, "size: 234567890", "size: 0", 1)
	_, err := packs.Load(writeManifest(t, bad))
	if err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("got %v, want size validation error", err)
	}
}

func TestLoad_RejectsMissingEmbedder(t *testing.T) {
	bad := strings.Replace(validReleaseYAML, "kind: hugot", "kind: \"\"", 1)
	_, err := packs.Load(writeManifest(t, bad))
	if err == nil || !strings.Contains(err.Error(), "embedder.kind") {
		t.Fatalf("got %v, want embedder.kind error", err)
	}
}

func TestLoad_RejectsMissingIndexedAt(t *testing.T) {
	bad := strings.Replace(validReleaseYAML, "indexed_at: 2026-04-13T20:00:00Z", "indexed_at: 0001-01-01T00:00:00Z", 1)
	_, err := packs.Load(writeManifest(t, bad))
	if err == nil || !strings.Contains(err.Error(), "indexed_at") {
		t.Fatalf("got %v, want indexed_at error", err)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")

	m := &packs.Manifest{
		Release: packs.ReleaseRecord{
			Tag:       "v0.1.0",
			Asset:     "deadzone.db",
			SHA256:    "1111111111111111111111111111111111111111111111111111111111111111",
			Size:      42,
			IndexedAt: time.Date(2026, 4, 13, 20, 0, 0, 0, time.UTC),
			Embedder: packs.EmbedderRecord{
				Kind:  "hugot",
				Model: "nomic-ai/nomic-embed-text-v1.5",
				Dim:   768,
			},
			LibCount: 11,
			DocCount: 1234,
		},
	}
	if err := m.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := packs.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Release.Tag != "v0.1.0" {
		t.Errorf("Tag = %q", loaded.Release.Tag)
	}
	if loaded.Release.SHA256 != m.Release.SHA256 {
		t.Errorf("SHA256 round-trip mismatch")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

func TestSave_RefusesInvalidManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	m := &packs.Manifest{
		Release: packs.ReleaseRecord{Tag: "v0.1.0", Size: 0}, // size=0 invalid when tag set
	}
	if err := m.Save(path); err == nil {
		t.Fatal("Save accepted manifest with zero size")
	}
}
