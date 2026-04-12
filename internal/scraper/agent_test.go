package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
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

// assertReasoningDisabled fails the test if the captured request body
// is missing any of the three reasoning-off fields the agent must
// always send. See agent.go's chatRequest doc and #60 for the why.
//
// Both the parsed body and the raw bytes are checked: omitempty on
// these fields would silently drop a wrong-typed value and make a
// parsed-only assertion trivially pass, so we walk the JSON literal
// too and prove the fields actually serialize on the wire.
func assertReasoningDisabled(t *testing.T, captured *capturedRequest) {
	t.Helper()

	if captured.parsedBody.ChatTemplateKwargs == nil {
		t.Errorf("chat_template_kwargs missing from request body")
	} else {
		v, ok := captured.parsedBody.ChatTemplateKwargs["enable_thinking"]
		if !ok {
			t.Errorf("chat_template_kwargs.enable_thinking key missing")
		} else if b, isBool := v.(bool); !isBool || b {
			t.Errorf("chat_template_kwargs.enable_thinking = %v (bool=%v), want false", v, isBool)
		}
	}
	if captured.parsedBody.ReasoningEffort != "minimal" {
		t.Errorf("reasoning_effort = %q, want %q", captured.parsedBody.ReasoningEffort, "minimal")
	}
	if captured.parsedBody.EnableThinking == nil {
		t.Errorf("enable_thinking is nil, want explicit false (omitempty + *bool)")
	} else if *captured.parsedBody.EnableThinking {
		t.Errorf("enable_thinking = true, want false")
	}

	if !bytes.Contains(captured.rawBody, []byte(`"chat_template_kwargs":{"enable_thinking":false}`)) {
		t.Errorf("raw body missing literal `\"chat_template_kwargs\":{\"enable_thinking\":false}`, got: %s", captured.rawBody)
	}
	if !bytes.Contains(captured.rawBody, []byte(`"reasoning_effort":"minimal"`)) {
		t.Errorf("raw body missing literal `\"reasoning_effort\":\"minimal\"`, got: %s", captured.rawBody)
	}
	if !bytes.Contains(captured.rawBody, []byte(`"enable_thinking":false`)) {
		t.Errorf("raw body missing literal `\"enable_thinking\":false`, got: %s", captured.rawBody)
	}
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
	// Strict raw-body check: float64's zero value would make a parsed
	// `Temperature != 0` assertion trivially true even if the field
	// were dropped from the JSON entirely. Walk the bytes to prove the
	// field is actually serialized as the literal "temperature":0.
	if !bytes.Contains(captured.rawBody, []byte(`"temperature":0`)) {
		t.Errorf("raw body missing literal `\"temperature\":0`, got: %s", captured.rawBody)
	}
	if !bytes.Contains(captured.rawBody, []byte(`"stream":false`)) {
		t.Errorf("raw body missing literal `\"stream\":false`, got: %s", captured.rawBody)
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
	// #60: every Extract call must opt out of reasoning-mode output
	// on Qwen3+ / DeepSeek-R1 / OpenAI o-series. Reasoning is pure
	// waste for HTML→Markdown extraction (3–6× latency, up to 268×
	// on trivial pings) and the system prompt already says "no
	// commentary".
	assertReasoningDisabled(t, captured)
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

func TestAgentExtract_DropsReasoningContentFromResponse(t *testing.T) {
	// #60 safety net: if a server ignores our reasoning-off hint
	// (or the user is on a backend that doesn't honor any of the
	// three knobs), the response will look like Qwen3+ / DeepSeek-R1
	// shape — content plus reasoning_content side-by-side. Our
	// chatResponse struct only declares Content, so reasoning_content
	// must drop on the json.Unmarshal floor and never reach the
	// caller. Lock that behavior in.
	reply := `{"choices":[{"message":{"role":"assistant","content":"# Clean output\n","reasoning_content":"Thinking Process: let me carefully consider how to extract this page..."}}]}`
	agent, _, _ := startFakeAgent(t, reply, http.StatusOK)

	out, err := agent.Extract(context.Background(), "<html><body>hi</body></html>", "text/html")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if out != "# Clean output" {
		t.Errorf("Extract returned %q, want %q", out, "# Clean output")
	}
	if strings.Contains(out, "Thinking Process") {
		t.Errorf("Extract leaked reasoning_content into output: %q", out)
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

func TestAgentPing_DisablesReasoning(t *testing.T) {
	// #60: Ping is just as wasteful as Extract on a reasoning-mode
	// model — without the disable, a one-token health check burns
	// ~268 completion tokens on Qwen3.5-9B. Verify the same three
	// fields apply to the ping path.
	agent, captured, _ := startFakeAgent(t, fakeAgentResponse("ok"), http.StatusOK)
	if err := agent.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	assertReasoningDisabled(t, captured)
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

// TestAgentExtract_LongDeadline pins the default per-request timeout
// to its post-#63 value. A regression to 5 min would re-open the
// smoke-test failure mode (see #63's repro table), so this assertion
// exists purely to make that regression loud.
func TestAgentExtract_LongDeadline(t *testing.T) {
	agent := NewAgent("http://example.invalid/v1", "test-model", "", nil)
	got := agent.client.Timeout
	if got < 15*time.Minute {
		t.Errorf("default client timeout = %v, want ≥15m (post-#63 floor); 5m regresses the smoke test", got)
	}
}

// TestAgentPing_FastTimeout verifies Ping honors agentPingTimeout
// independently of the per-request HTTP client timeout. A stuck
// endpoint with a 20-minute client timeout must still bail in ~ping
// budget via the context deadline.
func TestAgentPing_FastTimeout(t *testing.T) {
	// Block the server past any reasonable ping budget. If Ping
	// doesn't apply its own ctx deadline, the test would either time
	// out on go-test itself or wait the full client timeout — both
	// are louder failures than a green pass.
	//
	// serverDone is closed by t.Cleanup before srv.Close so the
	// handler returns promptly; httptest.Server.Close() waits for
	// active connections and would otherwise hang if r.Context().Done
	// fires late relative to the client tearing down its side.
	serverDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-serverDone:
		}
	}))
	t.Cleanup(func() {
		close(serverDone)
		srv.Close()
	})

	// Shrink agentPingTimeout so the test takes ~200ms instead of 30s.
	// Client timeout is deliberately long (mirrors production's 20m)
	// so a pass proves the ping timeout — not the client — short-
	// circuited the request.
	prevPing := agentPingTimeout
	agentPingTimeout = 200 * time.Millisecond
	defer func() { agentPingTimeout = prevPing }()

	agent := NewAgent(srv.URL+"/v1", "test-model", "", &http.Client{Timeout: 30 * time.Second})

	start := time.Now()
	err := agent.Ping(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error on hung endpoint")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded wrapped in err, got %v", err)
	}
	// Allow 2s slack for scheduler + handshake; production budget is
	// 30s so this cap needs to be well under it to be meaningful.
	if elapsed > 2*time.Second {
		t.Errorf("Ping took %v, expected it to bail near agentPingTimeout (%v)", elapsed, agentPingTimeout)
	}
}

// TestAgentDo_ReturnsHTTPStatusError verifies do() wraps non-200 as
// *HTTPStatusError (reachable via errors.As) instead of a plain
// fmt.Errorf. cmd/scraper relies on this to classify 5xx as transient.
func TestAgentDo_ReturnsHTTPStatusError(t *testing.T) {
	agent, _, _ := startFakeAgent(t, "", http.StatusServiceUnavailable)
	_, err := agent.Extract(context.Background(), "hello", "text/plain")
	if err == nil {
		t.Fatal("expected error on HTTP 503")
	}
	var httpErr *HTTPStatusError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *HTTPStatusError via errors.As, got %T: %v", err, err)
	}
	if httpErr.Status != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want 503", httpErr.Status)
	}
}

// --- IsTransientAgentError --------------------------------------------

func TestIsTransientAgentError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped deadline exceeded", fmt.Errorf("extract: %w", context.DeadlineExceeded), true},
		{"canceled (not transient)", context.Canceled, false},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"ECONNRESET", syscall.ECONNRESET, true},
		{"EPIPE", syscall.EPIPE, true},
		{"wrapped ECONNRESET", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{"http 503", &HTTPStatusError{Status: 503, URL: "x"}, true},
		{"http 500", &HTTPStatusError{Status: 500, URL: "x"}, true},
		{"http 599", &HTTPStatusError{Status: 599, URL: "x"}, true},
		{"http 401 (not transient)", &HTTPStatusError{Status: 401, URL: "x"}, false},
		{"http 404 (not transient)", &HTTPStatusError{Status: 404, URL: "x"}, false},
		{"wrapped http 502", fmt.Errorf("extract: %w", &HTTPStatusError{Status: 502, URL: "x"}), true},
		{"plain string error", errors.New("something else"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsTransientAgentError(tc.err)
			if got != tc.want {
				t.Errorf("IsTransientAgentError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// --- FetchOneViaAgent: Content-Type gate before body read -------------

// TestFetchOneViaAgent_GatesContentTypeBeforeRead verifies the
// Content-Type gate runs before io.ReadAll so an unsupported response
// (here a PDF) is rejected without the body ever being consumed.
// Implementation: the server writes the headers, flushes, then blocks
// until its request context cancels. If FetchOneViaAgent reads the
// body it will hang on io.ReadAll and the test fails the 2-second
// deadline; if it gates first, the call returns ErrPDFNotSupportedYet
// within microseconds and the server handler unblocks via the client
// closing the connection.
func TestFetchOneViaAgent_GatesContentTypeBeforeRead(t *testing.T) {
	serverDone := make(chan struct{})
	defer close(serverDone)
	sourceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-serverDone:
		}
	}))
	defer sourceSrv.Close()

	agent, _, _ := startFakeAgent(t, fakeAgentResponse("ignored"), http.StatusOK)

	done := make(chan error, 1)
	go func() {
		_, err := FetchOneViaAgent(context.Background(), sourceSrv.Client(), agent, "/test/lib", sourceSrv.URL+"/spec.pdf")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected ErrPDFNotSupportedYet, got nil")
		}
		if !errors.Is(err, ErrPDFNotSupportedYet) {
			t.Errorf("expected ErrPDFNotSupportedYet, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FetchOneViaAgent did not return within 2s; likely read the response body instead of gating on Content-Type")
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
