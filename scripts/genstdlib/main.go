// Command genstdlib walks the local Go toolchain's GOROOT/src and
// emits libraries_sources.yaml entries for every importable Go stdlib
// package. One lib_id per package (e.g. /golang/go/encoding/json),
// description = first-sentence synopsis + alphabetical "Key APIs:" list
// of the top exported names so search_libraries embeds rich enough text
// to discriminate between packages.
//
// Run via:
//
//	just gen-stdlib                                  # writes to stdout
//	just gen-stdlib > /tmp/stdlib.yaml               # capture
//	mise exec -- go run scripts/genstdlib/main.go    # without justfile
//
// The output is meant to be spliced into libraries_sources.yaml under
// the "── kind: godoc ──" section. Re-run on a Go version bump (the ref
// embedded in each entry comes from runtime.Version()).
//
// Skipped paths (not part of the user-facing stdlib):
//   - cmd/        Go toolchain (compile, link, go build, …)
//   - internal/   anywhere — Go convention for non-importable code
//   - vendor/     vendored deps
//   - builtin/    pseudo-package, no real source
//   - testdata/   test fixtures (defensive — none should land in src/)
//
// Description shape (per package):
//
//	"<doc.Synopsis>. Key APIs: A, B, C, D, E, F, G, H."
//
// where Key APIs is alphabetical, types first, funcs after, capped at 8
// total so the description stays embeddable without dwarfing the
// synopsis.
package main

import (
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	libIDPrefix = "/golang/go/"
	urlTemplate = "https://api.github.com/repos/golang/go/contents/src/%s?ref={ref}"
	maxAPIs     = 8
)

func main() {
	ref := runtime.Version() // e.g. "go1.26.2"
	if len(os.Args) >= 2 && strings.TrimSpace(os.Args[1]) != "" {
		ref = strings.TrimSpace(os.Args[1])
	}

	goroot := runtime.GOROOT()
	if goroot == "" {
		fmt.Fprintln(os.Stderr, "error: runtime.GOROOT() returned empty")
		os.Exit(1)
	}
	src := filepath.Join(goroot, "src")

	pkgs, err := walkStdlib(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "found %d stdlib packages under %s\n", len(pkgs), src)

	emitHeader(ref, len(pkgs))
	for _, pkg := range pkgs {
		desc := buildDescription(filepath.Join(src, pkg))
		emitEntry(pkg, desc, ref)
	}
}

// walkStdlib returns the relative paths (under src) of every Go stdlib
// package that has at least one non-test .go file and isn't filtered
// out by the skip rules listed at the top of this file.
func walkStdlib(src string) ([]string, error) {
	var pkgs []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if shouldSkipDir(rel) {
			return fs.SkipDir
		}
		if dirHasGoSources(path) {
			pkgs = append(pkgs, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(pkgs)
	return pkgs, nil
}

func shouldSkipDir(rel string) bool {
	first := strings.SplitN(rel, "/", 2)[0]
	switch first {
	case "cmd", "vendor", "builtin", "testdata":
		return true
	}
	if rel == "internal" || strings.HasPrefix(rel, "internal/") || strings.Contains(rel, "/internal/") || strings.HasSuffix(rel, "/internal") {
		return true
	}
	if strings.Contains(rel, "/testdata/") || strings.HasSuffix(rel, "/testdata") {
		return true
	}
	// Go convention: directories prefixed with `_` are excluded by
	// `go build` and friends. They typically hold code-generation
	// helpers (e.g. runtime/_gen/, simd/archsimd/_gen/simdgen) that
	// exist in the local toolchain checkout but are NOT shipped on
	// github.com/golang/go's src/ tree — including them produces 404s
	// at scrape time. See https://pkg.go.dev/cmd/go#hdr-Package_lists.
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, "_") {
			return true
		}
	}
	return false
}

func dirHasGoSources(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			return true
		}
	}
	return false
}

// buildDescription parses the package at dir and returns a one-line
// description of the form "<synopsis>. Key APIs: A, B, C, …".
//
// Falls back to a generic "Go standard library package <name>." if the
// package has no doc comment or can't be parsed.
func buildDescription(dir string) string {
	pkgName := filepath.Base(dir)
	relFromSrc := pathSuffix(dir, "/src/")
	fset := token.NewFileSet()
	parsed, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil || len(parsed) == 0 {
		return fmt.Sprintf("Go standard library package %s.", pkgName)
	}

	for name, astPkg := range parsed {
		if strings.HasSuffix(name, "_test") {
			continue
		}
		d := doc.New(astPkg, relFromSrc, doc.AllDecls)
		synopsis := strings.TrimSpace(doc.Synopsis(d.Doc))
		apis := topAPIs(d, maxAPIs)
		switch {
		case synopsis != "" && len(apis) > 0:
			return synopsis + " Key APIs: " + strings.Join(apis, ", ") + "."
		case synopsis != "":
			return synopsis
		case len(apis) > 0:
			return fmt.Sprintf("Go standard library package %s. Key APIs: %s.", pkgName, strings.Join(apis, ", "))
		default:
			return fmt.Sprintf("Go standard library package %s.", pkgName)
		}
	}
	return fmt.Sprintf("Go standard library package %s.", pkgName)
}

// topAPIs returns up to n exported identifier names from a parsed
// doc.Package, types first (alphabetical) then funcs (alphabetical).
// Methods/ctors are NOT listed (they live under their parent type).
// Const/var groups are skipped (their names are usually less
// search-relevant than types and funcs).
func topAPIs(d *doc.Package, n int) []string {
	var types, funcs []string
	for _, t := range d.Types {
		if ast.IsExported(t.Name) {
			types = append(types, t.Name)
		}
	}
	for _, f := range d.Funcs {
		if ast.IsExported(f.Name) {
			funcs = append(funcs, f.Name)
		}
	}
	sort.Strings(types)
	sort.Strings(funcs)

	out := make([]string, 0, n)
	out = append(out, types...)
	out = append(out, funcs...)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// pathSuffix returns the substring of path after the LAST occurrence
// of marker, with marker stripped. Used to get a stable import-path-
// like string ("encoding/json") from an absolute filesystem path
// (".../go/src/encoding/json").
func pathSuffix(path, marker string) string {
	idx := strings.LastIndex(path, marker)
	if idx < 0 {
		return filepath.Base(path)
	}
	return path[idx+len(marker):]
}

func emitHeader(ref string, count int) {
	fmt.Println("  # ── Go stdlib (auto-generated by scripts/genstdlib/main.go) ──")
	fmt.Printf("  # %d packages, ref: %s. Re-run on Go version bump:\n", count, ref)
	fmt.Println("  #   just gen-stdlib > /tmp/stdlib.yaml && splice this section into")
	fmt.Println("  # libraries_sources.yaml under '── kind: godoc ──'.")
	fmt.Println("  # Description format: <synopsis> Key APIs: <up to 8 exported names>")
	fmt.Println()
}

func emitEntry(pkg, description, ref string) {
	fmt.Printf("  - lib_id: %s%s\n", libIDPrefix, pkg)
	fmt.Printf("    description: %s\n", yamlString(description))
	fmt.Printf("    kind: godoc\n")
	fmt.Printf("    ref: %s\n", ref)
	fmt.Printf("    urls:\n")
	fmt.Printf("      - "+urlTemplate+"\n", pkg)
	fmt.Println()
}

// yamlString returns a YAML-safe rendering of s as a flow scalar.
//
// The Go stdlib synopsis sentences are typically clean ASCII text
// with no YAML-significant characters, so most of the time we emit
// the bare string. We escape into double-quoted form whenever we
// detect anything that would need quoting:
//   - leading character that would start a block sequence/mapping
//     ('-', '?', ':', '[', '{', '#', '&', '*', '!', '|', '>', '"')
//   - "; " — interferes with implicit-mapping parsers
//   - leading/trailing whitespace
//   - control chars
//
// In double-quoted form we escape backslashes and double quotes.
func yamlString(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if needsYAMLQuoting(s) {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		return `"` + s + `"`
	}
	return s
}

func needsYAMLQuoting(s string) bool {
	if s == "" {
		return true
	}
	switch s[0] {
	case '-', '?', ':', '[', '{', '#', '&', '*', '!', '|', '>', '"', '\'', '%', '@', '`':
		return true
	}
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "? ") {
		return true
	}
	// YAML 1.2 forbids unquoted ": " (colon-space) inside a plain
	// scalar — gopkg.in/yaml.v3 raises "mapping values are not allowed
	// in this context". Our "Key APIs: A, B, C." descriptions trigger
	// this. Same for " #" (mid-string comment) which strips trailing
	// content silently.
	if strings.Contains(s, ": ") || strings.Contains(s, " #") {
		return true
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
