package godoc_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/sumdb/dirhash"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/scraper/godoc"
)

// buildModuleZip creates an in-memory module zip with entries
// prefixed by "<modPath>@<version>/" — the layout proxy.golang.org
// uses (and that dirhash.HashZip expects).
func buildModuleZip(t *testing.T, modPath, version string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	prefix := modPath + "@" + version + "/"
	for name, content := range files {
		w, err := zw.Create(prefix + name)
		if err != nil {
			t.Fatalf("create entry %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write entry %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// hashZipBytes returns the canonical h1 hash of the supplied zip
// bytes — the value sumdb would store.
func hashZipBytes(t *testing.T, modPath, version string, zipBytes []byte) string {
	t.Helper()
	tmpZip := writeTempFile(t, "test.zip", zipBytes)
	got, err := dirhash.HashZip(tmpZip, dirhash.Hash1)
	if err != nil {
		t.Fatalf("dirhash.HashZip: %v", err)
	}
	return got
}

func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestFetchOneViaGodoc_ProxyHappyPath(t *testing.T) {
	const (
		modPath = "github.com/example/sample"
		version = "v1.2.3"
	)
	zipBytes := buildModuleZip(t, modPath, version, map[string]string{
		"go.mod": "module " + modPath + "\n\ngo 1.21\n",
		"sample.go": `// Package sample is a fixture.
package sample

// Greet returns a greeting.
func Greet(name string) string { return "hi " + name }
`,
	})
	expectedHash := hashZipBytes(t, modPath, version, zipBytes)

	// Sumdb returns the real h1 → verifyZipChecksum passes.
	sumdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lookup/"+modPath+"@"+version {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "12345\n%s %s %s\n%s %s/go.mod h1:fakegomod=\n\ngo.sum tree\n", modPath, version, expectedHash, modPath, version)
	}))
	defer sumdb.Close()
	t.Setenv(godoc.SumdbBaseEnv, sumdb.URL)

	// Proxy serves the zip on the canonical path.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+modPath+"/@v/"+version+".zip" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipBytes)
	}))
	defer proxy.Close()

	url := proxy.URL + "/" + modPath + "/@v/" + version + ".zip"
	res, err := godoc.FetchOneViaGodoc(context.Background(), proxy.Client(), "/example/sample", url)
	if err != nil {
		t.Fatalf("FetchOneViaGodoc: %v", err)
	}
	if len(res.Docs) == 0 {
		t.Fatalf("expected ≥1 doc, got 0")
	}
	if res.Bytes != len(zipBytes) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(zipBytes))
	}
	titles := titleSet(res.Docs)
	if !titles["sample package"] {
		t.Errorf("missing package overview chunk; got %v", keys(titles))
	}
	if !titles["sample.Greet"] {
		t.Errorf("missing sample.Greet chunk; got %v", keys(titles))
	}
}

func TestFetchOneViaGodoc_SumdbMismatchIsFatal(t *testing.T) {
	const (
		modPath = "github.com/example/tampered"
		version = "v0.0.1"
	)
	zipBytes := buildModuleZip(t, modPath, version, map[string]string{
		"x.go": "package x\n",
	})

	// Sumdb returns the WRONG hash — verifyZipChecksum must surface
	// ErrSumDBMismatch and fail the fetch.
	sumdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "1\n%s %s h1:bogusvaluethatwillnotmatchZ=\n%s %s/go.mod h1:bogus2=\n", modPath, version, modPath, version)
	}))
	defer sumdb.Close()
	t.Setenv(godoc.SumdbBaseEnv, sumdb.URL)

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	}))
	defer proxy.Close()

	url := proxy.URL + "/" + modPath + "/@v/" + version + ".zip"
	_, err := godoc.FetchOneViaGodoc(context.Background(), proxy.Client(), "/example/tampered", url)
	if !errors.Is(err, godoc.ErrSumDBMismatch) {
		t.Fatalf("err = %v, want errors.Is(ErrSumDBMismatch)", err)
	}
}

func TestFetchOneViaGodoc_ProxyNotFound(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer proxy.Close()
	// Sumdb won't be hit because download fails first; point at proxy
	// for safety in case order changes.
	t.Setenv(godoc.SumdbBaseEnv, proxy.URL)

	url := proxy.URL + "/github.com/example/missing/@v/v1.0.0.zip"
	_, err := godoc.FetchOneViaGodoc(context.Background(), proxy.Client(), "/example/missing", url)
	if !errors.Is(err, godoc.ErrModuleNotFound) {
		t.Fatalf("err = %v, want errors.Is(ErrModuleNotFound)", err)
	}
}

func TestFetchOneViaGodoc_ProxyGoneIsWithdrawn(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer proxy.Close()
	t.Setenv(godoc.SumdbBaseEnv, proxy.URL)

	url := proxy.URL + "/github.com/example/withdrawn/@v/v0.0.0.zip"
	_, err := godoc.FetchOneViaGodoc(context.Background(), proxy.Client(), "/example/withdrawn", url)
	if !errors.Is(err, godoc.ErrModuleWithdrawn) {
		t.Fatalf("err = %v, want errors.Is(ErrModuleWithdrawn)", err)
	}
}

func TestFetchOneViaGodoc_GitHubContentsHappyPath(t *testing.T) {
	// Two files served via Contents API + their raw download_url.
	const sampleSrc = `// Package json provides JSON encoding.
package json

// Marshal serializes v.
func Marshal(v any) ([]byte, error) { return nil, nil }
`
	const sampleSrc2 = `package json

// Unmarshal deserializes b.
func Unmarshal(b []byte, v any) error { return nil }
`

	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/golang/go/contents/src/encoding/json":
			// The real API ignores the order of query params; we just
			// echo a 2-file directory.
			entries := []map[string]string{
				{"name": "encode.go", "type": "file", "download_url": srvURL + "/raw/encode.go"},
				{"name": "decode.go", "type": "file", "download_url": srvURL + "/raw/decode.go"},
				// _test.go must be filtered by the fetcher.
				{"name": "encode_test.go", "type": "file", "download_url": srvURL + "/raw/encode_test.go"},
				// directory entries are skipped (no recursion in v0).
				{"name": "v2", "type": "dir", "download_url": ""},
			}
			body, _ := json.Marshal(entries)
			w.Header().Set("Content-Type", "application/vnd.github+json")
			_, _ = w.Write(body)
		case r.URL.Path == "/raw/encode.go":
			_, _ = w.Write([]byte(sampleSrc))
		case r.URL.Path == "/raw/decode.go":
			_, _ = w.Write([]byte(sampleSrc2))
		case r.URL.Path == "/raw/encode_test.go":
			t.Errorf("fetcher should NOT download _test.go files")
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	url := srv.URL + "/repos/golang/go/contents/src/encoding/json?ref=go1.26.2"
	res, err := godoc.FetchOneViaGodoc(context.Background(), srv.Client(), "/golang/go/encoding/json", url)
	if err != nil {
		t.Fatalf("FetchOneViaGodoc: %v", err)
	}
	if res.Bytes <= 0 {
		t.Errorf("Bytes = %d, want >0", res.Bytes)
	}
	titles := titleSet(res.Docs)
	if !titles["json package"] {
		t.Errorf("missing package overview; got %v", keys(titles))
	}
	if !titles["json.Marshal"] || !titles["json.Unmarshal"] {
		t.Errorf("missing Marshal/Unmarshal chunks; got %v", keys(titles))
	}
}

// TestFetchOneViaGodoc_NetworkCobra is the real end-to-end smoke
// against proxy.golang.org + sum.golang.org for /spf13/cobra@v1.10.2.
// Skipped under -short (which is what CI runs by default per
// .github/workflows/ci.yml line 152) — this exists for manual local
// validation and any future CI lane that opts into network tests.
//
// What it pins down that the httptest mocks can't:
//   - The real sumdb response format (which may evolve).
//   - The real proxy.golang.org zip layout (entries prefixed with
//     <module>@<version>/).
//   - The expected chunk count is in the 25-80 window from the issue
//     acceptance criteria.
//   - cobra.Command (the headline type) actually shows up.
func TestFetchOneViaGodoc_NetworkCobra(t *testing.T) {
	if testing.Short() {
		t.Skip("network test — run with `go test -tags ORT ./internal/scraper/godoc/ -run TestFetchOneViaGodoc_NetworkCobra` to exercise")
	}
	url := "https://proxy.golang.org/github.com/spf13/cobra/@v/v1.10.2.zip"
	res, err := godoc.FetchOneViaGodoc(context.Background(), http.DefaultClient, "/spf13/cobra", url)
	if err != nil {
		t.Fatalf("FetchOneViaGodoc: %v", err)
	}
	n := len(res.Docs)
	if n < 25 || n > 200 {
		t.Errorf("chunk count = %d, want in [25, 200] (issue AC: 25-80; widened to 200 to absorb cobra subpackages)", n)
	}
	titles := titleSet(res.Docs)
	if !titles["cobra.Command"] {
		t.Errorf("expected chunk 'cobra.Command' to be present; got titles: %v", keys(titles))
	}
}

// TestFetchOneViaGodoc_NetworkStdlibJSON exercises the GitHub
// Contents API path against the real api.github.com for
// /golang/go/encoding/json. Skipped under -short for the same reason
// as TestFetchOneViaGodoc_NetworkCobra. Note: anonymous github API
// has a 60 req/h rate limit; consecutive runs may 403. Set
// GITHUB_TOKEN to bypass.
func TestFetchOneViaGodoc_NetworkStdlibJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("network test — run with `go test ./internal/scraper/godoc/ -run TestFetchOneViaGodoc_NetworkStdlibJSON` to exercise")
	}
	url := "https://api.github.com/repos/golang/go/contents/src/encoding/json?ref=go1.26.2"
	res, err := godoc.FetchOneViaGodoc(context.Background(), http.DefaultClient, "/golang/go/encoding/json", url)
	if err != nil {
		// Anonymous rate-limit is the most likely failure mode for a
		// CI re-run cluster; surface it as Skip rather than Fail so
		// the suite stays green when the quota's exhausted.
		if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "rate limit") {
			t.Skipf("github API rate-limited: %v (set GITHUB_TOKEN to bypass)", err)
		}
		t.Fatalf("FetchOneViaGodoc: %v", err)
	}
	n := len(res.Docs)
	// The research doc §4 measured ~68 chunks for encoding/json against
	// an earlier Go version where v2/, jsontext/, and internal/ lived
	// at the same level. In Go 1.26.2 those moved to subdirs, and the
	// v0 fetcher does NOT recurse — operator selects exact package
	// dirs (each subpackage gets its own lib_id). So the count drops
	// to ~25 for the top-level json package alone. Bound the window
	// generously to absorb both shapes; tightening is a follow-up
	// once the granularity question (one lib_id vs 195) is settled.
	if n < 15 || n > 200 {
		t.Errorf("chunk count = %d, want in [15, 200]", n)
	}
	titles := titleSet(res.Docs)
	mustHave := []string{"json package", "json.Marshal", "json.Unmarshal"}
	for _, want := range mustHave {
		if !titles[want] {
			t.Errorf("expected chunk %q not found", want)
		}
	}
}

func TestFetchOneViaGodoc_RejectsUnknownURLShape(t *testing.T) {
	_, err := godoc.FetchOneViaGodoc(context.Background(), http.DefaultClient, "/lib", "https://example.com/random/path")
	if err == nil {
		t.Errorf("expected error for unknown URL shape, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported URL shape") {
		t.Errorf("err = %v, want a 'unsupported URL shape' message", err)
	}
}

// helpers
func titleSet(docs []db.Doc) map[string]bool {
	out := make(map[string]bool, len(docs))
	for _, d := range docs {
		out[d.Title] = true
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
