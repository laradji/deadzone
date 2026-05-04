package db

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSidecar_RoundTripV1 is the bread-and-butter case: write a v1
// JSON sidecar, read it back, all three fields survive the round-trip.
// FetchedAt is compared via Equal so a marshalled-then-parsed timestamp
// (which loses monotonic clock and may shift between Local and UTC
// representations) still matches the original.
func TestSidecar_RoundTripV1(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deadzone.db.release")
	want := sidecar{
		Tag:       "v0.6.0",
		SHA256:    "3a7f9e2b4c5d6f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f50",
		FetchedAt: time.Date(2026, 5, 4, 18, 23, 11, 0, time.UTC),
	}
	if err := writeSidecar(path, want); err != nil {
		t.Fatalf("writeSidecar: %v", err)
	}
	got, err := readSidecar(path)
	if err != nil {
		t.Fatalf("readSidecar: %v", err)
	}
	if got.Tag != want.Tag {
		t.Errorf("Tag = %q, want %q", got.Tag, want.Tag)
	}
	if got.SHA256 != want.SHA256 {
		t.Errorf("SHA256 = %q, want %q", got.SHA256, want.SHA256)
	}
	if !got.FetchedAt.Equal(want.FetchedAt) {
		t.Errorf("FetchedAt = %v, want %v", got.FetchedAt, want.FetchedAt)
	}
}

// TestSidecar_LegacyV0Read covers the "binary just upgraded over a v0
// cache" path: pre-#197 binaries wrote a single-line tag. The new
// reader must surface the tag with empty SHA256/FetchedAt so the
// auto-update probe can take the "compute sha local + rewrite" branch.
func TestSidecar_LegacyV0Read(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deadzone.db.release")
	if err := os.WriteFile(path, []byte("v0.5.0\n"), 0o644); err != nil {
		t.Fatalf("seed legacy sidecar: %v", err)
	}
	got, err := readSidecar(path)
	if err != nil {
		t.Fatalf("readSidecar: %v", err)
	}
	if got.Tag != "v0.5.0" {
		t.Errorf("Tag = %q, want %q", got.Tag, "v0.5.0")
	}
	if got.SHA256 != "" {
		t.Errorf("SHA256 = %q on legacy sidecar, want empty", got.SHA256)
	}
	if !got.FetchedAt.IsZero() {
		t.Errorf("FetchedAt = %v on legacy sidecar, want zero", got.FetchedAt)
	}
}

// TestSidecar_LegacyV0NoTrailingNewline covers a hand-crafted legacy
// sidecar without the trailing newline old binaries wrote. The reader
// trims whitespace so both forms parse identically.
func TestSidecar_LegacyV0NoTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deadzone.db.release")
	if err := os.WriteFile(path, []byte("v0.5.0"), 0o644); err != nil {
		t.Fatalf("seed legacy sidecar: %v", err)
	}
	got, err := readSidecar(path)
	if err != nil {
		t.Fatalf("readSidecar: %v", err)
	}
	if got.Tag != "v0.5.0" {
		t.Errorf("Tag = %q, want %q", got.Tag, "v0.5.0")
	}
}

// TestSidecar_MissingFieldsTolerated covers a partially-populated v1
// sidecar (e.g. written by a future binary that drops a field, or a
// hand-edited file). Missing optional fields fall back to their zero
// values; missing the required Tag field is an error so we don't end
// up keying the cache off an empty string.
func TestSidecar_MissingFieldsTolerated(t *testing.T) {
	dir := t.TempDir()

	// Tag-only v1 — sha256/fetched_at omitted. Must succeed: the
	// auto-update probe will treat this as "first probe ever", same
	// as the legacy v0 path.
	tagOnly := filepath.Join(dir, "tag-only.json")
	if err := os.WriteFile(tagOnly, []byte(`{"tag":"v0.6.0"}`), 0o644); err != nil {
		t.Fatalf("seed tag-only: %v", err)
	}
	got, err := readSidecar(tagOnly)
	if err != nil {
		t.Fatalf("readSidecar(tag-only): %v", err)
	}
	if got.Tag != "v0.6.0" || got.SHA256 != "" || !got.FetchedAt.IsZero() {
		t.Errorf("tag-only parse = %+v, want only Tag set", got)
	}

	// Empty object — no Tag means the file can't usefully key the
	// cache; readSidecar must error rather than return an empty Tag
	// that the caller might mistake for a cache hit.
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("seed empty: %v", err)
	}
	if _, err := readSidecar(empty); err == nil {
		t.Errorf("readSidecar(empty object) returned nil error, want error")
	}
}

// TestSidecar_CorruptedRejected covers torn-write / hostile-edit
// scenarios. A truncated JSON object must NOT silently fall back to
// the legacy parser and get classified as a literal tag — that would
// poison the cache lookup with a giant unparseable "tag" value. The
// reader's strict guard refuses anything containing JSON syntax that
// fails to parse cleanly.
func TestSidecar_CorruptedRejected(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
	}{
		{"truncated-json", `{"tag":"v0.6.0"`},
		{"multi-line-non-json", "v0.6.0\nextra\n"},
		{"only-newlines", "\n\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".release")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if _, err := readSidecar(path); err == nil {
				t.Errorf("readSidecar accepted corrupted content %q", tc.content)
			}
		})
	}
}

// TestSidecar_NonExistent covers the first-fetch path: no sidecar yet.
// The error must wrap os.ErrNotExist so Bootstrap can errors.Is its way
// to the "no cache" branch rather than treating it as a corruption.
func TestSidecar_NonExistent(t *testing.T) {
	dir := t.TempDir()
	_, err := readSidecar(filepath.Join(dir, "missing.release"))
	if err == nil {
		t.Fatal("readSidecar on missing file returned nil error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error %q does not wrap os.ErrNotExist", err)
	}
}

// TestSidecar_WriteIsAtomic asserts that no .tmp-* file is left behind
// after a successful write. This pins the os.Rename pattern: a future
// refactor that switches to non-atomic write-then-rename or forgets to
// clean up on the failure path would regress here.
func TestSidecar_WriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deadzone.db.release")
	s := sidecar{Tag: "v0.6.0", SHA256: strings.Repeat("a", 64), FetchedAt: time.Now().UTC()}
	if err := writeSidecar(path, s); err != nil {
		t.Fatalf("writeSidecar: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("tempfile %q left behind after successful write", e.Name())
		}
	}
}
