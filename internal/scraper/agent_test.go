package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAgentResponse builds a minimal OpenAI-compatible chat completion
// response wrapping a single choice. Tests use this to script the
// httptest server's reply.
func fakeAgentResponse(content string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%s}}]}`, mustQuote(content))
}

func mustQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// startFakeAgent spins up an httptest server that records the parsed
// request body and replies with replyContent (or a non-200 status when
// statusCode != 200). Returns the *Agent pre-pointed at the server URL
// plus a recorded-request pointer the test can inspect after the call.
func startFakeAgent(t *testing.T, replyContent string, statusCode int) (*Agent, *capturedRequest, *httptest.Server) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.contentType = r.Header.Get("Content-Type")
		captured.authorization = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		captured.rawBody = body
		_ = json.Unmarshal(body, &captured.parsedBody)

		if statusCode != 0 && statusCode != http.StatusOK {
			w.WriteHeader(statusCode)
			_, _ = w.Write([]byte(`{"error":{"message":"forced failure"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(replyContent))
	}))
	t.Cleanup(srv.Close)

	agent := NewAgent(srv.URL+"/v1", "test-model", "", srv.Client())
	return agent, captured, srv
}

type capturedRequest struct {
	method        string
	path          string
	contentType   string
	authorization string
	rawBody       []byte
	parsedBody    chatRequest
}

// --- NewAgentFromEnv ---------------------------------------------------

func TestNewAgentFromEnv_MissingEnvErrors(t *testing.T) {
	t.Setenv(EnvAgentEndpoint, "")
	t.Setenv(EnvAgentModel, "")
	t.Setenv(EnvAgentAPIKey, "")

	_, err := NewAgentFromEnv()
	if err == nil {
		t.Fatal("expected error for missing env, got nil")
	}
	if !errors.Is(err, ErrAgentNotConfigured) {
		t.Errorf("expected ErrAgentNotConfigured, got %v", err)
	}
}

func TestNewAgentFromEnv_PartialEnvErrors(t *testing.T) {
	t.Setenv(EnvAgentEndpoint, "http://localhost:11434/v1")
	t.Setenv(EnvAgentModel, "")

	_, err := NewAgentFromEnv()
	if !errors.Is(err, ErrAgentNotConfigured) {
		t.Errorf("expected ErrAgentNotConfigured for missing model, got %v", err)
	}
}

func TestNewAgentFromEnv_TrimsTrailingSlash(t *testing.T) {
	t.Setenv(EnvAgentEndpoint, "http://localhost:11434/v1/")
	t.Setenv(EnvAgentModel, "qwen2.5:7b")
	t.Setenv(EnvAgentAPIKey, "")

	agent, err := NewAgentFromEnv()
	if err != nil {
		t.Fatalf("NewAgentFromEnv: %v", err)
	}
	if agent.Endpoint() != "http://localhost:11434/v1" {
		t.Errorf("Endpoint = %q, want trailing slash stripped", agent.Endpoint())
	}
	if agent.Model() != "qwen2.5:7b" {
		t.Errorf("Model = %q", agent.Model())
	}
}

// --- Request body shape ------------------------------------------------

func TestAgentExtract_RequestBodyShape(t *testing.T) {
	agent, captured, _ := startFakeAgent(t, fakeAgentResponse("# Hello\n"), http.StatusOK)

	_, err := agent.Extract(context.Background(), "<html><body>hi</body></html>", "text/html")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if captured.method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.method)
	}
	if captured.path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", captured.path)
	}
	if !strings.HasPrefix(captured.contentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", captured.contentType)
	}
	if captured.authorization != "" {
		t.Errorf("Authorization header set without API key: %q", captured.authorization)
	}
	if captured.parsedBody.Model != "test-model" {
		t.Errorf("body.model = %q, want test-model", captured.parsedBody.Model)
	}
	if captured.parsedBody.Temperature != 0 {
		t.Errorf("body.temperature = %v, want 0", captured.parsedBody.Temperature)
	}
	if captured.parsedBody.Stream {
		t.Error("body.stream = true, want false")
	}
	if got := len(captured.parsedBody.Messages); got != 2 {
		t.Fatalf("body.messages len = %d, want 2", got)
	}
	if captured.parsedBody.Messages[0].Role != "system" {
		t.Errorf("messages[0].role = %q, want system", captured.parsedBody.Messages[0].Role)
	}
	if !strings.Contains(captured.parsedBody.Messages[0].Content, "documentation extractor") {
		t.Errorf("system prompt missing identity, got %q", captured.parsedBody.Messages[0].Content)
	}
	if captured.parsedBody.Messages[1].Role != "user" {
		t.Errorf("messages[1].role = %q, want user", captured.parsedBody.Messages[1].Role)
	}
	if !strings.Contains(captured.parsedBody.Messages[1].Content, "raw HTML") {
		t.Errorf("user message missing HTML hint, got %q", captured.parsedBody.Messages[1].Content)
	}
	if !strings.Contains(captured.parsedBody.Messages[1].Content, "<html>") {
		t.Errorf("user message missing source content, got %q", captured.parsedBody.Messages[1].Content)
	}
}

func TestAgentExtract_AuthHeaderWhenAPIKeySet(t *testing.T) {
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.authorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(fakeAgentResponse("ok")))
	}))
	defer srv.Close()

	agent := NewAgent(srv.URL+"/v1", "test-model", "sk-secret-token", srv.Client())
	if _, err := agent.Extract(context.Background(), "hello", "text/plain"); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if captured.authorization != "Bearer sk-secret-token" {
		t.Errorf("Authorization = %q, want %q", captured.authorization, "Bearer sk-secret-token")
	}
}

func TestAgentExtract_PropagatesNon200(t *testing.T) {
	agent, _, _ := startFakeAgent(t, "", http.StatusUnauthorized)
	_, err := agent.Extract(context.Background(), "hello", "text/plain")
	if err == nil {
		t.Fatal("expected error for HTTP 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention HTTP 401, got %v", err)
	}
}

func TestAgentExtract_EmptyContentRejected(t *testing.T) {
	agent, _, _ := startFakeAgent(t, fakeAgentResponse("ignored"), http.StatusOK)
	_, err := agent.Extract(context.Background(), "   \n\t  ", "text/plain")
	if err == nil {
		t.Fatal("expected error for empty content")
	}
}

func TestAgentExtract_TruncatesOversizedInput(t *testing.T) {
	agent, captured, _ := startFakeAgent(t, fakeAgentResponse("# truncated"), http.StatusOK)

	huge := strings.Repeat("A", agentInputMaxChars+10_000)
	if _, err := agent.Extract(context.Background(), huge, "text/plain"); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// The user message is "<hint>\n\n<content>". The content section
	// must be exactly agentInputMaxChars bytes long, never more.
	user := captured.parsedBody.Messages[1].Content
	parts := strings.SplitN(user, "\n\n", 2)
	if len(parts) != 2 {
		t.Fatalf("user message missing hint separator, got %q", user)
	}
	if got := len(parts[1]); got != agentInputMaxChars {
		t.Errorf("truncated content length = %d, want %d", got, agentInputMaxChars)
	}
}

// --- Ping --------------------------------------------------------------

func TestAgentPing_Succeeds(t *testing.T) {
	agent, captured, _ := startFakeAgent(t, fakeAgentResponse("ok"), http.StatusOK)
	if err := agent.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if captured.path != "/v1/chat/completions" {
		t.Errorf("Ping hit %q, want /v1/chat/completions", captured.path)
	}
}

func TestAgentPing_FailsOnUnreachable(t *testing.T) {
	// Wire the agent at a port nothing is listening on. The error
	// path doesn't care about the exact dial error — only that it
	// surfaces with the "agent ping" prefix.
	agent := NewAgent("http://127.0.0.1:1/v1", "test-model", "", &http.Client{})
	err := agent.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error pinging unreachable endpoint")
	}
	if !strings.Contains(err.Error(), "agent ping") {
		t.Errorf("error %q missing 'agent ping' prefix", err.Error())
	}
}

// --- verifyCodeBlocks --------------------------------------------------

func TestVerifyCodeBlocks_AcceptsMatchingBlocks(t *testing.T) {
	source := "Some text\n\n```go\nfmt.Println(\"hi\")\n```\nmore text"
	md := "## Title\n\n```go\nfmt.Println(\"hi\")\n```\n"

	if !verifyCodeBlocks(md, source) {
		t.Errorf("expected verify to pass with matching code block")
	}
}

func TestVerifyCodeBlocks_RejectsHallucinatedBlock(t *testing.T) {
	source := "documentation describing how to call the API"
	md := "## API\n\n```go\ndeleteUniverse() // does not exist in source\n```\n"

	if verifyCodeBlocks(md, source) {
		t.Errorf("expected verify to reject hallucinated code block")
	}
}

func TestVerifyCodeBlocks_AcceptsProseOnly(t *testing.T) {
	source := "Some prose with no code at all."
	md := "## Title\n\nClean markdown summary, no fences here.\n"

	if !verifyCodeBlocks(md, source) {
		t.Errorf("expected verify to accept prose-only output")
	}
}

func TestVerifyCodeBlocks_StrictWhitespace(t *testing.T) {
	// Source has 4-space indent; md has tab indent. Strict-by-default
	// per #27 means this is rejected, not accepted.
	source := "Example:\n\n    indented code line\n"
	md := "```\n\tindented code line\n```\n"

	if verifyCodeBlocks(md, source) {
		t.Errorf("expected verify to reject whitespace-divergent code")
	}
}

func TestVerifyCodeBlocks_HandlesTildeFences(t *testing.T) {
	source := "Example:\n\nfoo\nbar\n"
	md := "~~~\nfoo\nbar\n~~~\n"

	if !verifyCodeBlocks(md, source) {
		t.Errorf("expected verify to accept matching tilde-fenced block")
	}
}

func TestVerifyCodeBlocks_MultipleBlocksAllChecked(t *testing.T) {
	source := "alpha\n```\nfirst\n```\nbeta\n```\nsecond\n```\n"
	mdGood := "```\nfirst\n```\n\nand\n\n```\nsecond\n```\n"
	mdBad := "```\nfirst\n```\n\nand\n\n```\nsecond_modified\n```\n"

	if !verifyCodeBlocks(mdGood, source) {
		t.Errorf("expected good multi-block to pass")
	}
	if verifyCodeBlocks(mdBad, source) {
		t.Errorf("expected bad multi-block to fail (one bad block)")
	}
}

// --- contentTypeHint ---------------------------------------------------

func TestContentTypeHint_VariesByType(t *testing.T) {
	cases := map[string]string{
		"text/html":               "raw HTML",
		"text/markdown":           "Markdown",
		"text/plain":              "plain text",
		"application/octet-stream": "documentation content", // fallback
	}
	for ct, fragment := range cases {
		hint := contentTypeHint(ct)
		if !strings.Contains(hint, fragment) {
			t.Errorf("contentTypeHint(%q) = %q, want fragment %q", ct, hint, fragment)
		}
	}
}

// --- FetchOneViaAgent end-to-end ---------------------------------------

func TestFetchOneViaAgent_HappyPath(t *testing.T) {
	// Source server: serves an HTML page on GET /docs.
	sourceHTML := `<html><body><h1>Page</h1><pre><code>foo()</code></pre></body></html>`
	sourceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(sourceHTML))
	}))
	defer sourceSrv.Close()

	// Agent server: returns a markdown extraction whose code block
	// is verbatim from the source so the verifier accepts it.
	extracted := "# Page\n\n```\nfoo()\n```\n"
	agent, _, _ := startFakeAgent(t, fakeAgentResponse(extracted), http.StatusOK)

	res, err := FetchOneViaAgent(context.Background(), sourceSrv.Client(), agent, "/test/lib", sourceSrv.URL+"/docs")
	if err != nil {
		t.Fatalf("FetchOneViaAgent: %v", err)
	}
	if res.Bytes != len(sourceHTML) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(sourceHTML))
	}
	if len(res.Docs) == 0 {
		t.Fatal("expected at least one parsed doc, got 0")
	}
	for _, d := range res.Docs {
		if d.LibID != "/test/lib" {
			t.Errorf("doc has LibID %q, want /test/lib", d.LibID)
		}
	}
}

func TestFetchOneViaAgent_HallucinatedCodeRejected(t *testing.T) {
	sourceHTML := `<html><body>just prose, no code</body></html>`
	sourceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(sourceHTML))
	}))
	defer sourceSrv.Close()

	extracted := "# Page\n\n```\ninvented_code_block()\n```\n"
	agent, _, _ := startFakeAgent(t, fakeAgentResponse(extracted), http.StatusOK)

	_, err := FetchOneViaAgent(context.Background(), sourceSrv.Client(), agent, "/test/lib", sourceSrv.URL+"/docs")
	if err == nil {
		t.Fatal("expected verification failure, got nil")
	}
	if !errors.Is(err, ErrAgentVerificationFailed) {
		t.Errorf("expected ErrAgentVerificationFailed, got %v", err)
	}
}

func TestFetchOneViaAgent_PDFContentTypeRejected(t *testing.T) {
	sourceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.7\ndummy"))
	}))
	defer sourceSrv.Close()

	agent, _, _ := startFakeAgent(t, fakeAgentResponse("ignored"), http.StatusOK)

	_, err := FetchOneViaAgent(context.Background(), sourceSrv.Client(), agent, "/test/lib", sourceSrv.URL+"/spec.pdf")
	if err == nil {
		t.Fatal("expected error for PDF content type, got nil")
	}
	if !errors.Is(err, ErrPDFNotSupportedYet) {
		t.Errorf("expected ErrPDFNotSupportedYet, got %v", err)
	}
}

func TestFetchOneViaAgent_NilAgentReturnsError(t *testing.T) {
	sourceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hi"))
	}))
	defer sourceSrv.Close()

	_, err := FetchOneViaAgent(context.Background(), sourceSrv.Client(), nil, "/test/lib", sourceSrv.URL)
	if err == nil {
		t.Fatal("expected error for nil agent")
	}
	if !strings.Contains(err.Error(), "agent is nil") {
		t.Errorf("error %q should mention nil agent", err.Error())
	}
}
