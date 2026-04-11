package packs_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/laradji/deadzone/internal/packs"
)

// TestFileSHA256_KnownVector verifies the helper against the canonical
// "abc" → 0xba7816... vector from RFC 6234. If this ever fails, either
// crypto/sha256 has changed (won't happen) or the encoder produced a
// non-lowercase / non-hex result.
func TestFileSHA256_KnownVector(t *testing.T) {
	path := filepath.Join(t.TempDir(), "abc.bin")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := packs.FileSHA256(path)
	if err != nil {
		t.Fatalf("FileSHA256: %v", err)
	}
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestFileSHA256_EmptyFile verifies the empty-string SHA256 (the
// well-known e3b0c4... value), which is the case `packs upload` will
// hit if a scrape produces a zero-byte artifact for some reason.
func TestFileSHA256_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.bin")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := packs.FileSHA256(path)
	if err != nil {
		t.Fatalf("FileSHA256: %v", err)
	}
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFileSHA256_MissingFile(t *testing.T) {
	_, err := packs.FileSHA256(filepath.Join(t.TempDir(), "nope.bin"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
