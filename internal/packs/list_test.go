package packs_test

import (
	"bytes"
	"strings"
	"testing"

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
