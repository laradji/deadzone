package packs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// FileSHA256 returns the lowercase hex sha256 digest of the file at
// path. The file is streamed through io.Copy into the hasher so a 1 GB
// pack costs the same memory as a 1 KB one — important because vector
// artifacts grow over time and the verify path is on the hot loop of
// every download.
//
// The returned digest is exactly 64 characters long, matching the
// format Manifest validation expects, so callers can shuttle the result
// directly into a Pack.SHA256 field without normalization.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("sha256 %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("sha256 %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
