package scraper_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/laradji/deadzone/internal/scraper"
)

// writeConfig drops a libraries_sources.yaml file into a fresh temp dir
// and returns its absolute path. Each test gets its own dir so failures
// don't leak state between cases.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "libraries_sources.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfig_ValidMixedEntries(t *testing.T) {
	path := writeConfig(t, `
libraries:
  - lib_id: /modelcontextprotocol/go-sdk
    kind: github-md
    urls:
      - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/main/README.md
      - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/main/docs/quick_start.md
  - lib_id: /facebook/react
    kind: github-md
    versions: [v18, v19]
    urls:
      - https://raw.githubusercontent.com/facebook/react/{version}/README.md
      - https://raw.githubusercontent.com/facebook/react/{version}/docs/getting-started.md
`)

	cfg, err := scraper.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Libraries) != 2 {
		t.Fatalf("expected 2 libraries, got %d", len(cfg.Libraries))
	}

	if cfg.Libraries[0].LibID != "/modelcontextprotocol/go-sdk" {
		t.Errorf("libraries[0].LibID = %q", cfg.Libraries[0].LibID)
	}
	if len(cfg.Libraries[0].URLs) != 2 {
		t.Errorf("libraries[0].URLs len = %d, want 2", len(cfg.Libraries[0].URLs))
	}
	if len(cfg.Libraries[0].Versions) != 0 {
		t.Errorf("libraries[0] should have no versions, got %v", cfg.Libraries[0].Versions)
	}

	if cfg.Libraries[1].LibID != "/facebook/react" {
		t.Errorf("libraries[1].LibID = %q", cfg.Libraries[1].LibID)
	}
	if got := cfg.Libraries[1].Versions; len(got) != 2 || got[0].Name != "v18" || got[1].Name != "v19" {
		t.Errorf("libraries[1].Versions = %v, want [{v18} {v19}]", got)
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := scraper.LoadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("error should mention 'read config', got %v", err)
	}
}

func TestLoadConfig_MalformedYAML(t *testing.T) {
	path := writeConfig(t, "libraries: [this is not valid yaml")
	_, err := scraper.LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for malformed yaml, got nil")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error should mention 'parse config', got %v", err)
	}
}

func TestLoadConfig_EmptyLibraries(t *testing.T) {
	path := writeConfig(t, "libraries: []\n")
	_, err := scraper.LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for empty libraries, got nil")
	}
}

func TestLoadConfig_RejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing lib_id",
			yaml: `
libraries:
  - kind: github-md
    urls: [https://example.com/a.md]
`,
			want: "lib_id is required",
		},
		{
			name: "lib_id without leading slash",
			yaml: `
libraries:
  - lib_id: org/project
    kind: github-md
    urls: [https://example.com/a.md]
`,
			want: "must start with",
		},
		{
			name: "lib_id with trailing slash",
			yaml: `
libraries:
  - lib_id: /org/project/
    kind: github-md
    urls: [https://example.com/a.md]
`,
			want: "must not end with",
		},
		{
			name: "missing kind",
			yaml: `
libraries:
  - lib_id: /org/project
    urls: [https://example.com/a.md]
`,
			want: "kind is required",
		},
		{
			name: "unknown kind",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: html-scrape
    urls: [https://example.com/a.md]
`,
			want: "unknown kind",
		},
		{
			name: "missing urls",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
`,
			want: "urls must be non-empty",
		},
		{
			name: "empty urls list",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    urls: []
`,
			want: "urls must be non-empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.yaml)
			_, err := scraper.LoadConfig(path)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoadConfig_AcceptsScrapeViaAgentKind(t *testing.T) {
	path := writeConfig(t, `
libraries:
  - lib_id: /hashicorp/terraform-provider-aws
    kind: scrape-via-agent
    urls:
      - https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/s3_bucket
      - https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/iam_role
`)

	cfg, err := scraper.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Libraries) != 1 {
		t.Fatalf("expected 1 library, got %d", len(cfg.Libraries))
	}
	if got := cfg.Libraries[0].Kind; got != scraper.KindScrapeViaAgent {
		t.Errorf("Kind = %q, want %q", got, scraper.KindScrapeViaAgent)
	}

	// Verify the kind round-trips through Resolve as well.
	resolved := cfg.Resolve("", "")
	if len(resolved) != 1 {
		t.Fatalf("Resolve returned %d, want 1", len(resolved))
	}
	if resolved[0].Kind != scraper.KindScrapeViaAgent {
		t.Errorf("ResolvedSource.Kind = %q, want %q", resolved[0].Kind, scraper.KindScrapeViaAgent)
	}
}

func TestLoadConfig_VersionsRules(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "versions present but URL lacks placeholder",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    versions: [v1, v2]
    urls:
      - https://example.com/{version}/a.md
      - https://example.com/main/b.md
`,
			want: "missing the {version} placeholder",
		},
		{
			name: "URL contains placeholder but no versions",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    urls:
      - https://example.com/{version}/a.md
`,
			want: "no versions are listed",
		},
		{
			name: "version contains slash",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    versions: ["v1/foo"]
    urls:
      - https://example.com/{version}/a.md
`,
			want: `must not contain "/"`,
		},
		{
			name: "version contains whitespace",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    versions: ["v 1"]
    urls:
      - https://example.com/{version}/a.md
`,
			want: "contains whitespace",
		},
		{
			name: "version literally is {version}",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    versions: ["{version}"]
    urls:
      - https://example.com/{version}/a.md
`,
			want: `must not contain literal "{version}"`,
		},
		{
			name: "empty version string",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    versions: [""]
    urls:
      - https://example.com/{version}/a.md
`,
			want: "empty entry",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.yaml)
			_, err := scraper.LoadConfig(path)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestExpand_SingleVersionIsIdentity(t *testing.T) {
	src := scraper.LibrarySource{
		LibID: "/modelcontextprotocol/go-sdk",
		Kind:  "github-md",
		URLs: []string{
			"https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/main/README.md",
			"https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/main/docs/quick_start.md",
		},
	}
	out := src.Expand()
	if len(out) != 1 {
		t.Fatalf("expected 1 resolved source, got %d", len(out))
	}
	r := out[0]
	if r.LibID != src.LibID {
		t.Errorf("LibID = %q, want %q", r.LibID, src.LibID)
	}
	if r.BaseLibID != src.LibID {
		t.Errorf("BaseLibID = %q, want %q", r.BaseLibID, src.LibID)
	}
	if r.Version != "" {
		t.Errorf("Version = %q, want empty", r.Version)
	}
	if r.Kind != "github-md" {
		t.Errorf("Kind = %q, want github-md", r.Kind)
	}
	if len(r.URLs) != 2 || r.URLs[0] != src.URLs[0] || r.URLs[1] != src.URLs[1] {
		t.Errorf("URLs = %v, want %v", r.URLs, src.URLs)
	}
}

func TestExpand_MultiVersionExpandsAndSubstitutes(t *testing.T) {
	src := scraper.LibrarySource{
		LibID:    "/facebook/react",
		Kind:     "github-md",
		Versions: []scraper.VersionEntry{{Name: "v18"}, {Name: "v19"}},
		URLs: []string{
			"https://raw.githubusercontent.com/facebook/react/{version}/README.md",
			"https://raw.githubusercontent.com/facebook/react/{version}/docs/getting-started.md",
		},
	}
	out := src.Expand()
	if len(out) != 2 {
		t.Fatalf("expected 2 resolved sources, got %d", len(out))
	}

	// After #113 LibID stays equal to the base for every expansion;
	// the version lives in the dedicated Version field.
	want := []struct {
		libID   string
		version string
		urls    []string
	}{
		{
			libID:   "/facebook/react",
			version: "v18",
			urls: []string{
				"https://raw.githubusercontent.com/facebook/react/v18/README.md",
				"https://raw.githubusercontent.com/facebook/react/v18/docs/getting-started.md",
			},
		},
		{
			libID:   "/facebook/react",
			version: "v19",
			urls: []string{
				"https://raw.githubusercontent.com/facebook/react/v19/README.md",
				"https://raw.githubusercontent.com/facebook/react/v19/docs/getting-started.md",
			},
		},
	}
	for i, w := range want {
		if out[i].LibID != w.libID {
			t.Errorf("[%d].LibID = %q, want %q", i, out[i].LibID, w.libID)
		}
		if out[i].BaseLibID != "/facebook/react" {
			t.Errorf("[%d].BaseLibID = %q, want /facebook/react", i, out[i].BaseLibID)
		}
		if out[i].Version != w.version {
			t.Errorf("[%d].Version = %q, want %q", i, out[i].Version, w.version)
		}
		if len(out[i].URLs) != len(w.urls) {
			t.Fatalf("[%d].URLs len = %d, want %d", i, len(out[i].URLs), len(w.urls))
		}
		for j, u := range w.urls {
			if out[i].URLs[j] != u {
				t.Errorf("[%d].URLs[%d] = %q, want %q", i, j, out[i].URLs[j], u)
			}
		}
	}
}

func TestResolve_FilterByBaseLibID(t *testing.T) {
	cfg := mustLoadInline(t, `
libraries:
  - lib_id: /modelcontextprotocol/go-sdk
    kind: github-md
    urls:
      - https://example.com/go-sdk/README.md
  - lib_id: /facebook/react
    kind: github-md
    versions: [v18, v19]
    urls:
      - https://example.com/react/{version}/README.md
`)

	all := cfg.Resolve("", "")
	if len(all) != 3 {
		t.Fatalf("Resolve(\"\", \"\") returned %d, want 3", len(all))
	}

	react := cfg.Resolve("/facebook/react", "")
	if len(react) != 2 {
		t.Fatalf("Resolve(/facebook/react, \"\") returned %d, want 2", len(react))
	}
	for _, r := range react {
		if r.BaseLibID != "/facebook/react" {
			t.Errorf("filtered entry has BaseLibID %q, want /facebook/react", r.BaseLibID)
		}
	}
}

func TestResolve_FilterByLibAndVersion(t *testing.T) {
	cfg := mustLoadInline(t, `
libraries:
  - lib_id: /facebook/react
    kind: github-md
    versions: [v18, v19]
    urls:
      - https://example.com/react/{version}/README.md
`)

	v18 := cfg.Resolve("/facebook/react", "v18")
	if len(v18) != 1 {
		t.Fatalf("Resolve(/facebook/react, v18) returned %d, want 1", len(v18))
	}
	if v18[0].LibID != "/facebook/react" {
		t.Errorf("LibID = %q, want /facebook/react (base stays unversioned after #113)", v18[0].LibID)
	}
	if v18[0].Version != "v18" {
		t.Errorf("Version = %q, want v18", v18[0].Version)
	}
}

func TestResolve_FilterMatchesSingleVersionLib(t *testing.T) {
	cfg := mustLoadInline(t, `
libraries:
  - lib_id: /modelcontextprotocol/go-sdk
    kind: github-md
    urls:
      - https://example.com/go-sdk/README.md
`)

	got := cfg.Resolve("/modelcontextprotocol/go-sdk", "")
	if len(got) != 1 {
		t.Fatalf("Resolve returned %d, want 1", len(got))
	}
	if got[0].LibID != "/modelcontextprotocol/go-sdk" {
		t.Errorf("LibID = %q", got[0].LibID)
	}
}

func TestResolve_FilterNoMatch(t *testing.T) {
	cfg := mustLoadInline(t, `
libraries:
  - lib_id: /modelcontextprotocol/go-sdk
    kind: github-md
    urls:
      - https://example.com/go-sdk/README.md
`)

	if got := cfg.Resolve("/missing/lib", ""); len(got) != 0 {
		t.Errorf("expected no matches, got %d", len(got))
	}
}

// TestResolve_VersionFilterWithoutMatchingVersion exercises the
// (-lib, -version) pair when the version tag doesn't exist for the
// filtered lib: the slice comes back empty, exactly as the scrape
// command's startup error relies on.
func TestResolve_VersionFilterWithoutMatchingVersion(t *testing.T) {
	cfg := mustLoadInline(t, `
libraries:
  - lib_id: /facebook/react
    kind: github-md
    versions: [v18, v19]
    urls:
      - https://example.com/react/{version}/README.md
`)

	if got := cfg.Resolve("/facebook/react", "v20"); len(got) != 0 {
		t.Errorf("expected no matches for v20, got %d", len(got))
	}
}

// TestResolve_MultiVersionLibIDStaysBase is the regression test for
// the un-concatenation: every expansion of a multi-version lib must
// carry the base as LibID with the version in the Version field,
// never the old /<base>/<version> concat.
func TestResolve_MultiVersionLibIDStaysBase(t *testing.T) {
	cfg := mustLoadInline(t, `
libraries:
  - lib_id: /hashicorp/terraform
    kind: github-md
    versions: [v1.14, v1.13]
    urls:
      - https://example.com/tf/{version}/README.md
`)

	got := cfg.Resolve("", "")
	if len(got) != 2 {
		t.Fatalf("Resolve returned %d, want 2", len(got))
	}
	for i, r := range got {
		if r.LibID != "/hashicorp/terraform" {
			t.Errorf("[%d].LibID = %q, want /hashicorp/terraform", i, r.LibID)
		}
		if r.BaseLibID != "/hashicorp/terraform" {
			t.Errorf("[%d].BaseLibID = %q, want /hashicorp/terraform", i, r.BaseLibID)
		}
	}
	if got[0].Version != "v1.14" || got[1].Version != "v1.13" {
		t.Errorf("versions = %q, %q; want v1.14, v1.13", got[0].Version, got[1].Version)
	}
}

// mustLoadInline writes a YAML body to a temp file and loads it, failing
// the test on any parse/validate error. Keeps each Resolve test focused
// on the filter logic rather than file plumbing.
func mustLoadInline(t *testing.T, body string) *scraper.Config {
	t.Helper()
	cfg, err := scraper.LoadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// --- {ref} pinning (#103) ---

func TestLoadConfig_RefRules(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "single-version URL has {ref} but ref is unset",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    urls:
      - https://example.com/{ref}/a.md
`,
			want: "ref is not set",
		},
		{
			name: "multi-version URL has {ref} but no ref anywhere",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    versions: [v1, v2]
    urls:
      - https://example.com/{version}/{ref}/a.md
`,
			want: "neither versions",
		},
		{
			name: "ref contains whitespace",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    ref: "v1 0"
    urls:
      - https://example.com/{ref}/a.md
`,
			want: "whitespace",
		},
		{
			name: "per-version ref contains whitespace",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    versions:
      v1: { ref: "tag with space" }
    urls:
      - https://example.com/{version}/{ref}/a.md
`,
			want: "whitespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.yaml)
			_, err := scraper.LoadConfig(path)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoadConfig_RefSetButURLHasNoPlaceholderIsAllowed(t *testing.T) {
	// Back-compat: a lib may declare ref: for documentation/future use
	// even if no URL substitutes it yet. The scraper does not error.
	cfg := mustLoadInline(t, `
libraries:
  - lib_id: /org/project
    kind: github-md
    ref: v1.0.0
    urls:
      - https://example.com/main/a.md
`)
	got := cfg.Resolve("", "")
	if len(got) != 1 {
		t.Fatalf("Resolve returned %d, want 1", len(got))
	}
	if got[0].Ref != "v1.0.0" {
		t.Errorf("ResolvedSource.Ref = %q, want v1.0.0", got[0].Ref)
	}
	if got[0].URLs[0] != "https://example.com/main/a.md" {
		t.Errorf("URL was rewritten unexpectedly: %q", got[0].URLs[0])
	}
}

func TestExpand_SingleVersionSubstitutesRef(t *testing.T) {
	src := scraper.LibrarySource{
		LibID: "/python/cpython",
		Kind:  scraper.KindGithubRST,
		Ref:   "v3.13.1",
		URLs: []string{
			"https://raw.githubusercontent.com/python/cpython/{ref}/Doc/library/os.rst",
			"https://raw.githubusercontent.com/python/cpython/{ref}/Doc/library/sys.rst",
		},
	}
	out := src.Expand()
	if len(out) != 1 {
		t.Fatalf("Expand returned %d, want 1", len(out))
	}
	r := out[0]
	if r.Ref != "v3.13.1" {
		t.Errorf("Ref = %q, want v3.13.1", r.Ref)
	}
	want := []string{
		"https://raw.githubusercontent.com/python/cpython/v3.13.1/Doc/library/os.rst",
		"https://raw.githubusercontent.com/python/cpython/v3.13.1/Doc/library/sys.rst",
	}
	for i, u := range want {
		if r.URLs[i] != u {
			t.Errorf("URLs[%d] = %q, want %q", i, r.URLs[i], u)
		}
	}
}

func TestLoadConfig_VersionsMapShape(t *testing.T) {
	cfg := mustLoadInline(t, `
libraries:
  - lib_id: /hashicorp/terraform
    kind: github-md
    versions:
      v1.14: { ref: v1.14.6 }
      v1.13: { ref: v1.13.5 }
    urls:
      - https://raw.githubusercontent.com/hashicorp/web-unified-docs/{ref}/content/terraform/{version}.x/docs/intro/index.mdx
`)
	got := cfg.Resolve("", "")
	if len(got) != 2 {
		t.Fatalf("Resolve returned %d, want 2", len(got))
	}
	// Declaration order is preserved. LibID stays the base after #113;
	// the version lives in Version.
	want := []struct {
		libID   string
		version string
		ref     string
		url     string
	}{
		{
			libID:   "/hashicorp/terraform",
			version: "v1.14",
			ref:     "v1.14.6",
			url:     "https://raw.githubusercontent.com/hashicorp/web-unified-docs/v1.14.6/content/terraform/v1.14.x/docs/intro/index.mdx",
		},
		{
			libID:   "/hashicorp/terraform",
			version: "v1.13",
			ref:     "v1.13.5",
			url:     "https://raw.githubusercontent.com/hashicorp/web-unified-docs/v1.13.5/content/terraform/v1.13.x/docs/intro/index.mdx",
		},
	}
	for i, w := range want {
		if got[i].LibID != w.libID {
			t.Errorf("[%d].LibID = %q, want %q", i, got[i].LibID, w.libID)
		}
		if got[i].Version != w.version {
			t.Errorf("[%d].Version = %q, want %q", i, got[i].Version, w.version)
		}
		if got[i].Ref != w.ref {
			t.Errorf("[%d].Ref = %q, want %q", i, got[i].Ref, w.ref)
		}
		if got[i].URLs[0] != w.url {
			t.Errorf("[%d].URLs[0] = %q, want %q", i, got[i].URLs[0], w.url)
		}
	}
}

// --- per-version URL overrides (#115) ---

func TestExpand_PerVersionURLsOverrideBaseline(t *testing.T) {
	cfg := mustLoadInline(t, `
libraries:
  - lib_id: /modelcontextprotocol/go-sdk
    kind: github-md
    urls:
      - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/{version}/README.md
      - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/{version}/docs/server.md
    versions:
      v1.4.1: {}
      v1.5.0:
        urls:
          - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/{version}/README.md
          - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/{version}/docs/server.md
          - https://raw.githubusercontent.com/modelcontextprotocol/go-sdk/{version}/docs/quick_start.md
`)
	got := cfg.Resolve("", "")
	if len(got) != 2 {
		t.Fatalf("Resolve returned %d, want 2", len(got))
	}
	if got[0].Version != "v1.4.1" {
		t.Fatalf("[0].Version = %q, want v1.4.1", got[0].Version)
	}
	if len(got[0].URLs) != 2 {
		t.Errorf("v1.4.1 should inherit baseline (2 URLs), got %d", len(got[0].URLs))
	}
	for _, u := range got[0].URLs {
		if !strings.Contains(u, "/v1.4.1/") {
			t.Errorf("v1.4.1 URL not substituted: %q", u)
		}
	}
	if got[1].Version != "v1.5.0" {
		t.Fatalf("[1].Version = %q, want v1.5.0", got[1].Version)
	}
	if len(got[1].URLs) != 3 {
		t.Errorf("v1.5.0 should use override (3 URLs), got %d", len(got[1].URLs))
	}
	if !strings.HasSuffix(got[1].URLs[2], "/docs/quick_start.md") {
		t.Errorf("v1.5.0 URL[2] = %q, want …/docs/quick_start.md", got[1].URLs[2])
	}
}

func TestExpand_NoBaselineAllVersionsOverride(t *testing.T) {
	cfg := mustLoadInline(t, `
libraries:
  - lib_id: /org/project
    kind: github-md
    ref: fallback-ref
    versions:
      v1:
        ref: r1
        urls:
          - https://example.com/{version}/{ref}/a.md
      v2:
        urls:
          - https://example.com/{version}/{ref}/a.md
          - https://example.com/{version}/{ref}/b.md
`)
	got := cfg.Resolve("", "")
	if len(got) != 2 {
		t.Fatalf("Resolve returned %d, want 2", len(got))
	}
	if got[0].URLs[0] != "https://example.com/v1/r1/a.md" {
		t.Errorf("v1 URL = %q", got[0].URLs[0])
	}
	if len(got[1].URLs) != 2 {
		t.Fatalf("v2 URLs len = %d, want 2", len(got[1].URLs))
	}
	if got[1].URLs[0] != "https://example.com/v2/fallback-ref/a.md" {
		t.Errorf("v2 URLs[0] = %q", got[1].URLs[0])
	}
	if got[1].URLs[1] != "https://example.com/v2/fallback-ref/b.md" {
		t.Errorf("v2 URLs[1] = %q", got[1].URLs[1])
	}
}

func TestLoadConfig_PerVersionURLsRules(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "per-version urls is an explicit empty list",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    urls:
      - https://example.com/{version}/a.md
    versions:
      v1: { urls: [] }
`,
			want: "empty list",
		},
		{
			name: "per-version url missing {version} placeholder",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    urls:
      - https://example.com/{version}/a.md
    versions:
      v1:
        urls:
          - https://example.com/fixed/a.md
`,
			want: "missing the {version} placeholder",
		},
		{
			name: "inheriting version with empty baseline",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    versions:
      v1:
        urls:
          - https://example.com/{version}/a.md
      v2: {}
`,
			want: `versions["v2"] has no urls and the top-level urls is empty`,
		},
		{
			name: "per-version urls not a list",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    urls:
      - https://example.com/{version}/a.md
    versions:
      v1:
        urls: "https://example.com/{version}/a.md"
`,
			want: "must be a list",
		},
		{
			name: "per-version url with whitespace entry",
			yaml: `
libraries:
  - lib_id: /org/project
    kind: github-md
    urls:
      - https://example.com/{version}/a.md
    versions:
      v1:
        urls:
          - "   "
`,
			want: "contains an empty entry",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.yaml)
			_, err := scraper.LoadConfig(path)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoadConfig_PerVersionURLsAcceptMixedInheritAndOverride(t *testing.T) {
	cfg := mustLoadInline(t, `
libraries:
  - lib_id: /org/project
    kind: github-md
    urls:
      - https://example.com/{version}/a.md
    versions:
      v1: {}
      v2:
        urls:
          - https://example.com/{version}/a.md
          - https://example.com/{version}/b.md
`)
	got := cfg.Resolve("", "")
	if len(got) != 2 {
		t.Fatalf("Resolve returned %d, want 2", len(got))
	}
	if len(got[0].URLs) != 1 || got[0].URLs[0] != "https://example.com/v1/a.md" {
		t.Errorf("v1 URLs = %v, want [https://example.com/v1/a.md]", got[0].URLs)
	}
	if len(got[1].URLs) != 2 {
		t.Fatalf("v2 URLs len = %d, want 2", len(got[1].URLs))
	}
	if got[1].URLs[1] != "https://example.com/v2/b.md" {
		t.Errorf("v2 URLs[1] = %q", got[1].URLs[1])
	}
}

func TestLoadConfig_VersionsMapShape_PerVersionRefOverridesTopLevel(t *testing.T) {
	cfg := mustLoadInline(t, `
libraries:
  - lib_id: /org/project
    kind: github-md
    ref: fallback-ref
    versions:
      v1: { ref: per-version-ref }
      v2: {}
    urls:
      - https://example.com/{version}/{ref}/a.md
`)
	got := cfg.Resolve("", "")
	if len(got) != 2 {
		t.Fatalf("Resolve returned %d, want 2", len(got))
	}
	if got[0].Ref != "per-version-ref" {
		t.Errorf("v1 Ref = %q, want per-version-ref", got[0].Ref)
	}
	if got[1].Ref != "fallback-ref" {
		t.Errorf("v2 Ref = %q, want fallback-ref (top-level fallback)", got[1].Ref)
	}
	if got[0].URLs[0] != "https://example.com/v1/per-version-ref/a.md" {
		t.Errorf("v1 URL = %q", got[0].URLs[0])
	}
	if got[1].URLs[0] != "https://example.com/v2/fallback-ref/a.md" {
		t.Errorf("v2 URL = %q", got[1].URLs[0])
	}
}
