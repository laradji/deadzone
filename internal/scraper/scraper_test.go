package scraper_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/laradji/deadzone/internal/scraper"
)

// loadFixture reads testdata/<name> relative to this file's package.
func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("loadFixture %s: %v", name, err)
	}
	return string(b)
}

func TestParseMarkdown_SplitsByH2(t *testing.T) {
	content := loadFixture(t, "sample.md")
	docs := scraper.ParseMarkdown("/test/lib", "sample", content)

	// Expect 4 docs: preamble + Installation + Usage + Advanced
	if len(docs) != 4 {
		t.Fatalf("expected 4 docs, got %d", len(docs))
	}

	titles := []string{"Sample Library", "Installation", "Usage", "Advanced"}
	for i, want := range titles {
		if docs[i].Title != want {
			t.Errorf("docs[%d].Title = %q, want %q", i, docs[i].Title, want)
		}
	}
}

func TestParseMarkdown_PreambleBecomesDoc(t *testing.T) {
	content := loadFixture(t, "sample.md")
	docs := scraper.ParseMarkdown("/test/lib", "sample", content)

	// First doc is the preamble, titled from H1
	if docs[0].Title != "Sample Library" {
		t.Errorf("preamble doc title = %q, want %q", docs[0].Title, "Sample Library")
	}
	if !strings.Contains(docs[0].Content, "preamble text") {
		t.Errorf("preamble doc content missing expected text; got: %q", docs[0].Content)
	}
}

func TestParseMarkdown_IgnoresHeadingsInCodeFences(t *testing.T) {
	content := loadFixture(t, "sample.md")
	docs := scraper.ParseMarkdown("/test/lib", "sample", content)

	// "## NotAHeading" inside a code fence must not create a new Doc
	for _, d := range docs {
		if d.Title == "NotAHeading" || strings.Contains(d.Title, "NotAHeading") {
			t.Errorf("code fence heading leaked as Doc title: %q", d.Title)
		}
	}
	// Confirm the Usage section content contains the fence content
	var usageDoc *struct{ title, content string }
	for _, d := range docs {
		if d.Title == "Usage" {
			usageDoc = &struct{ title, content string }{d.Title, d.Content}
			break
		}
	}
	if usageDoc == nil {
		t.Fatal("Usage doc not found")
	}
	if !strings.Contains(usageDoc.content, "NotAHeading") {
		t.Errorf("Usage doc should contain fence text 'NotAHeading'; got: %q", usageDoc.content)
	}
}

func TestParseMarkdown_NoH2_SingleDoc(t *testing.T) {
	content := "# Only Title\n\nSome content without any H2 headings.\n"
	docs := scraper.ParseMarkdown("/test/lib", "nodoc", content)

	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	if docs[0].Title != "Only Title" {
		t.Errorf("title = %q, want %q", docs[0].Title, "Only Title")
	}
	if !strings.Contains(docs[0].Content, "Some content") {
		t.Errorf("content missing; got %q", docs[0].Content)
	}
}

func TestParseMarkdown_LibIDPropagated(t *testing.T) {
	content := loadFixture(t, "sample.md")
	const libID = "/myorg/mylib"
	docs := scraper.ParseMarkdown(libID, "sample", content)

	for i, d := range docs {
		if d.LibID != libID {
			t.Errorf("docs[%d].LibID = %q, want %q", i, d.LibID, libID)
		}
	}
}

func TestFetch_DownloadsAndParses(t *testing.T) {
	fixtureA := "# Doc A\n\n## Section One\n\nContent of section one.\n"
	fixtureB := "# Doc B\n\n## Section Two\n\nContent of section two.\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a.md":
			fmt.Fprint(w, fixtureA)
		case "/b.md":
			fmt.Fprint(w, fixtureB)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src := scraper.Source{
		LibID: "/test/lib",
		URLs:  []string{srv.URL + "/a.md", srv.URL + "/b.md"},
	}

	docs, err := scraper.Fetch(context.Background(), srv.Client(), src)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Each file has a preamble (H1) + one H2 → 2 docs per file = 4 total
	if len(docs) != 4 {
		t.Fatalf("expected 4 docs, got %d", len(docs))
	}

	titles := map[string]bool{}
	for _, d := range docs {
		titles[d.Title] = true
		if d.LibID != "/test/lib" {
			t.Errorf("doc %q has LibID %q, want /test/lib", d.Title, d.LibID)
		}
	}
	for _, want := range []string{"Doc A", "Section One", "Doc B", "Section Two"} {
		if !titles[want] {
			t.Errorf("missing expected doc title %q", want)
		}
	}
}

func TestFetch_404Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src := scraper.Source{
		LibID: "/test/lib",
		URLs:  []string{srv.URL + "/missing.md"},
	}

	_, err := scraper.Fetch(context.Background(), srv.Client(), src)
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}
}

// TestFetchOne_Non200ReturnsHTTPStatusError pins the contract that the
// github-md fast path returns a typed *HTTPStatusError on non-200, so
// cmd/deadzone/scrape.go's classifyFetchErr can errors.As-match it and
// soft-skip 5xx (instead of treating it as reason="other" and aborting
// the whole lib on the first transient blip from raw.gh's CDN).
func TestFetchOne_Non200ReturnsHTTPStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := scraper.FetchOne(context.Background(), srv.Client(), "/test/lib", srv.URL+"/x.md")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var httpErr *scraper.HTTPStatusError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *HTTPStatusError, got %T: %v", err, err)
	}
	if httpErr.Status != 503 {
		t.Errorf("Status = %d, want 503", httpErr.Status)
	}
}
