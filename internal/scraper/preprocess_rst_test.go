package scraper_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/scraper"
)

func TestParseRST_BasicHeadings(t *testing.T) {
	const content = `First Top-Level
===============

Intro paragraph for the first top-level section.

Second Top-Level
================

Body of second top-level.

A Subsection
------------

Body of the subsection.
`

	docs := scraper.ParseRST("/test/lib", "sample", content)

	if len(docs) != 3 {
		t.Fatalf("expected 3 docs, got %d: %+v", len(docs), docTitles(docs))
	}
	want := []string{"First Top-Level", "Second Top-Level", "A Subsection"}
	for i, w := range want {
		if docs[i].Title != w {
			t.Errorf("docs[%d].Title = %q, want %q", i, docs[i].Title, w)
		}
		if docs[i].LibID != "/test/lib" {
			t.Errorf("docs[%d].LibID = %q, want /test/lib", i, docs[i].LibID)
		}
	}
}

func TestParseRST_CodeBlocks(t *testing.T) {
	// Mix of `.. code-block:: lang` directive and trailing-`::` literal
	// block. Both forms must keep code (including indentation) verbatim.
	const content = `Examples
========

Use the directive form for syntax-highlighted code:

.. code-block:: python

   import os
   for name in os.listdir("."):
       print(name)

The literal-block form is also supported::

   def hello():
       return "world"

Final prose paragraph.
`
	docs := scraper.ParseRST("/test/lib", "examples", content)

	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	body := docs[0].Content

	for _, snippet := range []string{
		"import os",
		`for name in os.listdir(".")`,
		`       print(name)`, // 7-space indent preserved
		`def hello():`,
		`       return "world"`,
		".. code-block:: python", // directive header preserved
	} {
		if !strings.Contains(body, snippet) {
			t.Errorf("body missing %q\nbody:\n%s", snippet, body)
		}
	}
}

func TestParseRST_CrossRefs(t *testing.T) {
	const content = "Cross-Refs\n" +
		"==========\n\n" +
		"See :func:`os.path.join` for joining paths.\n" +
		"Also see :ref:`the open() builtin <open-builtin>` for the high-level interface.\n" +
		"The :class:`pathlib.Path` class is the modern alternative.\n\n" +
		".. _some-target:\n\n" +
		"Body continues here.\n"

	docs := scraper.ParseRST("/test/lib", "refs", content)
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	body := docs[0].Content

	for _, want := range []string{"os.path.join", "the open() builtin", "pathlib.Path"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected visible text %q in body, got:\n%s", want, body)
		}
	}
	for _, banned := range []string{":func:", ":ref:", ":class:", "<open-builtin>", ".. _some-target:"} {
		if strings.Contains(body, banned) {
			t.Errorf("expected %q to be stripped from body, got:\n%s", banned, body)
		}
	}
}

func TestParseRST_RealStdlib(t *testing.T) {
	content := loadFixture(t, "os_excerpt.rst")
	docs := scraper.ParseRST("/python/cpython", "os", content)

	if len(docs) < 3 {
		t.Fatalf("expected >=3 docs, got %d: %+v", len(docs), docTitles(docs))
	}

	// First section heading should carry the H1-equivalent title with
	// its `:mod:` cross-ref collapsed to "os".
	if !strings.HasPrefix(docs[0].Title, "os ---") {
		t.Errorf("docs[0].Title = %q, want prefix %q", docs[0].Title, "os ---")
	}

	// Subsections become flat siblings (no parent linkage).
	gotTitles := docTitles(docs)
	for _, want := range []string{"Process Parameters", "File Descriptor Operations"} {
		if !sliceContains(gotTitles, want) {
			t.Errorf("expected title %q in %v", want, gotTitles)
		}
	}

	// Code from both literal-block (`::`) and `.. code-block::` forms
	// survives verbatim.
	joined := strings.Join(docContents(docs), "\n---\n")
	if !strings.Contains(joined, "cwd = os.getcwd()") {
		t.Errorf("expected literal-block code preserved; bodies:\n%s", joined)
	}
	if !strings.Contains(joined, "fd = os.open(") {
		t.Errorf("expected code-block:: code preserved; bodies:\n%s", joined)
	}
}

func TestFetchOneViaGithubRST(t *testing.T) {
	const payload = "Title\n=====\n\n" +
		"Body paragraph with :func:`some.thing` cross-ref.\n\n" +
		"Section Two\n-----------\n\nMore body.\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".rst") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	res, err := scraper.FetchOneViaGithubRST(context.Background(), srv.Client(), "/test/lib", srv.URL+"/sample.rst")
	if err != nil {
		t.Fatalf("FetchOneViaGithubRST: %v", err)
	}
	if res.Bytes != len(payload) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(payload))
	}
	if len(res.Docs) != 2 {
		t.Fatalf("expected 2 docs, got %d: %+v", len(res.Docs), docTitles(res.Docs))
	}
	if res.Docs[0].Title != "Title" || res.Docs[1].Title != "Section Two" {
		t.Errorf("unexpected titles %+v", docTitles(res.Docs))
	}
	if !strings.Contains(res.Docs[0].Content, "some.thing") || strings.Contains(res.Docs[0].Content, ":func:") {
		t.Errorf("cross-ref not collapsed in body %q", res.Docs[0].Content)
	}
}

func TestFetchOneViaGithubRST_Non200ReturnsHTTPStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := scraper.FetchOneViaGithubRST(context.Background(), srv.Client(), "/test/lib", srv.URL+"/x.rst")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var httpErr *scraper.HTTPStatusError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *HTTPStatusError, got %T: %v", err, err)
	}
	if httpErr.Status != 500 {
		t.Errorf("Status = %d, want 500", httpErr.Status)
	}
}

func docTitles(docs []db.Doc) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.Title
	}
	return out
}

func docContents(docs []db.Doc) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.Content
	}
	return out
}

func sliceContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
