package packs_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laradji/deadzone/internal/packs"
)

func TestList_PrintsHeaderAndRows(t *testing.T) {
	manifestPath := writeManifest(t, validPackYAML)

	var buf bytes.Buffer
	if err := packs.RunList(packs.ListOptions{ManifestPath: manifestPath}, &buf); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "LIB_ID") || !strings.Contains(out, "ASSET") || !strings.Contains(out, "SHA256") {
		t.Errorf("missing header columns:\n%s", out)
	}
	if !strings.Contains(out, "/modelcontextprotocol/go-sdk") {
		t.Errorf("missing data row:\n%s", out)
	}
	// SHA256 prefix should be the first 12 chars (9f2e8c4b1a0d), not the full 64.
	if !strings.Contains(out, "9f2e8c4b1a0d") {
		t.Errorf("missing 12-char sha256 prefix:\n%s", out)
	}
	if strings.Contains(out, "9f2e8c4b1a0d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f") {
		t.Errorf("full sha256 leaked into output instead of prefix:\n%s", out)
	}
}

func TestList_ShowsStateColumns(t *testing.T) {
	manifestPath := writeManifest(t, validPackYAML)
	dir := filepath.Dir(manifestPath)
	state := &packs.StateFile{
		LibID:         "/modelcontextprotocol/go-sdk",
		SchemaVersion: 3,
		Embedder:      packs.EmbedderState{Kind: "hugot", Model: "nomic-embed", Dim: 768},
		CreatedAt:     time.Date(2026, 4, 12, 14, 0, 0, 0, time.UTC),
		UpdatedAt:     time.Date(2026, 4, 13, 14, 32, 0, 0, time.UTC),
		URLCount:      6,
		DocCount:      42,
	}
	if err := state.Save(filepath.Join(dir, "modelcontextprotocol_go-sdk.db.state")); err != nil {
		t.Fatalf("save state: %v", err)
	}

	var buf bytes.Buffer
	if err := packs.RunList(packs.ListOptions{ManifestPath: manifestPath, ArtifactsDir: dir}, &buf); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"EMBEDDER", "DOCS", "UPDATED_AT", "hugot nomic-embed", "42", "2026-04-13T14:32:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestList_MissingStateShowsEmDash(t *testing.T) {
	manifestPath := writeManifest(t, validPackYAML)
	dir := filepath.Dir(manifestPath)
	// Deliberately do NOT write a sidecar.

	var buf bytes.Buffer
	if err := packs.RunList(packs.ListOptions{ManifestPath: manifestPath, ArtifactsDir: dir}, &buf); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "—") {
		t.Errorf("expected em-dash placeholder for missing state:\n%s", out)
	}
}

func TestList_EmptyManifest(t *testing.T) {
	manifestPath := writeManifest(t, "release_tag: packs\nrepo: laradji/deadzone\npacks: []\n")
	var buf bytes.Buffer
	if err := packs.RunList(packs.ListOptions{ManifestPath: manifestPath}, &buf); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	if !strings.Contains(buf.String(), "(no packs in manifest)") {
		t.Errorf("expected empty marker, got:\n%s", buf.String())
	}
}
