package packs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laradji/deadzone/internal/packs"
)

// validPackYAML is a fixture body for a single well-formed pack entry.
// Reused by every "happy path" test below.
const validPackYAML = `release_tag: packs
repo: laradji/deadzone
packs:
  - lib_id: /modelcontextprotocol/go-sdk
    asset: modelcontextprotocol_go-sdk.db
    sha256: 9f2e8c4b1a0d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f
    size: 152834
    indexed_at: 2026-04-10T16:23:00Z
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

func TestLoad_ValidSinglePack(t *testing.T) {
	m, err := packs.Load(writeManifest(t, validPackYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.ReleaseTag != "packs" {
		t.Errorf("ReleaseTag = %q, want %q", m.ReleaseTag, "packs")
	}
	if m.Repo != "laradji/deadzone" {
		t.Errorf("Repo = %q, want %q", m.Repo, "laradji/deadzone")
	}
	if len(m.Packs) != 1 {
		t.Fatalf("len(Packs) = %d, want 1", len(m.Packs))
	}
	got := m.Packs[0]
	if got.LibID != "/modelcontextprotocol/go-sdk" {
		t.Errorf("LibID = %q", got.LibID)
	}
	if got.Asset != "modelcontextprotocol_go-sdk.db" {
		t.Errorf("Asset = %q", got.Asset)
	}
	if got.Size != 152834 {
		t.Errorf("Size = %d", got.Size)
	}
	wantTime := time.Date(2026, 4, 10, 16, 23, 0, 0, time.UTC)
	if !got.IndexedAt.Equal(wantTime) {
		t.Errorf("IndexedAt = %v, want %v", got.IndexedAt, wantTime)
	}
}

func TestLoad_EmptyPacksIsValid(t *testing.T) {
	m, err := packs.Load(writeManifest(t, "release_tag: packs\nrepo: laradji/deadzone\npacks: []\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Packs) != 0 {
		t.Errorf("len(Packs) = %d, want 0", len(m.Packs))
	}
}

func TestLoad_RejectsMissingReleaseTag(t *testing.T) {
	_, err := packs.Load(writeManifest(t, "packs: []\n"))
	if err == nil || !strings.Contains(err.Error(), "release_tag") {
		t.Fatalf("got %v, want error mentioning release_tag", err)
	}
}

func TestLoad_RejectsBadSHA256(t *testing.T) {
	bad := strings.Replace(validPackYAML, "9f2e8c4b1a0d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f", "deadbeef", 1)
	_, err := packs.Load(writeManifest(t, bad))
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("got %v, want sha256 validation error", err)
	}
}

func TestLoad_RejectsZeroSize(t *testing.T) {
	bad := strings.Replace(validPackYAML, "size: 152834", "size: 0", 1)
	_, err := packs.Load(writeManifest(t, bad))
	if err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("got %v, want size validation error", err)
	}
}

func TestLoad_RejectsLibIDWithoutSlash(t *testing.T) {
	bad := strings.Replace(validPackYAML, "/modelcontextprotocol/go-sdk", "modelcontextprotocol/go-sdk", 1)
	_, err := packs.Load(writeManifest(t, bad))
	if err == nil || !strings.Contains(err.Error(), `must start with "/"`) {
		t.Fatalf("got %v, want lib_id slash error", err)
	}
}

func TestLoad_RejectsDuplicateLibID(t *testing.T) {
	dup := `release_tag: packs
packs:
  - lib_id: /x/y
    asset: x_y.db
    sha256: 0000000000000000000000000000000000000000000000000000000000000001
    size: 1
    indexed_at: 2026-04-10T16:23:00Z
  - lib_id: /x/y
    asset: x_y_v2.db
    sha256: 0000000000000000000000000000000000000000000000000000000000000002
    size: 2
    indexed_at: 2026-04-10T16:23:00Z
`
	_, err := packs.Load(writeManifest(t, dup))
	if err == nil || !strings.Contains(err.Error(), "duplicate lib_id") {
		t.Fatalf("got %v, want duplicate lib_id error", err)
	}
}

func TestLoad_RejectsDuplicateAsset(t *testing.T) {
	dup := `release_tag: packs
packs:
  - lib_id: /a/one
    asset: same.db
    sha256: 0000000000000000000000000000000000000000000000000000000000000001
    size: 1
    indexed_at: 2026-04-10T16:23:00Z
  - lib_id: /a/two
    asset: same.db
    sha256: 0000000000000000000000000000000000000000000000000000000000000002
    size: 2
    indexed_at: 2026-04-10T16:23:00Z
`
	_, err := packs.Load(writeManifest(t, dup))
	if err == nil || !strings.Contains(err.Error(), "duplicate asset") {
		t.Fatalf("got %v, want duplicate asset error", err)
	}
}

func TestSaveLoad_RoundTripSortsPacks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")

	m := &packs.Manifest{
		ReleaseTag: "packs",
		Repo:       "laradji/deadzone",
		Packs: []packs.Pack{
			// Intentionally out of order so we can verify Save sorts.
			{
				LibID:     "/zzz/last",
				Asset:     "zzz_last.db",
				SHA256:    "0000000000000000000000000000000000000000000000000000000000000003",
				Size:      3,
				IndexedAt: time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC),
			},
			{
				LibID:     "/aaa/first",
				Asset:     "aaa_first.db",
				SHA256:    "0000000000000000000000000000000000000000000000000000000000000001",
				Size:      1,
				IndexedAt: time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC),
			},
		},
	}
	if err := m.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := packs.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Packs) != 2 {
		t.Fatalf("len(Packs) = %d, want 2", len(loaded.Packs))
	}
	// First entry on disk should be the lex-smallest lib_id.
	if loaded.Packs[0].LibID != "/aaa/first" {
		t.Errorf("Packs[0].LibID = %q, want /aaa/first", loaded.Packs[0].LibID)
	}
	if loaded.Packs[1].LibID != "/zzz/last" {
		t.Errorf("Packs[1].LibID = %q, want /zzz/last", loaded.Packs[1].LibID)
	}

	// And that no temp file leaked.
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
	m := &packs.Manifest{ReleaseTag: ""} // missing release_tag
	if err := m.Save(path); err == nil {
		t.Fatal("Save accepted manifest with empty release_tag")
	}
}

func TestFind(t *testing.T) {
	m, err := packs.Load(writeManifest(t, validPackYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, idx, ok := m.Find("/modelcontextprotocol/go-sdk")
	if !ok {
		t.Fatal("Find returned ok=false")
	}
	if idx != 0 {
		t.Errorf("idx = %d, want 0", idx)
	}
	if got.LibID != "/modelcontextprotocol/go-sdk" {
		t.Errorf("LibID = %q", got.LibID)
	}

	if _, _, ok := m.Find("/missing/lib"); ok {
		t.Error("Find of missing lib returned ok=true")
	}
}

func TestReplace_Updates(t *testing.T) {
	m, err := packs.Load(writeManifest(t, validPackYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	updated := m.Packs[0]
	updated.SHA256 = "1111111111111111111111111111111111111111111111111111111111111111"
	updated.Size = 999
	m.Replace(updated)
	if len(m.Packs) != 1 {
		t.Errorf("len(Packs) = %d, want 1 (Replace should update in place)", len(m.Packs))
	}
	if m.Packs[0].Size != 999 {
		t.Errorf("Size = %d, want 999", m.Packs[0].Size)
	}
}

func TestReplace_Appends(t *testing.T) {
	m, err := packs.Load(writeManifest(t, validPackYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m.Replace(packs.Pack{
		LibID:     "/new/lib",
		Asset:     "new_lib.db",
		SHA256:    "2222222222222222222222222222222222222222222222222222222222222222",
		Size:      42,
		IndexedAt: time.Now().UTC(),
	})
	if len(m.Packs) != 2 {
		t.Errorf("len(Packs) = %d, want 2", len(m.Packs))
	}
}

func TestFilterByLibID(t *testing.T) {
	multi := `release_tag: packs
packs:
  - lib_id: /facebook/react/v18
    asset: facebook_react_v18.db
    sha256: 0000000000000000000000000000000000000000000000000000000000000001
    size: 1
    indexed_at: 2026-04-10T16:23:00Z
  - lib_id: /facebook/react/v19
    asset: facebook_react_v19.db
    sha256: 0000000000000000000000000000000000000000000000000000000000000002
    size: 2
    indexed_at: 2026-04-10T16:23:00Z
  - lib_id: /facebook/reactother
    asset: facebook_reactother.db
    sha256: 0000000000000000000000000000000000000000000000000000000000000003
    size: 3
    indexed_at: 2026-04-10T16:23:00Z
  - lib_id: /unrelated/lib
    asset: unrelated_lib.db
    sha256: 0000000000000000000000000000000000000000000000000000000000000004
    size: 4
    indexed_at: 2026-04-10T16:23:00Z
`
	m, err := packs.Load(writeManifest(t, multi))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Empty filter → all packs.
	if got := m.FilterByLibID(""); len(got) != 4 {
		t.Errorf("FilterByLibID(\"\") returned %d packs, want 4", len(got))
	}

	// Base lib_id matches both versioned children but NOT /facebook/reactother.
	got := m.FilterByLibID("/facebook/react")
	if len(got) != 2 {
		t.Errorf("FilterByLibID(/facebook/react) returned %d packs, want 2", len(got))
	}
	for _, p := range got {
		if p.LibID == "/facebook/reactother" {
			t.Errorf("filter incorrectly matched %q against base /facebook/react", p.LibID)
		}
	}

	// Exact versioned filter matches one entry.
	got = m.FilterByLibID("/facebook/react/v18")
	if len(got) != 1 || got[0].LibID != "/facebook/react/v18" {
		t.Errorf("FilterByLibID(/facebook/react/v18) returned %v, want exactly v18", got)
	}

	// No match.
	if got := m.FilterByLibID("/totally/missing"); len(got) != 0 {
		t.Errorf("FilterByLibID(/totally/missing) returned %d, want 0", len(got))
	}
}
