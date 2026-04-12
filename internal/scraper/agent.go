package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

// Environment variables consumed by NewAgentFromEnv. Documented in the
// README "Scraping non-trivial doc sources" section. The agent endpoint
// is per-user infrastructure (Ollama on localhost, a shared corporate
// vLLM, OpenAI proper, ...), not project metadata, which is why these
// live in env rather than libraries_sources.yaml.
const (
	EnvAgentEndpoint = "DEADZONE_AGENT_ENDPOINT"
	EnvAgentModel    = "DEADZONE_AGENT_ENDPOINT_MODEL"
	EnvAgentAPIKey   = "DEADZONE_AGENT_ENDPOINT_API_KEY"
)

// agentInputMaxChars is the conservative cap applied to LLM input on
// the scrape-via-agent path. Inputs longer than this are truncated to
// this exact length and a warning is logged once per affected URL.
//
// 48 KiB ≈ 12k tokens for typical English-with-markup, which fits an
// 8k–16k context window with comfortable headroom for the system
// prompt and the model's own response. Modern local models (qwen2.5,
// llama3.1) easily exceed this in their default config; the cap exists
// for the small-context models that are common on consumer hardware.
//
// v1 deliberately ships a single constant rather than a tunable. The
// follow-up listed in #27 is "smart chunking" — section-aware splitting
// and multi-call extraction — which is the right place to revisit this.
const agentInputMaxChars = 48 * 1024

// agentDefaultTimeout caps a single LLM call. Sized so a cold-start
// reload on consumer hardware (oMLX LRU-evicting the model, a first
// large-context generation on a 9B class model) fits comfortably, while
// a genuinely hung endpoint still surfaces in the operator log within
// a reasonable window. 5 min turned out to be the exact failure point
// during the FastAPI smoke test (see #63); 20 min gives 4× headroom.
const agentDefaultTimeout = 20 * time.Minute

// agentPingTimeout caps Ping independently of agentDefaultTimeout.
// Ping is a startup health check: the operator wants a misconfigured
// endpoint to surface in seconds, not minutes. 30 s covers slow
// handshake + first-token latency on any reasonable local model and
// bails fast if the endpoint is dead. Kept as a var, not a const, so
// tests can shrink it without waiting the full budget.
var agentPingTimeout = 30 * time.Second

// systemPrompt is the extraction instruction shared by Ping and Extract.
// Locked at temperature 0, single-shot, no streaming. The prompt itself
// is content-type agnostic — Extract prepends a one-line hint so
// smaller models know whether they're looking at HTML, markdown, or
// plain text without changing the rules.
const systemPrompt = `You are a documentation extractor.
Given a technical documentation page, return clean Markdown.

Rules:
- Preserve every code block VERBATIM — do not paraphrase, rewrite, or
  format any code. Keep original indentation and whitespace.
- Preserve headings, lists, tables, and their hierarchy.
- Remove navigation, sidebars, footers, ads, breadcrumbs, table-of-contents
  widgets, and any site chrome that is not documentation content.
- Do NOT invent content. Do NOT add explanations. Do NOT summarize.
- If a section has no meaningful documentation content, omit it.
- Output ONLY the extracted Markdown. No preamble, no commentary.`

// ErrAgentNotConfigured is returned by NewAgentFromEnv when the
// required env vars are missing. Wrap with errors.Is to detect — the
// scraper main loop uses it to print a single actionable startup error
// instead of a generic env-var dump.
var ErrAgentNotConfigured = errors.New("agent endpoint not configured (set " + EnvAgentEndpoint + " and " + EnvAgentModel + ")")

// ErrAgentVerificationFailed is returned by FetchOneViaAgent when the
// LLM's output contains a fenced code block that does not appear in
// the source content (likely a hallucination). The doc is dropped and
// the failure is logged at scraper.agent_verification_failed.
var ErrAgentVerificationFailed = errors.New("agent output failed code-block verification")

// HTTPStatusError carries a non-200 HTTP status code from either the
// agent endpoint (Agent.do) or the source URL fetch (FetchOneViaAgent).
// Exported so cmd/scraper can classify 5xx as transient-and-soft-fail
// vs 4xx as likely-misconfiguration-and-hard-fail via errors.As.
type HTTPStatusError struct {
	Status int
	URL    string
	Body   string // truncated response body snippet, may be empty
}

func (e *HTTPStatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("HTTP %d from %s: %s", e.Status, e.URL, e.Body)
	}
	return fmt.Sprintf("HTTP %d from %s", e.Status, e.URL)
}

// IsTransientAgentError reports whether err represents a transient
// failure that should soft-skip the URL rather than abort the whole
// lib. Matches: context.DeadlineExceeded, net.Error.Timeout, ECONNRESET
// / EPIPE, unexpected EOF during response read, and 5xx HTTP status.
// Callers in cmd/scraper combine this with ErrAgentVerificationFailed
// (intentional per-URL drop) under a shared skippedThisLib ceiling.
func IsTransientAgentError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) {
		return httpErr.Status >= 500 && httpErr.Status < 600
	}
	return false
}

// Agent drives LLM-backed content extraction via an OpenAI-compatible
// /v1/chat/completions endpoint. Deadzone does not host an LLM — the
// user brings their own runtime (Ollama, vLLM, LocalAI, OpenAI, ...).
// All Agent does is build the request, ship it, and parse the
// response.
//
// Concurrency: Agent is safe for concurrent use as long as the
// embedded http.Client is. The default constructor wires a fresh
// http.Client per Agent, so you get that for free.
type Agent struct {
	// endpoint is the base URL of the OpenAI-compatible API, e.g.
	// "http://localhost:11434/v1". Trailing slashes are stripped at
	// construction time so requestURL can append "/chat/completions"
	// unconditionally.
	endpoint string

	// model is the model name to pass in the request body, e.g.
	// "qwen2.5:7b" for Ollama or "gpt-4o-mini" for OpenAI proper.
	model string

	// apiKey is the optional bearer token. Empty means no
	// Authorization header is sent — required for unauthenticated
	// localhost runtimes like Ollama.
	apiKey string

	client *http.Client
}

// NewAgentFromEnv constructs an Agent from the three DEADZONE_AGENT_*
// env vars. Endpoint and model are required; the API key is optional
// (Ollama and most local runtimes don't need one). Returns
// ErrAgentNotConfigured if either required var is unset, so callers
// can distinguish "user forgot to set env" from "endpoint is bad".
func NewAgentFromEnv() (*Agent, error) {
	endpoint := strings.TrimSpace(os.Getenv(EnvAgentEndpoint))
	model := strings.TrimSpace(os.Getenv(EnvAgentModel))
	if endpoint == "" || model == "" {
		return nil, ErrAgentNotConfigured
	}
	apiKey := strings.TrimSpace(os.Getenv(EnvAgentAPIKey))
	return NewAgent(endpoint, model, apiKey, nil), nil
}

// NewAgent is the explicit constructor used by tests (which inject an
// httptest.Server URL) and by NewAgentFromEnv. Passing client == nil
// gives you a fresh http.Client with agentDefaultTimeout.
func NewAgent(endpoint, model, apiKey string, client *http.Client) *Agent {
	if client == nil {
		client = &http.Client{Timeout: agentDefaultTimeout}
	}
	return &Agent{
		endpoint: strings.TrimRight(endpoint, "/"),
		model:    model,
		apiKey:   apiKey,
		client:   client,
	}
}

// Endpoint returns the base URL the agent talks to. Exposed for the
// startup log line so an operator can see which runtime they wired up.
func (a *Agent) Endpoint() string { return a.endpoint }

// Model returns the configured model name (for the startup log line).
func (a *Agent) Model() string { return a.model }

// chatRequest is the OpenAI-compatible /v1/chat/completions request body.
// Deadzone speaks the smallest possible subset: model, messages, and the
// determinism / single-shot knobs. No tool calling, no JSON mode, no
// response_format — the system prompt already says "output only Markdown".
//
// The trailing three fields — ChatTemplateKwargs, ReasoningEffort, and
// EnableThinking — all serve the same purpose: suppress reasoning-mode
// output on reasoning-capable backends (Qwen3+, GLM-4-Reasoning, OpenAI
// o-series, DeepSeek-R1, …). Deadzone's extraction task has zero use
// for a chain of thought — the system prompt already says "output only
// Markdown, no commentary" — and reasoning-on burns 3–6× the latency
// per URL (268× on trivial ping traffic, measured against oMLX +
// Qwen3.5-9B-MLX-4bit on 2026-04-11; see #60 for the full table).
//
// There is no standardized OpenAI field for "disable reasoning"; each
// server family picked its own convention. Servers silently ignore
// fields they don't recognize (this is part of the OpenAI spec's
// permissive request handling), so we send all three in every request
// and one code path covers every reasoning backend we might meet. If
// a future reviewer thinks these fields are unused and wants to strip
// them, the answer is in #60 — they are load-bearing on any 2026-era
// reasoning model and silently ignored everywhere else.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`

	// ChatTemplateKwargs carries Jinja-template kwargs to servers
	// that pass them through to the model's chat template — oMLX,
	// vLLM, Ollama, sglang. Setting {"enable_thinking": false}
	// disables reasoning on Qwen3+ and GLM-4-Reasoning.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`

	// ReasoningEffort is the OpenAI o-series knob (o1/o3/o5). We
	// send "minimal" to cap reasoning at the lowest tier; other
	// servers ignore this field.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	// EnableThinking is the DeepSeek-R1 family's top-level toggle.
	// *bool so the encoder distinguishes "unset" (nil → omitted)
	// from "explicit false" — omitempty on a plain bool would drop
	// the false we need to send.
	EnableThinking *bool `json:"enable_thinking,omitempty"`
}

// disableReasoning populates the three reasoning-off knobs on req.
// Applied by both Ping and Extract before do(). See chatRequest for
// per-field rationale and #60 for the empirical token measurement.
func disableReasoning(req *chatRequest) {
	req.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	req.ReasoningEffort = "minimal"
	off := false
	req.EnableThinking = &off
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the subset of the OpenAI-compatible response we read.
// We tolerate extra fields (Ollama and vLLM both add their own) by
// letting json.Unmarshal silently drop them.
type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// Ping sends a trivial chat completion to the configured endpoint to
// verify it is reachable, the model is loaded, and the API key (if
// any) is accepted. Called once at scraper startup when at least one
// source has kind=scrape-via-agent, so the operator finds out about a
// misconfigured endpoint before any URLs are processed.
//
// Ping uses the same code path as Extract — same URL, same auth, same
// response shape — so a successful Ping implies the actual extraction
// calls have a working transport.
func (a *Agent) Ping(ctx context.Context) error {
	// Wrap the caller's context with agentPingTimeout (30s) so a dead
	// endpoint surfaces fast regardless of the client's per-request
	// timeout (20 min for Extract). Decoupled from agentDefaultTimeout
	// so future bumps to Extract's budget don't slow Ping down.
	pingCtx, cancel := context.WithTimeout(ctx, agentPingTimeout)
	defer cancel()

	req := chatRequest{
		Model: a.model,
		Messages: []chatMessage{
			{Role: "system", Content: "You are a health check responder."},
			{Role: "user", Content: "Reply with the single word: ok"},
		},
		Temperature: 0,
		Stream:      false,
	}
	disableReasoning(&req)
	if _, err := a.do(pingCtx, req); err != nil {
		return fmt.Errorf("agent ping: %w", err)
	}
	return nil
}

// Extract normalizes content into clean Markdown via the LLM.
//
// contentType drives a one-line hint prepended to the user message so
// smaller models know what they're looking at. The system prompt
// itself is identical regardless of input type — the rules
// (preserve code verbatim, drop chrome, no commentary) don't depend
// on whether the source was HTML or PDF text.
//
// Inputs longer than agentInputMaxChars are truncated to that exact
// length and a slog.Warn is emitted via slog.Default(). v1 ships a
// single constant cap; smart chunking is a follow-up listed in #27.
func (a *Agent) Extract(ctx context.Context, content, contentType string) (string, error) {
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("agent extract: empty content")
	}

	if len(content) > agentInputMaxChars {
		slog.Warn("agent.input_truncated",
			"content_type", contentType,
			"original_chars", len(content),
			"truncated_to", agentInputMaxChars,
		)
		content = content[:agentInputMaxChars]
	}

	hint := contentTypeHint(contentType)
	userMsg := hint + "\n\n" + content

	req := chatRequest{
		Model: a.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userMsg},
		},
		Temperature: 0,
		Stream:      false,
	}
	disableReasoning(&req)

	resp, err := a.do(ctx, req)
	if err != nil {
		return "", fmt.Errorf("agent extract: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("agent extract: response has no choices")
	}
	out := strings.TrimSpace(resp.Choices[0].Message.Content)
	if out == "" {
		return "", fmt.Errorf("agent extract: empty response content")
	}
	return out, nil
}

// contentTypeHint produces a one-line nudge prepended to the user
// message. Locked-in strings so the prompt is fully reproducible from
// the source.
func contentTypeHint(contentType string) string {
	switch normalizeContentType(contentType) {
	case contentTypeHTML, contentTypeXHTML:
		return "The input below is raw HTML scraped from a documentation page."
	case contentTypeMarkdown, contentTypeXMD:
		return "The input below is Markdown that may be templated, dense, or contain site chrome to remove."
	case contentTypePlain:
		return "The input below is plain text extracted from a documentation page."
	default:
		// Anything else should have been rejected by preprocess
		// already, but stay on the safe side with a generic hint
		// rather than panicking.
		return "The input below is documentation content."
	}
}

// do executes one chat completion request against a.endpoint and
// returns the parsed response. Centralised so Ping and Extract share
// the same auth, error wrapping, and response decoding.
func (a *Agent) do(ctx context.Context, body chatRequest) (*chatResponse, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := a.endpoint + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build request %s: %w", url, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("read response %s: %w", url, readErr)
	}

	if resp.StatusCode != http.StatusOK {
		// Trim noisy bodies but keep enough to debug — most OpenAI-
		// compatible servers return a JSON error with a useful
		// "message" field that's well under 512 bytes.
		snippet := string(respBody)
		if len(snippet) > 512 {
			snippet = snippet[:512] + "…"
		}
		return nil, &HTTPStatusError{Status: resp.StatusCode, URL: url, Body: snippet}
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("decode response %s: %w", url, err)
	}
	return &parsed, nil
}

// verifyCodeBlocks returns true iff every fenced code block in md
// appears as a verbatim substring of source.
//
// Catches the most dangerous LLM failure mode: a hallucinated code
// example that looks plausible but doesn't exist in the source page.
// Strict by default — whitespace and indentation must match exactly.
// Documented as a known limitation: prose hallucination is still
// possible and will be tightened in a follow-up.
//
// "Empty md" and "md with no fenced blocks" both return true. Prose-
// only pages are legitimate output and shouldn't be rejected just for
// not containing code.
func verifyCodeBlocks(md, source string) bool {
	for _, block := range extractFencedBlocks(md) {
		if !strings.Contains(source, block) {
			return false
		}
	}
	return true
}

// extractFencedBlocks pulls the inner text of every fenced code block
// in md. Recognises both backtick (```) and tilde (~~~) fences with an
// optional language tag on the opening line. The closing fence must
// match the opener (a ``` block stays open until the next ```; a ~~~
// block stays open until the next ~~~).
//
// Returned strings are the raw inner content with the original line
// breaks preserved — no trimming, no language tag — so verifyCodeBlocks
// can do a literal substring check against the source.
func extractFencedBlocks(md string) []string {
	var blocks []string
	lines := strings.Split(md, "\n")

	var (
		inFence    bool
		fenceChar  byte // '`' or '~' so the closer matches the opener
		current    strings.Builder
		firstChunk bool
	)

	for _, line := range lines {
		stripped := strings.TrimLeft(line, " \t")

		if !inFence {
			if strings.HasPrefix(stripped, "```") {
				inFence = true
				fenceChar = '`'
				current.Reset()
				firstChunk = true
				continue
			}
			if strings.HasPrefix(stripped, "~~~") {
				inFence = true
				fenceChar = '~'
				current.Reset()
				firstChunk = true
				continue
			}
			continue
		}

		// We're inside a fence. The closer is three of the same fence
		// character at the start of a (possibly indented) line.
		closer := strings.Repeat(string(fenceChar), 3)
		if strings.HasPrefix(stripped, closer) {
			blocks = append(blocks, current.String())
			inFence = false
			current.Reset()
			continue
		}

		// Accumulate the inner content. Don't prepend a newline to
		// the first chunk so a single-line block doesn't end up with
		// a leading "\n".
		if !firstChunk {
			current.WriteByte('\n')
		}
		current.WriteString(line)
		firstChunk = false
	}

	// Unterminated fence: ignore the partial block. The verifier
	// silently accepts it — better to miss a flag than reject a whole
	// otherwise-valid extraction over a stray triple-backtick in
	// trailing prose. Operators see the malformed output in the
	// indexed docs and can iterate on the prompt.

	return blocks
}
