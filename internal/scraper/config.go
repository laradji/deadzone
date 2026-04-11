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

// validKinds enumerates the source kind discriminators known to v1 of the
// loader. Adding new kinds (e.g. scrape-via-agent in #27) means appending
// to this set, not changing the schema.
var validKinds = map[string]bool{
	"github-md": true,
}

// Config is the parsed libraries_sources.yaml file.
type Config struct {
	Libraries []LibrarySource `yaml:"libraries"`
}

// LibrarySource is a single entry in libraries_sources.yaml.
//
// A LibrarySource with no Versions describes one library directly. A
// LibrarySource with Versions is a YAML-level shorthand: at Expand() time
// it produces one ResolvedSource per version, each with its URLs templated
// and an effective lib_id of "<LibID>/<version>".
type LibrarySource struct {
	LibID    string   `yaml:"lib_id"`
	Kind     string   `yaml:"kind"`
	URLs     []string `yaml:"urls"`
	Versions []string `yaml:"versions,omitempty"`
}

// ResolvedSource is one library, post-version-expansion, ready to scrape.
//
// For single-version entries, LibID == BaseLibID and Version is empty.
// For multi-version entries, LibID is "<BaseLibID>/<Version>" and the URLs
// have the {version} placeholder substituted.
type ResolvedSource struct {
	LibID     string
	BaseLibID string
	Version   string
	Kind      string
	URLs      []string
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
//   - if versions is set: every version is non-empty with no whitespace,
//     no "/", and no literal "{version}"; every URL contains "{version}"
//   - if versions is unset: no URL contains "{version}"
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
		return fmt.Errorf("unknown kind %q (valid: github-md)", l.Kind)
	}
	if len(l.URLs) == 0 {
		return fmt.Errorf("urls must be non-empty")
	}
	for _, u := range l.URLs {
		if strings.TrimSpace(u) == "" {
			return fmt.Errorf("urls contains an empty entry")
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
		return nil
	}

	// Versions present: every version string must be a clean tag, and
	// every URL must reference {version} (otherwise the expansion would
	// produce N identical rows).
	for _, v := range l.Versions {
		if v == "" {
			return fmt.Errorf("versions contains an empty entry")
		}
		if strings.ContainsAny(v, " \t\n\r") {
			return fmt.Errorf("version %q contains whitespace", v)
		}
		if strings.Contains(v, "/") {
			return fmt.Errorf("version %q must not contain %q", v, "/")
		}
		if strings.Contains(v, versionPlaceholder) {
			return fmt.Errorf("version %q must not contain literal %q", v, versionPlaceholder)
		}
	}
	for _, u := range l.URLs {
		if !strings.Contains(u, versionPlaceholder) {
			return fmt.Errorf("url %q is missing the %s placeholder (required when versions is set)", u, versionPlaceholder)
		}
	}
	return nil
}

// Expand turns one LibrarySource into one or more ResolvedSources.
//
// Single-version entries pass through unchanged: LibID == BaseLibID,
// Version is empty, URLs are copied as-is.
//
// Multi-version entries produce one ResolvedSource per version, with the
// effective lib_id formed as "<base>/<version>" and the {version}
// placeholder substituted in each URL.
func (l LibrarySource) Expand() []ResolvedSource {
	if len(l.Versions) == 0 {
		return []ResolvedSource{{
			LibID:     l.LibID,
			BaseLibID: l.LibID,
			Kind:      l.Kind,
			URLs:      append([]string(nil), l.URLs...),
		}}
	}
	out := make([]ResolvedSource, 0, len(l.Versions))
	for _, v := range l.Versions {
		urls := make([]string, len(l.URLs))
		for i, u := range l.URLs {
			urls[i] = strings.ReplaceAll(u, versionPlaceholder, v)
		}
		out = append(out, ResolvedSource{
			LibID:     l.LibID + "/" + v,
			BaseLibID: l.LibID,
			Version:   v,
			Kind:      l.Kind,
			URLs:      urls,
		})
	}
	return out
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
