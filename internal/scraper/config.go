package scraper

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// versionPlaceholder is the literal token substituted with each entry in
// LibrarySource.Versions when expanding a multi-version entry. It is also
// the token rejected by validation in places where it must not appear
// (single-version entries, version strings themselves).
const versionPlaceholder = "{version}"

// refPlaceholder is the literal token substituted with the effective git
// ref (top-level Source.Ref or per-version VersionEntry.Ref) at expand
// time. URLs that do not contain the token are left untouched, so a lib
// can opt into ref pinning incrementally. See #103.
const refPlaceholder = "{ref}"

// Kind discriminators for LibrarySource.Kind. All branches feed the
// same downstream pipeline (parse → embed → store); they only differ
// in how the source markup is obtained and which parser turns it into
// db.Doc entries.
const (
	// KindGithubMD is the fast path: HTTP GET on raw markdown URLs and
	// straight into ParseMarkdown. No LLM, no preprocessing.
	KindGithubMD = "github-md"

	// KindGithubRST mirrors KindGithubMD for projects that ship docs as
	// reStructuredText in the source repo (cpython, Django, NumPy, …).
	// HTTP GET → ParseRST → db.Doc. No LLM. See #95.
	KindGithubRST = "github-rst"

	// KindScrapeViaAgent delegates content → clean markdown extraction
	// to an OpenAI-compatible chat completions endpoint (Ollama, vLLM,
	// LocalAI, OpenAI, ...). The catch-all path for HTML doc sites and
	// any other format that isn't trivially raw markdown. See #27.
	KindScrapeViaAgent = "scrape-via-agent"
)

// validKinds enumerates the source kind discriminators known to the
// loader. Adding new kinds means appending to this set, not changing
// the schema.
var validKinds = map[string]bool{
	KindGithubMD:       true,
	KindGithubRST:      true,
	KindScrapeViaAgent: true,
}

// Config is the parsed libraries_sources.yaml file.
type Config struct {
	Libraries []LibrarySource `yaml:"libraries"`
}

// VersionEntry is one element of LibrarySource.Versions after parsing.
//
// The list shorthand `versions: [v1, v2]` produces entries with Ref
// empty (the lib's top-level Ref applies, if any). The map shorthand
// `versions: {v1: {ref: tag1}, v2: {ref: tag2}}` produces entries with
// per-version Ref set, which overrides the top-level Ref for that
// version. Declaration order is preserved so scrapes are deterministic.
type VersionEntry struct {
	Name string
	Ref  string
}

// LibrarySource is a single entry in libraries_sources.yaml.
//
// A LibrarySource with no Versions describes one library directly. A
// LibrarySource with Versions is a YAML-level shorthand: at Expand() time
// it produces one ResolvedSource per version, each with its URLs templated
// and an effective lib_id of "<LibID>/<version>".
//
// Ref pins URLs to a single upstream git tag or commit SHA when URLs
// contain the literal "{ref}" token. See #103.
type LibrarySource struct {
	LibID    string
	Kind     string
	URLs     []string
	Ref      string
	Versions []VersionEntry
}

// ResolvedSource is one library, post-version-expansion, ready to scrape.
//
// For single-version entries, LibID == BaseLibID and Version is empty.
// For multi-version entries, LibID is "<BaseLibID>/<Version>" and the URLs
// have the {version} placeholder substituted. Ref is the effective git
// ref applied to the URLs (per-version Ref if set, else the top-level
// LibrarySource.Ref).
type ResolvedSource struct {
	LibID     string
	BaseLibID string
	Version   string
	Kind      string
	Ref       string
	URLs      []string
}

// UnmarshalYAML accepts both shapes for `versions:`:
//
//	versions: [v1, v2]                                 # list shorthand
//	versions: {v1: {ref: tag1}, v2: {ref: tag2}}       # map shorthand (per-version ref)
//
// All other fields parse via the standard reflection path. Declaration
// order is preserved for the map shape so the scrape loop hits versions
// in a deterministic order.
func (l *LibrarySource) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		LibID    string    `yaml:"lib_id"`
		Kind     string    `yaml:"kind"`
		URLs     []string  `yaml:"urls"`
		Ref      string    `yaml:"ref"`
		Versions yaml.Node `yaml:"versions"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	l.LibID = raw.LibID
	l.Kind = raw.Kind
	l.URLs = raw.URLs
	l.Ref = raw.Ref
	l.Versions = nil

	if raw.Versions.Kind == 0 {
		return nil
	}
	switch raw.Versions.Kind {
	case yaml.SequenceNode:
		for _, item := range raw.Versions.Content {
			var name string
			if err := item.Decode(&name); err != nil {
				return fmt.Errorf("versions list entry: %w", err)
			}
			l.Versions = append(l.Versions, VersionEntry{Name: name})
		}
	case yaml.MappingNode:
		// Content alternates key, value, key, value, ... — iterate by
		// declaration order.
		for i := 0; i+1 < len(raw.Versions.Content); i += 2 {
			keyNode := raw.Versions.Content[i]
			valNode := raw.Versions.Content[i+1]
			var name string
			if err := keyNode.Decode(&name); err != nil {
				return fmt.Errorf("versions map key: %w", err)
			}
			var entry struct {
				Ref string `yaml:"ref"`
			}
			if err := valNode.Decode(&entry); err != nil {
				return fmt.Errorf("versions[%q]: %w", name, err)
			}
			l.Versions = append(l.Versions, VersionEntry{Name: name, Ref: entry.Ref})
		}
	default:
		return fmt.Errorf("versions must be a list or a mapping, got yaml kind %d", raw.Versions.Kind)
	}
	return nil
}

// LoadConfig reads, parses, and validates a libraries_sources.yaml file.
// All validation runs at parse time so a misconfigured registry fails the
// scraper immediately rather than mid-run.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if len(cfg.Libraries) == 0 {
		return nil, fmt.Errorf("config %s: no libraries defined", path)
	}
	for i, lib := range cfg.Libraries {
		if err := lib.validate(); err != nil {
			return nil, fmt.Errorf("config %s: libraries[%d] (%q): %w", path, i, lib.LibID, err)
		}
	}
	return &cfg, nil
}

// validate enforces the v1 schema rules:
//   - lib_id non-empty, starts with "/", does not end with "/"
//   - kind in the known set
//   - urls non-empty, no empty/whitespace entries
//   - ref (when set) has no whitespace and no placeholder tokens
//   - if versions is set: every version has a non-empty name without
//     whitespace, "/", or "{version}"; per-version refs (map shape) have
//     no whitespace; every URL contains "{version}"
//   - if versions is unset: no URL contains "{version}"
//   - if any URL contains "{ref}": for the single-version entry the
//     top-level ref must be set; for a multi-version entry every version
//     must resolve to a non-empty ref (per-version ref or top-level ref).
func (l LibrarySource) validate() error {
	if l.LibID == "" {
		return fmt.Errorf("lib_id is required")
	}
	if !strings.HasPrefix(l.LibID, "/") {
		return fmt.Errorf("lib_id %q must start with %q", l.LibID, "/")
	}
	if strings.HasSuffix(l.LibID, "/") {
		return fmt.Errorf("lib_id %q must not end with %q", l.LibID, "/")
	}
	if l.Kind == "" {
		return fmt.Errorf("kind is required")
	}
	if !validKinds[l.Kind] {
		return fmt.Errorf("unknown kind %q (valid: %s, %s, %s)", l.Kind, KindGithubMD, KindGithubRST, KindScrapeViaAgent)
	}
	if len(l.URLs) == 0 {
		return fmt.Errorf("urls must be non-empty")
	}
	for _, u := range l.URLs {
		if strings.TrimSpace(u) == "" {
			return fmt.Errorf("urls contains an empty entry")
		}
	}
	if err := validateRef(l.Ref); err != nil {
		return fmt.Errorf("ref: %w", err)
	}

	urlHasRef := false
	for _, u := range l.URLs {
		if strings.Contains(u, refPlaceholder) {
			urlHasRef = true
			break
		}
	}

	if len(l.Versions) == 0 {
		// No versions: no URL may contain {version} — it would be an
		// unresolved placeholder at runtime.
		for _, u := range l.URLs {
			if strings.Contains(u, versionPlaceholder) {
				return fmt.Errorf("url %q contains %s but no versions are listed", u, versionPlaceholder)
			}
		}
		if urlHasRef && l.Ref == "" {
			return fmt.Errorf("a url contains %s but ref is not set", refPlaceholder)
		}
		return nil
	}

	// Versions present: every version string must be a clean tag, and
	// every URL must reference {version} (otherwise the expansion would
	// produce N identical rows).
	for _, v := range l.Versions {
		if v.Name == "" {
			return fmt.Errorf("versions contains an empty entry")
		}
		if strings.ContainsAny(v.Name, " \t\n\r") {
			return fmt.Errorf("version %q contains whitespace", v.Name)
		}
		if strings.Contains(v.Name, "/") {
			return fmt.Errorf("version %q must not contain %q", v.Name, "/")
		}
		if strings.Contains(v.Name, versionPlaceholder) {
			return fmt.Errorf("version %q must not contain literal %q", v.Name, versionPlaceholder)
		}
		if err := validateRef(v.Ref); err != nil {
			return fmt.Errorf("versions[%q].ref: %w", v.Name, err)
		}
		if urlHasRef && v.Ref == "" && l.Ref == "" {
			return fmt.Errorf("a url contains %s but neither versions[%q].ref nor the top-level ref is set", refPlaceholder, v.Name)
		}
	}
	for _, u := range l.URLs {
		if !strings.Contains(u, versionPlaceholder) {
			return fmt.Errorf("url %q is missing the %s placeholder (required when versions is set)", u, versionPlaceholder)
		}
	}
	return nil
}

// validateRef enforces the format rules common to top-level and
// per-version refs: empty is allowed (caller decides whether that's a
// problem), but a non-empty ref must not contain whitespace or either
// of the substitution placeholders.
func validateRef(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.ContainsAny(ref, " \t\n\r") {
		return fmt.Errorf("ref %q contains whitespace", ref)
	}
	if strings.Contains(ref, refPlaceholder) {
		return fmt.Errorf("ref %q must not contain literal %q", ref, refPlaceholder)
	}
	if strings.Contains(ref, versionPlaceholder) {
		return fmt.Errorf("ref %q must not contain literal %q", ref, versionPlaceholder)
	}
	return nil
}

// Expand turns one LibrarySource into one or more ResolvedSources.
//
// Single-version entries pass through unchanged: LibID == BaseLibID,
// Version is empty, URLs are copied with {ref} substituted from the
// top-level Ref (if present).
//
// Multi-version entries produce one ResolvedSource per version, with the
// effective lib_id formed as "<base>/<version>", the {version}
// placeholder substituted in each URL, and the {ref} placeholder
// substituted from the per-version Ref if set, else from the top-level
// Ref.
func (l LibrarySource) Expand() []ResolvedSource {
	if len(l.Versions) == 0 {
		urls := make([]string, len(l.URLs))
		for i, u := range l.URLs {
			urls[i] = substituteRef(u, l.Ref)
		}
		return []ResolvedSource{{
			LibID:     l.LibID,
			BaseLibID: l.LibID,
			Kind:      l.Kind,
			Ref:       l.Ref,
			URLs:      urls,
		}}
	}
	out := make([]ResolvedSource, 0, len(l.Versions))
	for _, v := range l.Versions {
		ref := v.Ref
		if ref == "" {
			ref = l.Ref
		}
		urls := make([]string, len(l.URLs))
		for i, u := range l.URLs {
			u = strings.ReplaceAll(u, versionPlaceholder, v.Name)
			urls[i] = substituteRef(u, ref)
		}
		out = append(out, ResolvedSource{
			LibID:     l.LibID + "/" + v.Name,
			BaseLibID: l.LibID,
			Version:   v.Name,
			Kind:      l.Kind,
			Ref:       ref,
			URLs:      urls,
		})
	}
	return out
}

// substituteRef replaces {ref} in the URL when ref is non-empty.
// Validation guarantees we never reach Expand with a {ref} placeholder
// and an empty effective ref.
func substituteRef(url, ref string) string {
	if ref == "" {
		return url
	}
	return strings.ReplaceAll(url, refPlaceholder, ref)
}

// Resolve flattens every entry in the config into ResolvedSources, applying
// the two-level lib_id filter described in #51:
//
//   - filter == "" matches every resolved entry
//   - filter == "/org/project" matches every expanded version of that base
//     (and the base itself for single-version entries)
//   - filter == "/org/project/version" matches exactly one expanded entry
func (c *Config) Resolve(filter string) []ResolvedSource {
	var out []ResolvedSource
	for _, lib := range c.Libraries {
		for _, r := range lib.Expand() {
			if filter == "" || r.LibID == filter || r.BaseLibID == filter {
				out = append(out, r)
			}
		}
	}
	return out
}
