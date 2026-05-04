package packs

// state.go implements the per-lib `state.yaml` YAML sidecar introduced
// by #96 and moved to `<artifactsDir>/<slug>/state.yaml` by #101.
//
// The sidecar carries metadata about an artifact's *contents* (embedder
// identity, schema version, scrape lifecycle dates, doc counts). The
// top-level artifacts/manifest.yaml carries metadata about the
// *release event* (sha256, size, indexed_at of the consolidated
// deadzone.db). Keeping the two concepts in separate files is the
// whole point of the split — don't conflate them.
//
// Lifecycle, as implemented here and in cmd/deadzone/scrape.go:
//
//   - The scraper reads the existing `state.yaml` (if any) BEFORE
//     wiping the `artifact.db` + WAL/SHM sidecars, to capture
//     `created_at` so it survives a re-scrape.
//   - After a lib finishes successfully (after UpdateLibCount) the
//     scraper writes a fresh `state.yaml` with `created_at` preserved
//     (or set to now on first scrape) and `updated_at = now`.
//   - On lib failure the `state.yaml` is NOT rewritten. The pre-existing
//     sidecar stays in place even though the `artifact.db` was wiped;
//     an operator re-running the scrape overwrites both. This is a
//     documented trade-off (see the scraper call-site).
//
// The path helpers (`StatePath`, `ArtifactDir`, `ArtifactDBPath`,
// `Slug`) live in paths.go.

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
	LibID string `yaml:"lib_id"`
	// Version is the per-artifact version tag introduced by #113.
	// Empty string is the canonical single-version form; multi-version
	// libs carry the version as written in the registry (e.g.
	// "v1.14"). Serialized with omitempty so single-version sidecars
	// keep their pre-#113 shape on disk.
	Version       string        `yaml:"version,omitempty"`
	SchemaVersion int           `yaml:"schema_version"`
	Embedder      EmbedderState `yaml:"embedder"`
	// Ref is the resolved upstream git tag or commit SHA the URLs were
	// pinned to via the registry's `ref:` field (#103). Empty for libs
	// that have not opted into pinning yet — back-compat with pre-#103
	// state files.
	Ref       string    `yaml:"ref,omitempty"`
	CreatedAt time.Time `yaml:"created_at"`
	UpdatedAt time.Time `yaml:"updated_at"`
	URLCount  int       `yaml:"url_count"`
	DocCount  int       `yaml:"doc_count"`
	// GoToolchain is the runtime.Version() of the deadzone binary that
	// produced this artifact (#198). For kind: godoc artifacts the
	// chunk content is a function of go/doc's output, which is
	// stable per Go 1 compatibility but varies subtly in formatting
	// across releases — recording the toolchain lets an operator
	// correlate a chunk-shape regression with a deadzone build.
	// Empty for artifacts produced before this field was introduced.
	GoToolchain string `yaml:"go_toolchain,omitempty"`
}

// EmbedderState mirrors the embedder identity triple the scraper also
// writes into `db.Meta`. Kept structurally separate from db.Meta so the
// packs package does not pull in the db package for this tiny struct.
type EmbedderState struct {
	Kind  string `yaml:"kind"`
	Model string `yaml:"model"`
	Dim   int    `yaml:"dim"`
}

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
