// Package packs owns the local per-lib artifact layout (paths.go,
// state.go) and the manifest describing the most recent consolidated
// `deadzone.db` release (manifest.go).
//
// As of #101 the release model is operator-driven: `deadzone dbrelease`
// uploads `deadzone.db` + `deadzone.db.sha256` to an existing tagged
// GitHub Release (created by CI's release.yml when the tag was pushed)
// and rewrites this manifest to record the release event. The
// per-artifact upload/download/list flow is disabled; see the banners
// in upload.go / download.go / list.go.
package packs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultRepo is the upstream repository manifest-less `dbrelease`
// runs default to. Forks override via the `-repo` flag.
const DefaultRepo = "laradji/deadzone"

// sha256HexRE matches a 64-character hex string. Validation rejects
// anything else in the manifest's sha256 field, catching the "someone
// hand-edited it" failure mode at load time.
var sha256HexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ReleaseRecord describes the most recent `deadzone.db` release — the
// asset name, sha256, size, indexing time, embedder identity, and the
// lib/doc counts snapshotted at release time. Written by `deadzone
// dbrelease`, committed as a release-history trace.
//
// Field order is preserved on marshal via yaml.v3 struct tag
// declaration order, so diffs in PR review land in a predictable
// shape.
type ReleaseRecord struct {
	Tag       string         `yaml:"tag"`
	Asset     string         `yaml:"asset"`
	SHA256    string         `yaml:"sha256"`
	Size      int64          `yaml:"size"`
	IndexedAt time.Time      `yaml:"indexed_at"`
	Embedder  EmbedderRecord `yaml:"embedder"`
	LibCount  int            `yaml:"lib_count"`
	DocCount  int            `yaml:"doc_count"`
}

// EmbedderRecord mirrors the embedder identity triple the scraper
// writes into `db.Meta` and the per-lib `state.yaml` sidecar. Kept
// structurally separate so the packs package does not import the db
// or embed packages for this tiny struct.
type EmbedderRecord struct {
	Kind  string `yaml:"kind"`
	Model string `yaml:"model"`
	Dim   int    `yaml:"dim"`
}

// Manifest is the parsed artifacts/manifest.yaml. It carries exactly
// one ReleaseRecord describing the most recent `deadzone.db` release.
// When per-artifact distribution returns (see #101) a sibling `packs`
// field is expected to come back.
type Manifest struct {
	Release ReleaseRecord `yaml:"release"`
}

// Load reads, parses, and validates a manifest file at path. A freshly
// scraped repo (no release yet) has a zero-value Release with Tag = ""
// — that is valid; Load accepts it and Save later fills it in.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load manifest %s: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	return &m, nil
}

// validate enforces the schema rules. A manifest whose Release.Tag is
// empty is valid and represents "no release yet"; any release that
// carries a tag must also carry every other required field.
func (m *Manifest) validate() error {
	r := m.Release
	if r.Tag == "" {
		// Unset release — the remaining fields are required to be
		// zero too; a partially-populated record is a hand-edit
		// mistake we want to catch at load time.
		if r.Asset != "" || r.SHA256 != "" || r.Size != 0 || !r.IndexedAt.IsZero() {
			return errors.New("release.tag is required when any other release field is set")
		}
		return nil
	}
	if strings.TrimSpace(r.Asset) == "" {
		return errors.New("release.asset is required")
	}
	if !sha256HexRE.MatchString(r.SHA256) {
		return fmt.Errorf("release.sha256 %q is not a 64-char hex string", r.SHA256)
	}
	if r.Size <= 0 {
		return fmt.Errorf("release.size must be > 0, got %d", r.Size)
	}
	if r.IndexedAt.IsZero() {
		return errors.New("release.indexed_at is required")
	}
	if r.Embedder.Kind == "" {
		return errors.New("release.embedder.kind is required")
	}
	if r.Embedder.Model == "" {
		return errors.New("release.embedder.model is required")
	}
	if r.Embedder.Dim <= 0 {
		return fmt.Errorf("release.embedder.dim must be > 0, got %d", r.Embedder.Dim)
	}
	if r.LibCount < 0 {
		return fmt.Errorf("release.lib_count must be >= 0, got %d", r.LibCount)
	}
	if r.DocCount < 0 {
		return fmt.Errorf("release.doc_count must be >= 0, got %d", r.DocCount)
	}
	return nil
}

// Save writes the manifest to path atomically via a temp-file-and-rename
// dance. The temp file lives in the same directory as the destination so
// os.Rename is a true atomic-on-success move. On any failure between
// Write and Rename, the destination file remains untouched.
func (m *Manifest) Save(path string) error {
	if err := m.validate(); err != nil {
		return fmt.Errorf("save manifest %s: %w", path, err)
	}

	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal manifest %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".manifest-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create tmp manifest in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tmp manifest %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp manifest %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}
