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

// FetchOneResult bundles what FetchOne produces for a single URL: the
// parsed docs plus the raw byte count of the response body. Bytes is
// exposed so callers can log fetch volume per URL without holding on
// to (or re-reading) the body.
type FetchOneResult struct {
	Docs  []db.Doc
	Bytes int
}

// FetchOne downloads a single markdown URL with the given http.Client
// and parses it into docs. Errors are wrapped with the URL so callers
// can include them verbatim in structured logs.
//
// FetchOne is the per-URL primitive used by Fetch and by cmd/scraper,
// which drives its own URL loop so it can emit per-URL log events
// (scraper.fetch / scraper.fetch_failed) and time embedding/insertion
// alongside the fetch.
func FetchOne(ctx context.Context, client *http.Client, libID, url string) (FetchOneResult, error) {
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
		return FetchOneResult{}, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}

	// Derive source name from URL filename without extension.
	sourceName := strings.TrimSuffix(path.Base(url), ".md")

	docs := ParseMarkdown(libID, sourceName, string(body))
	return FetchOneResult{Docs: docs, Bytes: len(body)}, nil
}

// FetchOneViaAgent is the per-URL primitive for the scrape-via-agent
// kind: HTTP GET, content-type-aware preprocessing, LLM extraction,
// code-block verification, and ParseMarkdown into docs.
//
// Returns ErrAgentVerificationFailed if the LLM emitted a fenced code
// block that does not appear verbatim in the source content. The
// caller is expected to log the failure and skip the doc; the rest of
// the URLs in the same source still get processed.
//
// Like FetchOne, this is the per-URL primitive used by cmd/scraper so
// the operator log can carry per-URL fetch / verify / index events
// instead of one summary per source.
func FetchOneViaAgent(ctx context.Context, client *http.Client, agent *Agent, libID, url string) (FetchOneResult, error) {
	if agent == nil {
		return FetchOneResult{}, fmt.Errorf("fetch via agent %s: agent is nil", url)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return FetchOneResult{}, fmt.Errorf("build request %s: %w", url, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return FetchOneResult{}, fmt.Errorf("fetch %s: %w", url, err)
	}

	// Inspect status + Content-Type before io.ReadAll so a 100 MB PDF
	// or an unsupported binary doesn't get streamed into memory just
	// to be thrown away by preprocess. HTTPStatusError is typed so
	// cmd/scraper can route 5xx to the transient-soft-fail path.
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return FetchOneResult{}, &HTTPStatusError{Status: resp.StatusCode, URL: url}
	}
	contentType := resp.Header.Get("Content-Type")
	if err := preprocessGate(contentType); err != nil {
		resp.Body.Close()
		return FetchOneResult{}, fmt.Errorf("preprocess %s: %w", url, err)
	}

	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return FetchOneResult{}, fmt.Errorf("read body %s: %w", url, readErr)
	}

	text, err := preprocess(body, contentType)
	if err != nil {
		return FetchOneResult{}, fmt.Errorf("preprocess %s: %w", url, err)
	}

	md, err := agent.Extract(ctx, text, contentType)
	if err != nil {
		return FetchOneResult{}, fmt.Errorf("extract %s: %w", url, err)
	}

	if !verifyCodeBlocks(md, text) {
		return FetchOneResult{}, fmt.Errorf("%s: %w", url, ErrAgentVerificationFailed)
	}

	sourceName := strings.TrimSuffix(path.Base(url), path.Ext(url))
	docs := ParseMarkdown(libID, sourceName, md)
	return FetchOneResult{Docs: docs, Bytes: len(body)}, nil
}

// Fetch downloads each URL in src and returns the concatenated Docs.
// Implemented as a thin loop over FetchOne so callers that just want
// "give me everything" don't have to deal with per-URL bookkeeping.
func Fetch(ctx context.Context, client *http.Client, src Source) ([]db.Doc, error) {
	var out []db.Doc
	for _, u := range src.URLs {
		res, err := FetchOne(ctx, client, src.LibID, u)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Docs...)
	}
	return out, nil
}
