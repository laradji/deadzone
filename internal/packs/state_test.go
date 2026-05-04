package packs_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laradji/deadzone/internal/packs"
)

func TestStateFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "x_y")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(libDir, "state.yaml")

	created := time.Date(2026, 4, 12, 14, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 4, 13, 14, 32, 0, 0, time.UTC)
	want := &packs.StateFile{
		LibID:         "/x/y",
		Version:       "v1.14",
		SchemaVersion: 4,
		Embedder: packs.EmbedderState{
			Kind:  "hugot",
			Model: "nomic-ai/nomic-embed-text-v1.5",
			Dim:   768,
		},
		Ref:         "v1.2.3",
		CreatedAt:   created,
		UpdatedAt:   updated,
		URLCount:    6,
		DocCount:    42,
		GoToolchain: "go1.26.2",
	}
	if err := want.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := packs.LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.LibID != want.LibID {
		t.Errorf("LibID = %q, want %q", got.LibID, want.LibID)
	}
	if got.Version != want.Version {
		t.Errorf("Version = %q, want %q", got.Version, want.Version)
	}
	if got.SchemaVersion != want.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, want.SchemaVersion)
	}
	if got.Embedder != want.Embedder {
		t.Errorf("Embedder = %+v, want %+v", got.Embedder, want.Embedder)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, want.UpdatedAt)
	}
	if got.URLCount != want.URLCount {
		t.Errorf("URLCount = %d, want %d", got.URLCount, want.URLCount)
	}
	if got.DocCount != want.DocCount {
		t.Errorf("DocCount = %d, want %d", got.DocCount, want.DocCount)
	}
	if got.Ref != want.Ref {
		t.Errorf("Ref = %q, want %q", got.Ref, want.Ref)
	}
	if got.GoToolchain != want.GoToolchain {
		t.Errorf("GoToolchain = %q, want %q", got.GoToolchain, want.GoToolchain)
	}
}

// TestStateFile_EmptyGoToolchainOmittedOnDisk pins the on-disk shape
// for sidecars produced by builds that don't (yet) set GoToolchain
// — and for non-godoc artifacts where the toolchain is informational.
// omitempty keeps pre-#198 sidecars stable and avoids a noisy
// `go_toolchain: ""` line when the field is unset.
func TestStateFile_EmptyGoToolchainOmittedOnDisk(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "x_y")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(libDir, "state.yaml")

	s := &packs.StateFile{
		LibID:         "/x/y",
		SchemaVersion: 5,
		Embedder:      packs.EmbedderState{Kind: "hugot", Model: "m", Dim: 8},
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "go_toolchain:") {
		t.Errorf("expected no go_toolchain: line when field is empty, got:\n%s", string(data))
	}
}

// TestStateFile_EmptyVersionOmittedOnDisk pins the on-disk shape
// canonical single-version sidecars keep: Version is serialized with
// omitempty so pre-#113 single-version sidecars don't grow a blank
// `version: ""` line just because the field now exists.
func TestStateFile_EmptyVersionOmittedOnDisk(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "x_y")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(libDir, "state.yaml")

	s := &packs.StateFile{
		LibID:         "/x/y",
		SchemaVersion: 4,
		Embedder:      packs.EmbedderState{Kind: "hugot", Model: "m", Dim: 8},
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Guard specifically against the top-level `version:` key — naive
	// strings.Contains("version:") would false-positive on
	// `schema_version:`.
	if strings.Contains("\n"+string(data), "\nversion:") {
		t.Errorf("expected no top-level version: line for single-version sidecar, got:\n%s", string(data))
	}
}

func TestStateFile_LoadMissingIsIsNotExist(t *testing.T) {
	_, err := packs.LoadState(filepath.Join(t.TempDir(), "nope.yaml"))
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want os.IsNotExist", err)
	}
}

func TestStateFile_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "x_y")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(libDir, "state.yaml")

	// Concurrent writers thrashing on the same destination should
	// never leave a partial / corrupt YAML behind. Each iteration
	// rewrites with a different doc_count so a torn write would
	// surface as a parse error or a doc_count outside the set we
	// know we wrote.
	const writers = 8
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		i := i
		go func() {
			defer wg.Done()
			s := &packs.StateFile{
				LibID:         "/x/y",
				SchemaVersion: 3,
				Embedder:      packs.EmbedderState{Kind: "hugot", Model: "m", Dim: 8},
				CreatedAt:     time.Now().UTC(),
				UpdatedAt:     time.Now().UTC(),
				URLCount:      1,
				DocCount:      i,
			}
			if err := s.Save(path); err != nil {
				t.Errorf("writer %d Save: %v", i, err)
			}
		}()
	}
	wg.Wait()

	got, err := packs.LoadState(path)
	if err != nil {
		t.Fatalf("LoadState after concurrent writes: %v", err)
	}
	if got.DocCount < 0 || got.DocCount >= writers {
		t.Errorf("DocCount = %d, want one of [0..%d)", got.DocCount, writers)
	}

	// And no .tmp file leaked.
	entries, err := os.ReadDir(libDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}
