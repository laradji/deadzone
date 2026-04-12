package scraper

import (
	"errors"
	"fmt"
	"mime"
	"strings"
)

// ErrPDFNotSupportedYet is returned by preprocess for application/pdf
// content. The slot is reserved deliberately: the architecture in #27
// is designed so adding PDF is a single new case in this switch plus a
// pure-Go text extractor, not a rearchitecture. Until that follow-up
// lands the scraper refuses PDFs loudly rather than silently routing
// raw bytes through the LLM.
var ErrPDFNotSupportedYet = errors.New("application/pdf input is not supported yet (planned follow-up to #27)")

// ContentType buckets we accept on the agent path. The string values
// match the canonical MIME types after media-type parameter stripping.
const (
	contentTypeHTML     = "text/html"
	contentTypeXHTML    = "application/xhtml+xml"
	contentTypeMarkdown = "text/markdown"
	contentTypeXMD      = "text/x-markdown"
	contentTypePlain    = "text/plain"
	contentTypePDF      = "application/pdf"
)

// preprocessGate decides whether contentType is acceptable for the
// agent path without needing the body. Split out so FetchOneViaAgent
// can reject unsupported types (PDF, binary) before io.ReadAll eats a
// potentially large response into memory. Returns nil for accepted
// types, ErrPDFNotSupportedYet for PDFs, and a formatted "unsupported
// content type" error for everything else (including empty headers).
func preprocessGate(contentType string) error {
	switch normalizeContentType(contentType) {
	case contentTypeHTML, contentTypeXHTML,
		contentTypeMarkdown, contentTypeXMD, contentTypePlain:
		return nil
	case contentTypePDF:
		return ErrPDFNotSupportedYet
	default:
		// Empty Content-Type is most often a misconfigured static
		// server; surface it as the same "unsupported" error rather
		// than guessing.
		return fmt.Errorf("unsupported content type %q", contentType)
	}
}

// preprocess turns a raw HTTP response body into LLM-ready text for
// Agent.Extract. The contentType argument is the raw Content-Type
// response header (parameters and casing tolerated); preprocess does
// the parsing via preprocessGate.
//
// Behaviour is intentionally minimal in v1: HTML, markdown, and plain
// text are returned as-is — modern LLMs handle HTML natively and don't
// need a Go-side DOM walk to do good extraction. PDF is reserved for a
// follow-up. Anything else is rejected with a clear error so the
// operator notices instead of feeding the model raw bytes that turn
// into garbage downstream.
func preprocess(body []byte, contentType string) (string, error) {
	if err := preprocessGate(contentType); err != nil {
		return "", err
	}
	return string(body), nil
}

// normalizeContentType strips media-type parameters (charset=, boundary=, …)
// and lower-cases the bare type/subtype so the switch in preprocess can
// match exact strings. Failures fall back to the trimmed lower-cased
// header so the resulting "unsupported" error message includes whatever
// the server actually sent.
func normalizeContentType(raw string) string {
	if raw == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(raw))
	}
	return mt
}
