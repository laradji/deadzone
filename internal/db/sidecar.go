package db

// sidecar.go owns the small JSON manifest that lives next to a cached
// deadzone.db. The file's sole job is answering, at boot, three
// questions cheaply:
//
//   1. Which release tag does the cached DB belong to? (already needed
//      by the version-pin contract from #108).
//   2. What's the sha256 of the cached DB? (needed by the auto-update
//      probe from #197 — comparing the local sha to the remote
//      deadzone.db.sha256 must not cost a 50 MB rehash on every boot).
//   3. When did we last fetch it? (operator visibility / audit).
//
// Format evolution: v0 was a single line containing just the tag (e.g.
// "v0.6.0\n"). v1 is the JSON object below. The reader accepts both,
// so a binary upgrade onto a v0 cache reads the legacy tag and treats
// the missing sha256 as "first probe ever — recompute, then rewrite".
// The writer always emits v1.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sidecar is the on-disk manifest companion to deadzone.db. JSON tags
// are snake_case to align with the slog field convention used elsewhere
// in internal/db.
type sidecar struct {
	Tag        string    `json:"tag"`
	SHA256     string    `json:"sha256,omitempty"`
	FetchedAt  time.Time `json:"fetched_at,omitempty"`
}

// readSidecar parses the manifest at path. Order matters: we try JSON
// first because the v1 format is what the writer emits today; only on
// JSON parse failure do we fall back to the v0 single-line tag. Doing
// it the other way around would mis-classify a valid JSON sidecar as a
// literal tag (the brace would parse as a tag string and never trip
// the JSON path).
//
// A non-existent file returns os.ErrNotExist so callers can use
// errors.Is to distinguish "no cache yet" from "cache present but
// unreadable".
func readSidecar(path string) (sidecar, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return sidecar{}, err
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return sidecar{}, fmt.Errorf("sidecar %s is empty", path)
	}

	var s sidecar
	if err := json.Unmarshal([]byte(trimmed), &s); err == nil && s.Tag != "" {
		return s, nil
	}

	// Legacy v0: a single non-JSON line that IS the tag. Anything more
	// complex (multi-line, JSON-like-but-broken) is a corruption we
	// surface rather than guess at.
	if strings.ContainsAny(trimmed, "{}\n") {
		return sidecar{}, fmt.Errorf("sidecar %s: unrecognised format", path)
	}
	return sidecar{Tag: trimmed}, nil
}

// writeSidecar serialises s as pretty-printed JSON and atomically
// renames it into place. Atomicity matters because the sidecar now
// carries a sha256 the auto-update probe trusts: a torn write that
// leaves "tag" set but "sha256" truncated would cause the next boot
// to compute a fresh sha and rewrite, which is fine functionally but
// wastes ~50 MB of disk read on the path that's supposed to be cheap.
//
// The temp file is created in the destination's directory so os.Rename
// is a true rename (same filesystem) rather than a cross-device copy.
func writeSidecar(path string, s sidecar) error {
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sidecar: %w", err)
	}
	body = append(body, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create sidecar tempfile in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("write sidecar tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close sidecar tempfile: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename sidecar %s -> %s: %w", tmpPath, path, err)
	}
	cleanup = false
	return nil
}
