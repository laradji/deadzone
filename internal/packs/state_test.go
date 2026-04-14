package packs_test

import (
	"os"
	"path/filepath"
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
		SchemaVersion: 3,
		Embedder: packs.EmbedderState{
			Kind:  "hugot",
			Model: "nomic-ai/nomic-embed-text-v1.5",
			Dim:   768,
		},
		Ref:       "v1.2.3",
		CreatedAt: created,
		UpdatedAt: updated,
		URLCount:  6,
		DocCount:  42,
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
