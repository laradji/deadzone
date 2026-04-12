package scraper

import (
	"errors"
	"strings"
	"testing"
)

// preprocess and normalizeContentType are package-private; this test
// file lives in the same package (no _test suffix on the package
// declaration) so the unit tests can call them directly without an
// exported wrapper.
func TestPreprocess_AcceptsHTML(t *testing.T) {
	body := []byte("<html><body><h1>Hi</h1></body></html>")
	cases := []string{
		"text/html",
		"text/html; charset=utf-8",
		"TEXT/HTML",
		"application/xhtml+xml",
	}
	for _, ct := range cases {
		t.Run(ct, func(t *testing.T) {
			text, err := preprocess(body, ct)
			if err != nil {
				t.Fatalf("preprocess(%q): %v", ct, err)
			}
			if text != string(body) {
				t.Errorf("preprocess returned %q, want %q", text, body)
			}
		})
	}
}

func TestPreprocess_AcceptsMarkdownAndPlain(t *testing.T) {
	body := []byte("# Title\n\nbody")
	for _, ct := range []string{"text/markdown", "text/x-markdown", "text/plain", "text/plain; charset=us-ascii"} {
		t.Run(ct, func(t *testing.T) {
			text, err := preprocess(body, ct)
			if err != nil {
				t.Fatalf("preprocess(%q): %v", ct, err)
			}
			if text != string(body) {
				t.Errorf("preprocess returned %q, want %q", text, body)
			}
		})
	}
}

func TestPreprocess_RejectsPDFWithSentinel(t *testing.T) {
	_, err := preprocess([]byte("%PDF-1.7\n..."), "application/pdf")
	if err == nil {
		t.Fatal("expected error for application/pdf, got nil")
	}
	if !errors.Is(err, ErrPDFNotSupportedYet) {
		t.Errorf("expected ErrPDFNotSupportedYet, got %v", err)
	}
}

func TestPreprocess_RejectsUnknownTypes(t *testing.T) {
	cases := []string{
		"image/png",
		"application/octet-stream",
		"application/json",
		"",
		"   ",
	}
	for _, ct := range cases {
		t.Run(ct, func(t *testing.T) {
			_, err := preprocess([]byte("anything"), ct)
			if err == nil {
				t.Fatalf("expected error for content type %q, got nil", ct)
			}
			if !strings.Contains(err.Error(), "unsupported content type") {
				t.Errorf("error %q does not mention 'unsupported content type'", err.Error())
			}
		})
	}
}

// preprocessGate is the header-only gate used by FetchOneViaAgent to
// refuse unsupported content types before io.ReadAll. Behavior must
// stay in lockstep with preprocess so that "accepted by the gate" ⇒
// "accepted by preprocess" for any body.
func TestPreprocessGate_MatchesPreprocess(t *testing.T) {
	cases := []struct {
		contentType string
		wantErrIs   error // nil means "no error expected"
		wantUnknown bool  // true when preprocessGate should return the "unsupported" formatted error
	}{
		{"text/html", nil, false},
		{"text/html; charset=utf-8", nil, false},
		{"application/xhtml+xml", nil, false},
		{"text/markdown", nil, false},
		{"text/x-markdown", nil, false},
		{"text/plain", nil, false},
		{"application/pdf", ErrPDFNotSupportedYet, false},
		{"image/png", nil, true},
		{"application/octet-stream", nil, true},
		{"", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.contentType, func(t *testing.T) {
			gateErr := preprocessGate(tc.contentType)
			_, preErr := preprocess([]byte("body"), tc.contentType)
			// Parity: whatever kind of error preprocessGate returns,
			// preprocess must agree on (nil-or-not, same sentinel).
			if (gateErr == nil) != (preErr == nil) {
				t.Fatalf("parity broken: preprocessGate err=%v, preprocess err=%v", gateErr, preErr)
			}
			if tc.wantErrIs != nil && !errors.Is(gateErr, tc.wantErrIs) {
				t.Errorf("preprocessGate(%q) = %v, want errors.Is %v", tc.contentType, gateErr, tc.wantErrIs)
			}
			if tc.wantUnknown {
				if gateErr == nil || !strings.Contains(gateErr.Error(), "unsupported content type") {
					t.Errorf("preprocessGate(%q) = %v, want 'unsupported content type'", tc.contentType, gateErr)
				}
			}
		})
	}
}

func TestNormalizeContentType_StripsParameters(t *testing.T) {
	cases := map[string]string{
		"text/html; charset=utf-8":               "text/html",
		"TEXT/HTML; charset=UTF-8":               "text/html",
		"application/pdf":                        "application/pdf",
		" text/markdown ":                        "text/markdown",
		"":                                       "",
		"not a real header but trimmed":          "not a real header but trimmed",
		"application/xhtml+xml; profile=\"foo\"": "application/xhtml+xml",
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			got := normalizeContentType(input)
			if got != want {
				t.Errorf("normalizeContentType(%q) = %q, want %q", input, got, want)
			}
		})
	}
}
