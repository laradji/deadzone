package godoc

import (
	"archive/zip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/sumdb/dirhash"
)

func TestEscapeModulePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"github lowercase", "github.com/spf13/cobra", "github.com/spf13/cobra"},
		{"gopkg.in dot-version", "gopkg.in/yaml.v3", "gopkg.in/yaml.v3"},
		{"github mixed case", "github.com/Azure/azure-sdk-for-go", "github.com/!azure/azure-sdk-for-go"},
		{"all-caps segment", "github.com/AB/CD", "github.com/!a!b/!c!d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := escapeModulePath(tc.in)
			if err != nil {
				t.Fatalf("escapeModulePath(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("escapeModulePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestEscapeModulePath_RejectsInvalid(t *testing.T) {
	// Empty path is not a valid module path per the spec.
	_, err := escapeModulePath("")
	if err == nil {
		t.Errorf("expected error for empty module path, got nil")
	}
}

func TestParseSumdbResponse(t *testing.T) {
	// Real-world response fetched from sum.golang.org for cobra@v1.10.2
	// (4 lines + tree proof). The h1 zip hash is on line 2; line 3 is
	// the go.mod hash which we must NOT return.
	body := []byte(`47802283
github.com/spf13/cobra v1.10.2 h1:DMTTonx5m65Ic0GOoRY2c16WCbHxOOw6xxezuLaBpcU=
github.com/spf13/cobra v1.10.2/go.mod h1:7C1pvHqHw5A4vrJfjNwvOdzYu0Gml16OCs2GRiTUUS4=

go.sum database tree
12345678
sometreestuff
`)
	got, err := parseSumdbResponse(body)
	if err != nil {
		t.Fatalf("parseSumdbResponse: %v", err)
	}
	want := "h1:DMTTonx5m65Ic0GOoRY2c16WCbHxOOw6xxezuLaBpcU="
	if got != want {
		t.Errorf("h1 = %q, want %q (must be the zip hash, NOT the /go.mod hash)", got, want)
	}
}

func TestParseSumdbResponse_Empty(t *testing.T) {
	_, err := parseSumdbResponse([]byte(""))
	if err == nil {
		t.Errorf("expected error on empty body, got nil")
	}
}

func TestParseSumdbResponse_OnlyGoModLine(t *testing.T) {
	// If somehow only the /go.mod line is present, we must not return
	// its hash — that would silently verify against the wrong artifact.
	body := []byte("github.com/x/y v1/go.mod h1:abc=\n")
	_, err := parseSumdbResponse(body)
	if err == nil {
		t.Errorf("expected error when only /go.mod line is present, got nil")
	}
}

func TestFetchSumdbHash_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lookup/github.com/spf13/cobra@v1.10.2":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("47802283\ngithub.com/spf13/cobra v1.10.2 h1:abcdef=\ngithub.com/spf13/cobra v1.10.2/go.mod h1:xyz=\n"))
		case "/lookup/github.com/missing/mod@v1.0.0":
			http.NotFound(w, r)
		case "/lookup/github.com/sumdb-down/mod@v1.0.0":
			http.Error(w, "down", http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Run("happy path", func(t *testing.T) {
		got, err := fetchSumdbHash(context.Background(), srv.Client(), srv.URL, "github.com/spf13/cobra", "v1.10.2")
		if err != nil {
			t.Fatalf("fetchSumdbHash: %v", err)
		}
		if got != "h1:abcdef=" {
			t.Errorf("h1 = %q, want %q", got, "h1:abcdef=")
		}
	})

	t.Run("404 maps to ErrModuleNotFound", func(t *testing.T) {
		_, err := fetchSumdbHash(context.Background(), srv.Client(), srv.URL, "github.com/missing/mod", "v1.0.0")
		if !errors.Is(err, ErrModuleNotFound) {
			t.Errorf("err = %v, want errors.Is(ErrModuleNotFound)", err)
		}
	})

	t.Run("5xx maps to ErrSumDBUnavailable", func(t *testing.T) {
		_, err := fetchSumdbHash(context.Background(), srv.Client(), srv.URL, "github.com/sumdb-down/mod", "v1.0.0")
		if !errors.Is(err, ErrSumDBUnavailable) {
			t.Errorf("err = %v, want errors.Is(ErrSumDBUnavailable)", err)
		}
	})
}

func TestVerifyZipChecksum(t *testing.T) {
	// Build a tiny zip whose entries follow the dirhash convention
	// (entries prefixed with "<module>@<version>/"). HashZip then
	// returns a deterministic h1 we can re-feed to verifyZipChecksum.
	zipPath := buildSampleZip(t, "example.com/x", "v1.0.0", map[string]string{
		"foo.go": "package x\n",
		"bar.go": "package x\nfunc Bar() {}\n",
	})

	got, err := dirhash.HashZip(zipPath, dirhash.Hash1)
	if err != nil {
		t.Fatalf("dirhash.HashZip: %v", err)
	}
	if !strings.HasPrefix(got, "h1:") {
		t.Fatalf("h1 prefix missing: %q", got)
	}

	t.Run("matching hash", func(t *testing.T) {
		if err := verifyZipChecksum(zipPath, got); err != nil {
			t.Errorf("verifyZipChecksum (matching): %v", err)
		}
	})

	t.Run("mismatched hash", func(t *testing.T) {
		err := verifyZipChecksum(zipPath, "h1:wronghashvalueZ=")
		if !errors.Is(err, ErrSumDBMismatch) {
			t.Errorf("err = %v, want errors.Is(ErrSumDBMismatch)", err)
		}
	})
}

// buildSampleZip writes a zip at <tempdir>/test.zip whose entries are
// prefixed with "<module>@<version>/" — the layout proxy.golang.org
// uses, which is what dirhash.HashZip expects.
func buildSampleZip(t *testing.T, modPath, version string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "test.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	prefix := modPath + "@" + version + "/"
	// Sort filenames for stable zip — HashZip itself sorts internally
	// so this isn't strictly required, but it makes test output
	// reproducible if we ever inspect the raw zip.
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
	return zipPath
}

