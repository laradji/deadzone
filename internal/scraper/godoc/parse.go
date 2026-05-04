// Package godoc parses Go source trees via go/parser + go/doc and
// emits one db.Doc per exported identifier. It is the parse half of
// kind: godoc; the source-bytes half (proxy.golang.org / GitHub
// Contents API) lives in fetch.go. Design + ACs: #198.
package godoc

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/laradji/deadzone/internal/db"
)

// ParseGodoc walks srcDir, parses every Go package found, and emits
// one db.Doc per exported identifier. Test files (`_test.go`) and
// vendored deps (`vendor/`) are skipped. Empty subdirectories are
// ignored.
//
// libID is stamped onto every emitted Doc (db.Doc.LibID). The chunk
// shape contract is:
//
//   - Package overview: Title="<pkg> package"
//   - Func:             Title="<pkg>.<Name>"
//   - Type:             Title="<pkg>.<Name>" (methods/ctors rolled into Content)
//   - Const/Var group:  Title="<pkg>.<first-name> ..."
//
// TestParseGodoc_GoldenFixture in parse_test.go locks this contract
// against testdata/sample.golden — that's the canonical source of
// truth, edits here must keep the golden in sync (regenerate via
// UPDATE_GOLDEN=1 go test ./internal/scraper/godoc/).
//
// The output is deterministic: packages are visited in directory-sort
// order, and within each package idents are sorted by name. Declaration
// order from go/doc is preserved only within a single const/var group
// (which is itself one chunk).
func ParseGodoc(libID, srcDir string) ([]db.Doc, error) {
	var pkgDirs []string
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != srcDir && (name == "vendor" || strings.HasPrefix(name, ".") || name == "testdata") {
			return fs.SkipDir
		}
		hasGo, err := dirHasGoFiles(path)
		if err != nil {
			return err
		}
		if hasGo {
			pkgDirs = append(pkgDirs, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", srcDir, err)
	}
	sort.Strings(pkgDirs)

	var docs []db.Doc
	for _, dir := range pkgDirs {
		pkgDocs, err := parsePackageDir(libID, dir)
		if err != nil {
			return nil, err
		}
		docs = append(docs, pkgDocs...)
	}
	return docs, nil
}

func dirHasGoFiles(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			return true, nil
		}
	}
	return false, nil
}

func parsePackageDir(libID, dir string) ([]db.Doc, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		// Filter is "include if true" — drop _test.go.
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse dir %s: %w", dir, err)
	}

	// Map iteration is non-deterministic; sort package names for
	// reproducible chunk order across runs.
	names := make([]string, 0, len(pkgs))
	for n := range pkgs {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []db.Doc
	for _, name := range names {
		pkg := pkgs[name]
		// go/parser sometimes synthesizes a "_test" external test
		// package even when we filter _test.go — defensive skip.
		if strings.HasSuffix(name, "_test") {
			continue
		}
		// importPath is opaque to doc.New beyond cross-reference
		// rendering, which we don't surface; pass the short name.
		d := doc.New(pkg, name, doc.AllDecls)
		out = append(out, packageOverview(libID, d)...)
		out = append(out, funcChunks(libID, d, fset, d.Funcs)...)
		out = append(out, typeChunks(libID, d, fset)...)
		out = append(out, valueChunks(libID, d, fset, d.Consts, "const")...)
		out = append(out, valueChunks(libID, d, fset, d.Vars, "var")...)
	}
	return out, nil
}

func packageOverview(libID string, d *doc.Package) []db.Doc {
	body := strings.TrimSpace(d.Doc)
	// Notes (BUG/TODO) — roll up under the package overview so
	// they're discoverable but don't fragment into N tiny chunks.
	if len(d.Notes) > 0 {
		var notes []string
		for marker, items := range d.Notes {
			for _, n := range items {
				notes = append(notes, fmt.Sprintf("%s(%s): %s", marker, n.UID, strings.TrimSpace(n.Body)))
			}
		}
		sort.Strings(notes)
		if body != "" {
			body += "\n\n"
		}
		body += "Notes:\n" + strings.Join(notes, "\n")
	}
	if body == "" {
		// No package-level comment AND no notes — skip the chunk
		// rather than emit an empty body. The downstream embedder
		// drops empty content too, but skipping here keeps logs clean.
		return nil
	}
	return []db.Doc{{
		LibID:   libID,
		Title:   d.Name + " package",
		Content: body,
	}}
}

func funcChunks(libID string, d *doc.Package, fset *token.FileSet, fns []*doc.Func) []db.Doc {
	if len(fns) == 0 {
		return nil
	}
	sorted := append([]*doc.Func(nil), fns...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var out []db.Doc
	for _, fn := range sorted {
		if !ast.IsExported(fn.Name) {
			continue
		}
		sig := printFuncSignature(fset, fn.Decl)
		body := strings.TrimSpace(fn.Doc)
		content := sig
		if body != "" {
			content = body + "\n\n" + sig
		}
		out = append(out, db.Doc{
			LibID:   libID,
			Title:   d.Name + "." + fn.Name,
			Content: content,
		})
	}
	return out
}

func typeChunks(libID string, d *doc.Package, fset *token.FileSet) []db.Doc {
	if len(d.Types) == 0 {
		return nil
	}
	sorted := append([]*doc.Type(nil), d.Types...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var out []db.Doc
	for _, t := range sorted {
		if !ast.IsExported(t.Name) {
			continue
		}
		decl := printNode(fset, t.Decl)
		body := strings.TrimSpace(t.Doc)
		var content strings.Builder
		if body != "" {
			content.WriteString(body)
			content.WriteString("\n\n")
		}
		content.WriteString(decl)

		// Constructors and methods rolled into the type chunk so
		// `<pkg>.<Type>` retrieval surfaces the whole API surface
		// in one hit — matches how pkg.go.dev structures type pages.
		writeAssoc := func(label string, items []*doc.Func) {
			var exported []*doc.Func
			for _, fn := range items {
				if ast.IsExported(fn.Name) {
					exported = append(exported, fn)
				}
			}
			if len(exported) == 0 {
				return
			}
			sort.Slice(exported, func(i, j int) bool { return exported[i].Name < exported[j].Name })
			content.WriteString("\n\n## ")
			content.WriteString(label)
			content.WriteString("\n")
			for _, fn := range exported {
				content.WriteString("\n### ")
				content.WriteString(fn.Name)
				content.WriteString("\n\n")
				content.WriteString(printFuncSignature(fset, fn.Decl))
				if fnDoc := strings.TrimSpace(fn.Doc); fnDoc != "" {
					content.WriteString("\n\n")
					content.WriteString(fnDoc)
				}
				content.WriteString("\n")
			}
		}
		writeAssoc("Constructors", t.Funcs)
		writeAssoc("Methods", t.Methods)

		out = append(out, db.Doc{
			LibID:   libID,
			Title:   d.Name + "." + t.Name,
			Content: strings.TrimRight(content.String(), "\n"),
		})
	}
	return out
}

func valueChunks(libID string, d *doc.Package, fset *token.FileSet, vals []*doc.Value, _ string) []db.Doc {
	if len(vals) == 0 {
		return nil
	}
	var out []db.Doc
	for _, v := range vals {
		var exportedNames []string
		for _, n := range v.Names {
			if ast.IsExported(n) {
				exportedNames = append(exportedNames, n)
			}
		}
		if len(exportedNames) == 0 {
			continue
		}
		title := d.Name + "." + exportedNames[0]
		if len(exportedNames) > 1 {
			title += " ..."
		}
		decl := printNode(fset, v.Decl)
		body := strings.TrimSpace(v.Doc)
		content := decl
		if body != "" {
			content = body + "\n\n" + decl
		}
		out = append(out, db.Doc{
			LibID:   libID,
			Title:   title,
			Content: content,
		})
	}
	return out
}

func printNode(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}

// printFuncSignature renders the func declaration without its body,
// which is what callers want in chunk content (signature is the API
// contract; the body is implementation detail).
func printFuncSignature(fset *token.FileSet, decl *ast.FuncDecl) string {
	if decl == nil {
		return ""
	}
	clone := *decl
	clone.Body = nil
	return printNode(fset, &clone)
}
