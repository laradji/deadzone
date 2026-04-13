// preprocess_rst.go is the github-rst counterpart to scraper.go's
// ParseMarkdown / FetchOne. It mirrors the markdown fast path
// (HTTP GET → parse → []db.Doc) for libraries whose docs live as
// reStructuredText in the source repo (cpython, Django, NumPy, …).
//
// Implementation choice: hand-rolled, ~150 LOC, no external dep. As of
// 2026-04-13 (when #95 landed), no actively maintained Apache/MIT
// pure-Go RST library covered both section detection and code-block
// preservation well enough to justify the dependency cost. The
// rejection criteria and the search are documented in #95. Coverage is
// deliberately partial: extract enough RST to feed an embedder usefully
// (headings, prose, code blocks, cross-refs collapsed to plain text)
// but do NOT round-trip to RST or render to HTML. Sphinx extensions,
// Jinja interpolation, and exotic directives degrade gracefully to
// opaque text rather than failing the parse.

package scraper

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/laradji/deadzone/internal/db"
)

// rstAdornmentChars is the set of single-character "adornments" that
// docutils accepts as section underlines. Any of these, repeated for
// the full line, makes a heading underline.
const rstAdornmentChars = `=-~^"'_*+#:.<>`

// crossRefRE matches a Sphinx interpreted-text role: one or more colon-
// delimited role names, then a single backtick-delimited target. We
// collapse to the visible text — `:func:` "`os.path.join`" → "os.path.join",
// `:ref:` "`title <label>`" → "title".
var crossRefRE = regexp.MustCompile(":[a-zA-Z][\\w:+-]*:`([^`]+)`")

// labelTargetRE matches a hyperlink target: `.. _name:` with no body on
// the same line. These are pure cross-ref machinery and add no signal
// for an embedder, so we drop them.
var labelTargetRE = regexp.MustCompile(`^\s*\.\. _[^:]+:\s*$`)

// codeBlockDirectiveRE matches the three directive openers Sphinx uses
// for verbatim code: `.. code-block::`, `.. sourcecode::`, `.. code::`.
// The optional language argument after `::` is captured but unused —
// we just need the indent of the directive line to know where the
// indented body ends.
var codeBlockDirectiveRE = regexp.MustCompile(`^(\s*)\.\. (code-block|sourcecode|code)::`)

// ParseRST chunks a reStructuredText document into db.Doc entries, one
// per section heading. Mirrors ParseMarkdown's contract for downstream
// pipeline compatibility — see scraper.go for the markup-agnostic Doc
// shape.
//
// Rules:
//   - A section heading is any non-empty line followed by an underline
//     made of repeated rstAdornmentChars (>=3 chars). Overline form is
//     not detected — cpython/Django/NumPy stdlib docs use underline only.
//   - One Doc per heading; nested subsections become flat sibling Docs
//     (no parent linkage), same as ParseMarkdown's H2 split.
//   - Doc.Title is the heading text without the underline characters.
//   - Code blocks (`.. code-block:: lang`, `.. sourcecode::`, `.. code::`,
//     and trailing `::` literal blocks) are preserved verbatim by
//     indentation tracking.
//   - Sphinx noise: `.. _label:` targets dropped; `:role:` cross-refs
//     collapsed to their visible text.
//   - Unknown `.. directive::` lines are kept verbatim so a downstream
//     search query for "seealso" still has signal.
//   - Text before the first heading is emitted as a single Doc titled
//     sourceName, matching ParseMarkdown's preamble behavior.
func ParseRST(libID, sourceName, content string) []db.Doc {
	lines := strings.Split(content, "\n")

	var (
		docs             []db.Doc
		currentTitle     string
		currentBuf       strings.Builder
		seenFirstHeading bool
	)

	flush := func(title string) {
		body := strings.TrimSpace(currentBuf.String())
		if body != "" {
			docs = append(docs, db.Doc{LibID: libID, Title: title, Content: body})
		}
		currentBuf.Reset()
	}

	// Literal block tracking. Once entered (via "::" or a code-block
	// directive), every subsequent line whose indent strictly exceeds
	// literalBaseIndent (or is blank) is preserved verbatim. The block
	// ends on the first non-blank line at or below the base indent.
	inLiteral := false
	literalBaseIndent := -1

	i := 0
	for i < len(lines) {
		line := lines[i]

		if inLiteral {
			if strings.TrimSpace(line) == "" {
				currentBuf.WriteString(line)
				currentBuf.WriteByte('\n')
				i++
				continue
			}
			if leadingSpaces(line) > literalBaseIndent {
				currentBuf.WriteString(line)
				currentBuf.WriteByte('\n')
				i++
				continue
			}
			inLiteral = false
			literalBaseIndent = -1
			// fall through into normal-line handling for this line
		}

		// Section heading: title line + adornment underline below.
		if i+1 < len(lines) && isRSTUnderlineFor(lines[i+1], line) {
			title := transformRSTLine(strings.TrimSpace(line))
			if !seenFirstHeading {
				flush(sourceName)
				seenFirstHeading = true
			} else {
				flush(currentTitle)
			}
			currentTitle = title
			i += 2
			continue
		}

		// Drop hyperlink-target stubs.
		if labelTargetRE.MatchString(line) {
			i++
			continue
		}

		// `.. code-block:: lang` (and friends) — preserve the directive
		// header line and switch into literal mode bounded by its indent.
		if m := codeBlockDirectiveRE.FindStringSubmatch(line); m != nil {
			currentBuf.WriteString(line)
			currentBuf.WriteByte('\n')
			inLiteral = true
			literalBaseIndent = len(m[1])
			i++
			continue
		}

		// Trailing `::` introduces an indented literal block. Keep the
		// introducer line as-is and switch to literal mode bounded by
		// the line's own indent.
		trimmedRight := strings.TrimRight(line, " \t")
		if strings.HasSuffix(trimmedRight, "::") && !strings.HasPrefix(strings.TrimSpace(line), "..") {
			currentBuf.WriteString(transformRSTLine(line))
			currentBuf.WriteByte('\n')
			inLiteral = true
			literalBaseIndent = leadingSpaces(line)
			i++
			continue
		}

		currentBuf.WriteString(transformRSTLine(line))
		currentBuf.WriteByte('\n')
		i++
	}

	if !seenFirstHeading {
		flush(sourceName)
	} else {
		flush(currentTitle)
	}

	return docs
}

// transformRSTLine collapses Sphinx cross-references to their visible
// text. Plain prose passes through unchanged. Called per-line outside
// of literal blocks so source code in `.. code-block::` bodies stays
// byte-for-byte identical to the input.
func transformRSTLine(line string) string {
	return crossRefRE.ReplaceAllStringFunc(line, func(m string) string {
		sub := crossRefRE.FindStringSubmatch(m)
		text := sub[1]
		// `:ref:` "`title <label>`" form — keep the visible title.
		if idx := strings.LastIndex(text, "<"); idx > 0 && strings.HasSuffix(text, ">") {
			return strings.TrimSpace(text[:idx])
		}
		return text
	})
}

// leadingSpaces counts leading-whitespace columns. Tabs are expanded to
// 8 columns to match docutils' default tab width — RST itself is
// indentation-significant only relative to the first non-blank line of
// a block, so any consistent expansion works as long as it's stable.
func leadingSpaces(line string) int {
	n := 0
	for _, r := range line {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 8
		default:
			return n
		}
	}
	return n
}

// isRSTUnderlineFor reports whether `underline` is a valid section
// adornment for `title`. Requirements: non-empty title, underline made
// of >=3 repetitions of one rstAdornmentChars char, and underline
// length within 2 of the title length (docutils requires >=, but RST
// authors sometimes underline a hair short; the tolerance avoids
// missing real headings without producing false positives on prose
// that happens to contain dashes).
func isRSTUnderlineFor(underline, title string) bool {
	u := strings.TrimRight(underline, " \t")
	if len(u) < 3 {
		return false
	}
	first := u[0]
	if !strings.ContainsRune(rstAdornmentChars, rune(first)) {
		return false
	}
	for i := 0; i < len(u); i++ {
		if u[i] != first {
			return false
		}
	}
	titleLen := len(strings.TrimSpace(title))
	if titleLen == 0 {
		return false
	}
	return len(u)+2 >= titleLen
}

// FetchOneViaGithubRST downloads a single raw .rst URL and parses it
// into docs. Mirrors FetchOne's contract; only the parser differs.
//
// Like the markdown fast path, this path does no content-type check —
// raw.githubusercontent.com serves text/plain for .rst and the parser
// is robust to whatever bytes it gets.
func FetchOneViaGithubRST(ctx context.Context, client *http.Client, libID, url string) (FetchOneResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return FetchOneResult{}, fmt.Errorf("build request %s: %w", url, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return FetchOneResult{}, fmt.Errorf("fetch %s: %w", url, err)
	}

	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return FetchOneResult{}, fmt.Errorf("read body %s: %w", url, readErr)
	}

	if resp.StatusCode != http.StatusOK {
		return FetchOneResult{}, &HTTPStatusError{Status: resp.StatusCode, URL: url}
	}

	sourceName := strings.TrimSuffix(path.Base(url), ".rst")
	docs := ParseRST(libID, sourceName, string(body))
	return FetchOneResult{Docs: docs, Bytes: len(body)}, nil
}
