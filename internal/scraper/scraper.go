// Package scraper fetches and parses documentation pages for indexing.
package scraper

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/laradji/deadzone/internal/db"
)

// Source describes a library's documentation to scrape.
type Source struct {
	LibID string   // e.g. "/modelcontextprotocol/go-sdk"
	URLs  []string // raw markdown URLs to fetch
}

// ParseMarkdown splits raw markdown content into db.Doc values by H2 headings.
//
// Rules:
//   - Text before the first H2 is emitted as one Doc, titled from the H1 heading
//     (the first "# …" line) or sourceName if no H1 is found.
//   - Each H2 section becomes one Doc; the title is the H2 heading text.
//   - "##" lines inside backtick code fences (``` or ~~~) are ignored.
//   - Docs with empty content (whitespace-only) are dropped.
func ParseMarkdown(libID, sourceName, content string) []db.Doc {
	lines := strings.Split(content, "\n")

	var (
		docs    []db.Doc
		inFence bool

		// h1Title is the document-level title discovered before the first H2.
		h1Title string

		// currentTitle is the title of the section being accumulated.
		// Empty means we are accumulating the preamble.
		currentTitle string
		currentBuf   strings.Builder

		// seenFirstH2 tracks whether we've encountered any H2 yet.
		seenFirstH2 bool
	)

	flush := func(title string) {
		body := strings.TrimSpace(currentBuf.String())
		if body != "" {
			docs = append(docs, db.Doc{
				LibID:   libID,
				Title:   title,
				Content: body,
			})
		}
		currentBuf.Reset()
	}

	for _, line := range lines {
		// Detect code fence boundaries (``` or ~~~).
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "```") || strings.HasPrefix(stripped, "~~~") {
			inFence = !inFence
			currentBuf.WriteString(line)
			currentBuf.WriteByte('\n')
			continue
		}

		if !inFence {
			// H1 — capture as document title if not yet set.
			if strings.HasPrefix(line, "# ") && h1Title == "" {
				h1Title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
				// Don't emit a Doc for H1 itself — it becomes the preamble title.
				currentBuf.WriteString(line)
				currentBuf.WriteByte('\n')
				continue
			}

			// H2 — flush current section, start a new one.
			if strings.HasPrefix(line, "## ") {
				title := currentTitle
				if !seenFirstH2 {
					// Flush preamble with h1Title (or sourceName fallback).
					if title == "" {
						if h1Title != "" {
							title = h1Title
						} else {
							title = sourceName
						}
					}
					seenFirstH2 = true
				}
				flush(title)
				currentTitle = strings.TrimSpace(strings.TrimPrefix(line, "## "))
				continue
			}
		}

		currentBuf.WriteString(line)
		currentBuf.WriteByte('\n')
	}

	// Flush the last section.
	if !seenFirstH2 {
		// No H2 at all — entire file is one Doc.
		title := h1Title
		if title == "" {
			title = sourceName
		}
		flush(title)
	} else {
		flush(currentTitle)
	}

	return docs
}

// Fetch downloads each URL in src and returns the parsed Docs.
// The provided http.Client is used so callers can inject test servers.
func Fetch(ctx context.Context, client *http.Client, src Source) ([]db.Doc, error) {
	var out []db.Doc

	for _, u := range src.URLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("build request %s: %w", u, err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", u, err)
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read body %s: %w", u, readErr)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch %s: HTTP %d", u, resp.StatusCode)
		}

		// Derive source name from URL filename without extension.
		sourceName := strings.TrimSuffix(path.Base(u), ".md")

		docs := ParseMarkdown(src.LibID, sourceName, string(body))
		out = append(out, docs...)
	}

	return out, nil
}
