package packs

// state.go implements the per-artifact `<lib>.db.state` YAML sidecar
// introduced by #96.
//
// The sidecar carries metadata about an artifact's *contents* (embedder
// identity, schema version, scrape lifecycle dates, doc counts). The
// manifest.yaml entry next to it carries metadata about the *upload
// event* (sha256, size, indexed_at). Keeping the two concepts in
// separate files is the whole point of the split — don't conflate
// them.
//
// Lifecycle, as implemented here and in cmd/scraper/main.go:
//
//   - The scraper reads the existing `.state` (if any) BEFORE wiping
//     the `.db` + WAL/SHM sidecars, to capture `created_at` so it
//     survives a re-scrape.
//   - After a lib finishes successfully (after UpdateLibCount) the
//     scraper writes a fresh `.state` with `created_at` preserved (or
//     set to now on first scrape) and `updated_at = now`.
//   - On lib failure the `.state` is NOT rewritten. The pre-existing
//     `.state` stays in place even though the `.db` was wiped; an
//     operator re-running the scrape overwrites both. This is a
//     documented trade-off (see the scraper call-site).
//   - `packs upload` ships the `.state` as a release asset alongside
//     the `.db`, and `packs download` fetches both. `packs list`
//     reads the downloaded `.state` to surface rich columns.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// StateFile is the deserialized shape of a `<lib>.db.state` YAML
// sidecar. Field order is preserved on marshal via yaml.v3 tag
// declaration order, so diffs in operator-visible `.state` files land
// in a predictable shape.
type StateFile struct {
	LibID         string        `yaml:"lib_id"`
	SchemaVersion int           `yaml:"schema_version"`
	Embedder      EmbedderState `yaml:"embedder"`
	CreatedAt     time.Time     `yaml:"created_at"`
	UpdatedAt     time.Time     `yaml:"updated_at"`
	URLCount      int           `yaml:"url_count"`
	DocCount      int           `yaml:"doc_count"`
}

// EmbedderState mirrors the embedder identity triple the scraper also
// writes into `db.Meta`. Kept structurally separate from db.Meta so the
// packs package does not pull in the db package for this tiny struct.
type EmbedderState struct {
	Kind  string `yaml:"kind"`
	Model string `yaml:"model"`
	Dim   int    `yaml:"dim"`
}

// StatePath returns the canonical sidecar path for an artifact `.db`
// path: the same path with a `.state` suffix appended. Keeping the
// `.db` prefix ensures `ls artifacts/` groups each pair next to each
// other and a glob like `*.db*` picks up both.
func StatePath(dbPath string) string { return dbPath + ".state" }

// LoadState reads and parses a sidecar. A missing file is surfaced as
// an `os.IsNotExist`-detectable error so callers can branch cleanly
// ("first scrape? use time.Now() for created_at").
func LoadState(path string) (*StateFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s StateFile
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	return &s, nil
}

// Save writes the sidecar atomically via temp-file + rename in the
// same directory, matching the pattern used by Manifest.Save.
func (s *StateFile) Save(path string) error {
	data, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal state %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".state-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create tmp state in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tmp state %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp state %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}
