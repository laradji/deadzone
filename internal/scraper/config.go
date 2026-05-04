package scraper

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// refPlaceholder is the literal token substituted with the effective git
// ref (top-level Source.Ref or per-version VersionEntry.Ref) at expand
// time. It is the sole URL placeholder after #120 — the former
// {version} placeholder was dropped because version identifiers (the
// `versions:` map keys) are now user-facing labels that no longer need
// to appear in URLs. URLs that don't contain the token are left
// untouched, so a lib can opt into ref pinning incrementally. See #103.
const refPlaceholder = "{ref}"

// deprecatedVersionPlaceholder is the former URL placeholder, now
// rejected at parse time with a pointer at {ref}. Kept as a named
// constant so the rejection message is grep-able.
const deprecatedVersionPlaceholder = "{version}"

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

	// KindGodoc parses Go source files via go/parser + go/doc to emit
	// one db.Doc per exported identifier. Source bytes come from
	// proxy.golang.org for third-party modules (sumdb-verified) or from
	// the GitHub Contents API for the stdlib (golang/go is not a Go
	// module, so the proxy doesn't serve it). Design + ACs: #198.
	KindGodoc = "godoc"
)

// validKinds enumerates the source kind discriminators known to the
// loader. Adding new kinds means appending to this set, not changing
// the schema.
var validKinds = map[string]bool{
	KindGithubMD:       true,
	KindGithubRST:      true,
	KindScrapeViaAgent: true,
	KindGodoc:          true,
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
//
// Description (see #191) is a per-version override of the parent
// LibrarySource.Description. The empty string means "inherit the
// top-level description"; whitespace-only values normalize to "" at
// parse time so YAML quirks don't accidentally suppress inheritance.
// Used only when two versions have meaningfully diverged (e.g. major
// API rewrite); otherwise leave it empty so both versions ride the
// shared top-level description.
type VersionEntry struct {
	Name        string
	Ref         string
	URLs        []string
	Description string
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
//
// Description (see #191) is an optional 1-2 sentence upstream-authored
// summary of what this lib IS and what role it plays. It is mixed into
// the libs-table embedding alongside the normalized lib_id so
// search_libraries can rank on semantic intent rather than lib_id token
// overlap alone. Empty string is the legacy behavior (embed lib_id
// alone); whitespace-only values normalize to "" at parse time.
type LibrarySource struct {
	LibID       string
	Kind        string
	URLs        []string
	Ref         string
	Description string
	Versions    []VersionEntry
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
//
// Description (see #191) is the effective per-(lib_id, version)
// description: per-version override → top-level → "". Threaded down to
// the embedder so two versions of the same lib can produce distinct
// embeddings when their descriptions diverge.
type ResolvedSource struct {
	LibID       string
	BaseLibID   string
	Version     string
	Kind        string
	Ref         string
	URLs        []string
	Description string
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
		LibID       string    `yaml:"lib_id"`
		Kind        string    `yaml:"kind"`
		URLs        []string  `yaml:"urls"`
		Ref         string    `yaml:"ref"`
		Description string    `yaml:"description"`
		Versions    yaml.Node `yaml:"versions"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	l.LibID = raw.LibID
	l.Kind = raw.Kind
	l.URLs = raw.URLs
	l.Ref = raw.Ref
	// Whitespace-only descriptions normalize to "" so an accidental
	// `description: " "` doesn't suppress inheritance for per-version
	// entries — same rule applied at both levels (#191).
	l.Description = strings.TrimSpace(raw.Description)
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
				Ref         string    `yaml:"ref"`
				URLs        yaml.Node `yaml:"urls"`
				Description string    `yaml:"description"`
			}
			if err := valNode.Decode(&entry); err != nil {
				return fmt.Errorf("versions[%q]: %w", name, err)
			}
			v := VersionEntry{
				Name:        name,
				Ref:         entry.Ref,
				Description: strings.TrimSpace(entry.Description),
			}
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

// validate enforces the v1 schema rules (post-#120):
//   - lib_id non-empty, starts with "/", does not end with "/"
//   - kind in the known set
//   - top-level urls: no empty/whitespace entries; no URL may contain the
//     deprecated "{version}" placeholder. Non-empty when versions is
//     unset; may be empty when versions is set only if every version
//     supplies its own urls (see #115)
//   - ref (when set) has no whitespace and no placeholder token
//   - if versions is set: every version has a non-empty name without
//     whitespace or "/"; per-version refs have no whitespace; every
//     effective URL list (baseline OR per-version override) contains
//     "{ref}"; per-version urls (when set) are a non-empty list with no
//     whitespace-only entries and no "{version}"
//   - if any URL contains "{ref}": for a single-version entry the top
//     level ref must be set; for a multi-version entry every version
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
		return fmt.Errorf("unknown kind %q (valid: %s, %s, %s, %s)", l.Kind, KindGithubMD, KindGithubRST, KindScrapeViaAgent, KindGodoc)
	}
	for _, u := range l.URLs {
		if strings.TrimSpace(u) == "" {
			return fmt.Errorf("urls contains an empty entry")
		}
		if err := rejectDeprecatedVersionPlaceholder(u); err != nil {
			return err
		}
	}
	if err := validateRef(l.Ref); err != nil {
		return fmt.Errorf("ref: %w", err)
	}

	if len(l.Versions) == 0 {
		// No versions: top-level urls must be non-empty and any {ref}
		// requires a top-level ref.
		if len(l.URLs) == 0 {
			return fmt.Errorf("urls must be non-empty")
		}
		for _, u := range l.URLs {
			if strings.Contains(u, refPlaceholder) && l.Ref == "" {
				return fmt.Errorf("a url contains %s but ref is not set", refPlaceholder)
			}
		}
		return nil
	}

	// Versions present. Baseline urls, when set, are the fallback for
	// any version that doesn't override. When baseline is empty, every
	// version must supply its own urls.
	if len(l.URLs) == 0 {
		for _, v := range l.Versions {
			if len(v.URLs) == 0 {
				return fmt.Errorf("urls must be non-empty (or every version must provide its own urls); versions[%q] has no urls and the top-level urls is empty", v.Name)
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
		if err := validateRef(v.Ref); err != nil {
			return fmt.Errorf("versions[%q].ref: %w", v.Name, err)
		}
		for _, u := range v.URLs {
			if strings.TrimSpace(u) == "" {
				return fmt.Errorf("versions[%q].urls contains an empty entry", v.Name)
			}
			if err := rejectDeprecatedVersionPlaceholder(u); err != nil {
				return fmt.Errorf("versions[%q].urls: %w", v.Name, err)
			}
		}

		// Every effective URL list must contain {ref} (baseline or
		// per-version override) — that is the "differentiates per
		// version" invariant that used to be spelled via {version}.
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
		if !urlHasRef {
			return fmt.Errorf("versions[%q]: no effective url contains %s (required when versions is set)", v.Name, refPlaceholder)
		}
		if v.Ref == "" && l.Ref == "" {
			return fmt.Errorf("a url contains %s but neither versions[%q].ref nor the top-level ref is set", refPlaceholder, v.Name)
		}
	}
	return nil
}

// rejectDeprecatedVersionPlaceholder returns an error when a URL still
// carries the pre-#120 "{version}" placeholder. The message names {ref}
// as the replacement so operators porting an old registry get a direct
// pointer at the new placeholder vocabulary.
func rejectDeprecatedVersionPlaceholder(u string) error {
	if strings.Contains(u, deprecatedVersionPlaceholder) {
		return fmt.Errorf("url %q contains deprecated %s placeholder — use %s instead (see #120)", u, deprecatedVersionPlaceholder, refPlaceholder)
	}
	return nil
}

// validateRef enforces the format rules common to top-level and
// per-version refs: empty is allowed (caller decides whether that's a
// problem), but a non-empty ref must not contain whitespace or the
// {ref} placeholder token itself.
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
	return nil
}

// Expand turns one LibrarySource into one or more ResolvedSources.
//
// Single-version entries pass through unchanged: LibID == BaseLibID,
// Version is empty, URLs are copied with {ref} substituted from the
// top-level Ref (if present).
//
// Multi-version entries produce one ResolvedSource per version with
// LibID == BaseLibID (the base, e.g. /hashicorp/terraform) and Version
// set to the version identifier from the `versions:` map. The {ref}
// placeholder is substituted from the per-version Ref if set, else
// from the top-level Ref. Post-#120 there is no {version} substitution:
// the version identifier is a user-facing label only, and per-version
// URL divergence is expressed either through {ref} (10 libs) or
// per-version `urls:` overrides (terraform). The "<base>/<version>"
// concatenation that earlier builds produced here is gone (#113);
// downstream code treats (LibID, Version) as the canonical slot.
func (l LibrarySource) Expand() []ResolvedSource {
	if len(l.Versions) == 0 {
		urls := make([]string, len(l.URLs))
		for i, u := range l.URLs {
			urls[i] = substituteRef(u, l.Ref)
		}
		return []ResolvedSource{{
			LibID:       l.LibID,
			BaseLibID:   l.LibID,
			Kind:        l.Kind,
			Ref:         l.Ref,
			URLs:        urls,
			Description: l.Description,
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
			urls[i] = substituteRef(u, ref)
		}
		// Effective description: per-version override → top-level →
		// "". Whitespace-only is already normalized to "" at parse
		// time (UnmarshalYAML), so a non-empty per-version value is
		// always a deliberate override (#191).
		desc := v.Description
		if desc == "" {
			desc = l.Description
		}
		out = append(out, ResolvedSource{
			LibID:       l.LibID,
			BaseLibID:   l.LibID,
			Version:     v.Name,
			Kind:        l.Kind,
			Ref:         ref,
			URLs:        urls,
			Description: desc,
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
