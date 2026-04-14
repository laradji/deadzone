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
// Entries come from the map shape `versions: {v1: {ref: tag1}, v2: {ref:
// tag2}}`. Ref, when set, overrides the top-level Ref for this version;
// when empty the top-level Ref applies (if any). Declaration order is
// preserved so scrapes are deterministic.
//
// URLs (see #115) is a per-version override of the parent
// LibrarySource.URLs. When non-nil, Expand uses it verbatim for this
// version; when nil, the version inherits the top-level URLs. An
// explicit empty list is rejected at parse time — inheritance is
// expressed by omitting the field.
type VersionEntry struct {
	Name string
	Ref  string
	URLs []string
}

// LibrarySource is a single entry in libraries_sources.yaml.
//
// A LibrarySource with no Versions describes one library directly. A
// LibrarySource with Versions is a YAML-level shorthand: at Expand() time
// it produces one ResolvedSource per version, each with its URLs templated
// and the version surfaced in the dedicated Version field. The base LibID
// is never mutated — two versions of the same lib share one LibID and
// differ only in Version. See #113.
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
// LibID always equals BaseLibID — it is the base lib_id (e.g.
// /hashicorp/terraform). Version is empty for single-version entries
// and carries the version tag (e.g. "v1.14") for multi-version
// entries. The "<base>/<version>" concat that earlier builds produced
// here is gone (#113); callers that need a (lib_id, version) slot
// pass them as two fields. Ref is the effective git ref applied to
// the URLs (per-version Ref if set, else the top-level
// LibrarySource.Ref).
//
// BaseLibID is retained as a separate field for readability at call
// sites — it is always == LibID after #113, but the name documents
// intent ("the unversioned identity of this lib").
type ResolvedSource struct {
	LibID     string
	BaseLibID string
	Version   string
	Kind      string
	Ref       string
	URLs      []string
}

// UnmarshalYAML parses `versions:` as a mapping:
//
//	versions: {v1: {ref: tag1}, v2: {ref: tag2}}
//
// Each value is an object with optional `ref:` and `urls:` per-version
// overrides. The legacy list form (`versions: [v1, v2]`) is rejected —
// it carried no per-version metadata and was strictly a subset of the
// map shape, so keeping it meant two parse paths for the same semantics
// (see #117). All other fields parse via the standard reflection path.
// Declaration order is preserved so the scrape loop hits versions in a
// deterministic order.
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
		return fmt.Errorf("versions must be a mapping {v1: {ref: tag1}, v2: {ref: tag2}}; list form is no longer supported")
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
				Ref  string    `yaml:"ref"`
				URLs yaml.Node `yaml:"urls"`
			}
			if err := valNode.Decode(&entry); err != nil {
				return fmt.Errorf("versions[%q]: %w", name, err)
			}
			v := VersionEntry{Name: name, Ref: entry.Ref}
			// Distinguish omitted urls (inherit baseline) from explicit
			// `urls: []` (rejected as ambiguous — see #115).
			if entry.URLs.Kind != 0 {
				if entry.URLs.Kind != yaml.SequenceNode {
					return fmt.Errorf("versions[%q].urls must be a list", name)
				}
				if len(entry.URLs.Content) == 0 {
					return fmt.Errorf("versions[%q].urls is an empty list (omit the field to inherit the top-level urls)", name)
				}
				var urls []string
				if err := entry.URLs.Decode(&urls); err != nil {
					return fmt.Errorf("versions[%q].urls: %w", name, err)
				}
				v.URLs = urls
			}
			l.Versions = append(l.Versions, v)
		}
	default:
		return fmt.Errorf("versions must be a mapping, got yaml kind %d", raw.Versions.Kind)
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
//   - top-level urls: no empty/whitespace entries. Non-empty when versions
//     is unset; may be empty when versions is set only if every version
//     supplies its own urls (see #115)
//   - ref (when set) has no whitespace and no placeholder tokens
//   - if versions is set: every version has a non-empty name without
//     whitespace, "/", or "{version}"; per-version refs (map shape) have
//     no whitespace; every effective URL (baseline OR per-version
//     override) contains "{version}"; per-version urls (when set) are a
//     non-empty list with no whitespace-only entries
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
	for _, u := range l.URLs {
		if strings.TrimSpace(u) == "" {
			return fmt.Errorf("urls contains an empty entry")
		}
	}
	if err := validateRef(l.Ref); err != nil {
		return fmt.Errorf("ref: %w", err)
	}

	if len(l.Versions) == 0 {
		// No versions: top-level urls must be non-empty, no URL may
		// contain {version} (unresolved placeholder at runtime), and
		// any {ref} requires a top-level ref.
		if len(l.URLs) == 0 {
			return fmt.Errorf("urls must be non-empty")
		}
		for _, u := range l.URLs {
			if strings.Contains(u, versionPlaceholder) {
				return fmt.Errorf("url %q contains %s but no versions are listed", u, versionPlaceholder)
			}
		}
		for _, u := range l.URLs {
			if strings.Contains(u, refPlaceholder) && l.Ref == "" {
				return fmt.Errorf("a url contains %s but ref is not set", refPlaceholder)
			}
		}
		return nil
	}

	// Versions present.
	//
	// Baseline urls, when set, are the fallback for any version that
	// doesn't override — so they must contain {version}. When baseline
	// is empty, every version must supply its own urls.
	if len(l.URLs) == 0 {
		for _, v := range l.Versions {
			if len(v.URLs) == 0 {
				return fmt.Errorf("urls must be non-empty (or every version must provide its own urls); versions[%q] has no urls and the top-level urls is empty", v.Name)
			}
		}
	} else {
		for _, u := range l.URLs {
			if !strings.Contains(u, versionPlaceholder) {
				return fmt.Errorf("url %q is missing the %s placeholder (required when versions is set)", u, versionPlaceholder)
			}
		}
	}

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
		for _, u := range v.URLs {
			if strings.TrimSpace(u) == "" {
				return fmt.Errorf("versions[%q].urls contains an empty entry", v.Name)
			}
			if !strings.Contains(u, versionPlaceholder) {
				return fmt.Errorf("versions[%q] url %q is missing the %s placeholder (required when versions is set)", v.Name, u, versionPlaceholder)
			}
		}

		// {ref} check runs on the effective URL list for this version
		// (per-version override, else baseline).
		effective := v.URLs
		if len(effective) == 0 {
			effective = l.URLs
		}
		urlHasRef := false
		for _, u := range effective {
			if strings.Contains(u, refPlaceholder) {
				urlHasRef = true
				break
			}
		}
		if urlHasRef && v.Ref == "" && l.Ref == "" {
			return fmt.Errorf("a url contains %s but neither versions[%q].ref nor the top-level ref is set", refPlaceholder, v.Name)
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
// Multi-version entries produce one ResolvedSource per version with
// LibID == BaseLibID (the base, e.g. /hashicorp/terraform) and
// Version set to the version tag. The {version} placeholder is
// substituted in each URL, and the {ref} placeholder is substituted
// from the per-version Ref if set, else from the top-level Ref. The
// "<base>/<version>" concatenation that earlier builds produced here
// is gone (#113); downstream code treats (LibID, Version) as the
// canonical slot.
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
		// Per-version URLs replace the baseline wholesale when set;
		// nil/empty means inherit the top-level urls (#115).
		src := v.URLs
		if len(src) == 0 {
			src = l.URLs
		}
		urls := make([]string, len(src))
		for i, u := range src {
			u = strings.ReplaceAll(u, versionPlaceholder, v.Name)
			urls[i] = substituteRef(u, ref)
		}
		out = append(out, ResolvedSource{
			LibID:     l.LibID,
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

// Resolve flattens every entry in the config into ResolvedSources,
// applying the (libFilter, versionFilter) filter pair introduced in
// #113:
//
//   - libFilter == "" matches every resolved entry (versionFilter is
//     ignored in this case; the caller is expected to reject that
//     combination as a usage error before calling in).
//   - libFilter != "", versionFilter == "" matches every expanded
//     version of that base (and the base itself for single-version
//     entries). This is the "scrape every version of terraform" knob.
//   - libFilter != "", versionFilter != "" matches the exactly-one
//     expanded entry whose (BaseLibID, Version) pair equals the
//     filter. This is the "scrape only terraform v1.14" knob.
func (c *Config) Resolve(libFilter, versionFilter string) []ResolvedSource {
	var out []ResolvedSource
	for _, lib := range c.Libraries {
		for _, r := range lib.Expand() {
			if libFilter == "" {
				out = append(out, r)
				continue
			}
			if r.BaseLibID != libFilter {
				continue
			}
			if versionFilter != "" && r.Version != versionFilter {
				continue
			}
			out = append(out, r)
		}
	}
	return out
}
