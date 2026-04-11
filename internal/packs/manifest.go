// Package packs implements the distribution layer for per-lib artifact
// .db files: it uploads them to a rolling GitHub Release and downloads
// them back on a fresh clone, with sha256 verification on both sides.
//
// The package is the implementation of issue #30. It exposes three
// top-level entry points (one per cmd/packs subcommand):
//
//   - upload.Run: shell out to `gh release upload <tag> <file> --clobber`
//     for every locally-changed artifact and rewrite the manifest file.
//   - download.Run: HTTP GET each release asset referenced by the
//     manifest, verify its sha256, drop it into ./artifacts.
//   - list.Run: pretty-print the manifest as a tabwriter table.
//
// The Manifest YAML schema lives in this file because it is the data
// contract shared by all three subcommands and is the one piece of the
// system that is committed to git.
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

// DefaultRepo is the upstream repository the manifest defaults to when
// neither the manifest's `repo:` field nor the -repo CLI flag is set.
// Forks override either of those two; the constant exists so the
// "no manifest, no flag" first-time-use path still produces a usable
// download URL.
const DefaultRepo = "laradji/deadzone"

// DefaultReleaseTag is the rolling tag the upload command uses on a
// brand-new manifest. Mirrors the "rolling vs. versioned" decision in
// issue #30 — there is one release, named `packs`, that gets clobbered
// in place.
const DefaultReleaseTag = "packs"

// sha256HexRE matches a 64-character hex string. Validation rejects
// anything else in the manifest's sha256 field, catching the "someone
// hand-edited it" failure mode at load time instead of at download
// time.
var sha256HexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Pack is a single entry in the manifest. Field order is preserved on
// round-trip via the yaml.v3 struct tag declaration order; the package
// always serializes lib_id first so the YAML diffs in PRs are easy to
// read.
type Pack struct {
	LibID               string    `yaml:"lib_id"`
	Asset               string    `yaml:"asset"`
	SHA256              string    `yaml:"sha256"`
	Size                int64     `yaml:"size"`
	IndexedAt           time.Time `yaml:"indexed_at"`
	ScrapedWithEmbedder string    `yaml:"scraped_with_embedder,omitempty"`
	ScrapedWithModel    string    `yaml:"scraped_with_model,omitempty"`
}

// Manifest is the parsed artifacts/manifest.yaml. ReleaseTag is the
// rolling tag attached to the GitHub Release that holds the assets;
// Repo is the optional `owner/name` default for downloads (the -repo
// CLI flag overrides it at runtime); Packs is the list of currently-
// canonical per-lib artifacts.
type Manifest struct {
	ReleaseTag string `yaml:"release_tag"`
	Repo       string `yaml:"repo,omitempty"`
	Packs      []Pack `yaml:"packs"`
}

// Load reads, parses, and validates a manifest file at path. Validation
// runs at load time so a malformed manifest fails the run upfront with
// a single error rather than mid-loop with a partial result.
//
// A manifest file with no entries (`packs: []`) is valid and useful —
// it's the placeholder shape committed before the first real upload.
// A manifest with no `release_tag` is rejected because every download
// URL needs one; the upload path defaults to DefaultReleaseTag when
// constructing a brand-new manifest, so the file on disk always has it.
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

// validate enforces the schema rules:
//
//   - release_tag is non-empty
//   - every Pack has a non-empty lib_id starting with "/"
//   - every Pack has a non-empty asset filename
//   - every Pack has a 64-char hex sha256
//   - every Pack has a positive size
//   - every Pack has a non-zero indexed_at timestamp
//   - lib_id is unique across all Packs
//   - asset is unique across all Packs
//
// Embedder identity fields (scraped_with_embedder, scraped_with_model)
// are intentionally optional — they're informational, not load-bearing,
// and a manifest produced before #30 added them should still parse.
func (m *Manifest) validate() error {
	if strings.TrimSpace(m.ReleaseTag) == "" {
		return errors.New("release_tag is required")
	}
	seenLibID := map[string]int{}
	seenAsset := map[string]int{}
	for i, p := range m.Packs {
		if p.LibID == "" {
			return fmt.Errorf("packs[%d]: lib_id is required", i)
		}
		if !strings.HasPrefix(p.LibID, "/") {
			return fmt.Errorf("packs[%d] (%q): lib_id must start with %q", i, p.LibID, "/")
		}
		if p.Asset == "" {
			return fmt.Errorf("packs[%d] (%q): asset is required", i, p.LibID)
		}
		if !sha256HexRE.MatchString(p.SHA256) {
			return fmt.Errorf("packs[%d] (%q): sha256 %q is not a 64-char hex string", i, p.LibID, p.SHA256)
		}
		if p.Size <= 0 {
			return fmt.Errorf("packs[%d] (%q): size must be > 0, got %d", i, p.LibID, p.Size)
		}
		if p.IndexedAt.IsZero() {
			return fmt.Errorf("packs[%d] (%q): indexed_at is required", i, p.LibID)
		}
		if prev, dup := seenLibID[p.LibID]; dup {
			return fmt.Errorf("packs[%d]: duplicate lib_id %q (also at packs[%d])", i, p.LibID, prev)
		}
		if prev, dup := seenAsset[p.Asset]; dup {
			return fmt.Errorf("packs[%d] (%q): duplicate asset %q (also at packs[%d])", i, p.LibID, p.Asset, prev)
		}
		seenLibID[p.LibID] = i
		seenAsset[p.Asset] = i
	}
	return nil
}

// Save writes the manifest to path atomically via a temp-file-and-rename
// dance. The temp file lives in the same directory as the destination so
// os.Rename is a true atomic-on-success move (cross-filesystem renames
// are not, on most Unix variants). On any failure between Write and
// Rename, the destination file remains untouched.
//
// The serialized output is normalized: packs are sorted by lib_id so
// the manifest's git diff is stable across runs regardless of the
// scraper's filesystem walk order. yaml.v3's Encoder uses 2-space
// indentation, which matches the rest of the project's YAML conventions.
func (m *Manifest) Save(path string) error {
	if err := m.validate(); err != nil {
		return fmt.Errorf("save manifest %s: %w", path, err)
	}

	// Local copy with sorted packs so the on-disk byte stream is
	// deterministic; the in-memory Manifest stays untouched in case the
	// caller still has references to specific Pack indices.
	out := *m
	out.Packs = append([]Pack(nil), m.Packs...)
	sortPacksByLibID(out.Packs)

	data, err := yaml.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshal manifest %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".manifest-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create tmp manifest in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup of the temp file on any failure path that
	// doesn't reach the successful Rename. Ignore the error from a
	// double-Remove because Rename consumed the original.
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

// Find returns the Pack with the given lib_id, its index, and a found
// flag. The pointer aliases the slice element so callers can mutate
// in-place; this is intentional for the upload path which preserves
// the existing IndexedAt verbatim on a sha256 match.
func (m *Manifest) Find(libID string) (*Pack, int, bool) {
	for i := range m.Packs {
		if m.Packs[i].LibID == libID {
			return &m.Packs[i], i, true
		}
	}
	return nil, -1, false
}

// Replace inserts or updates a Pack by lib_id. New entries are appended;
// existing entries are overwritten in place. Order of existing entries
// is preserved so the on-disk diff stays minimal (Save sorts on output).
func (m *Manifest) Replace(p Pack) {
	for i := range m.Packs {
		if m.Packs[i].LibID == p.LibID {
			m.Packs[i] = p
			return
		}
	}
	m.Packs = append(m.Packs, p)
}

// FilterByLibID returns the packs whose lib_id matches the filter. An
// empty filter matches everything; otherwise the matcher follows the
// scraper's two-level rule (LibID == filter || LibID has filter as a
// /-suffixed prefix), so passing a base lib_id matches every versioned
// child.
func (m *Manifest) FilterByLibID(filter string) []Pack {
	if filter == "" {
		return append([]Pack(nil), m.Packs...)
	}
	var out []Pack
	for _, p := range m.Packs {
		if matchLibID(p.LibID, filter) {
			out = append(out, p)
		}
	}
	return out
}

// sortPacksByLibID sorts in lexicographic order on lib_id. Used by Save
// to produce a stable on-disk byte stream so manifest diffs in PRs only
// reflect actual content changes, never the scraper's walk order. Kept
// as a helper rather than inlined so tests can independently exercise
// it.
func sortPacksByLibID(p []Pack) {
	// Manual insertion sort — avoids importing sort just for this and
	// is fastest for the small (~100 entries) sizes the manifest will
	// see in practice.
	for i := 1; i < len(p); i++ {
		j := i
		for j > 0 && p[j-1].LibID > p[j].LibID {
			p[j-1], p[j] = p[j], p[j-1]
			j--
		}
	}
}
