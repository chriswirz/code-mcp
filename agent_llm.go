package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// The LLM side of the agent. One conversation representation is kept, and each
// endpoint style translates it into its own wire format on every request, so
// switching models mid-conversation keeps the history.

const (
	styleOpenAI      = "openai"      // POST {base}/chat/completions
	styleResponses   = "responses"   // POST {base}/responses
	styleAnthropic   = "anthropic"   // POST {base}/messages
	styleCompletions = "completions" // POST {base}/completions (legacy)
	styleOllama      = "ollama"      // POST {root}/api/chat
)

type chatMessage struct {
	Role       string // "user", "assistant" or "tool"
	Content    string
	ToolCalls  []toolCall // assistant only
	ToolCallID string     // tool only
	ToolName   string     // tool only
}

type toolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type toolSpec struct {
	Name        string
	Description string
	Schema      map[string]any
}

type llmReply struct {
	Text      string
	Reasoning string // the model's reasoning/thinking, when the endpoint sends it separately from Text
	ToolCalls []toolCall
	InTokens  int
	OutTokens int
}

// llmClient is one model endpoint, detected or configured.
type llmClient struct {
	cfg      AgentModelConfig
	base     string // normalized base URL
	style    string // resolved style
	endpoint string // resolved request URL
	model    string // resolved model id
	http     *http.Client
}

func newLLMClient(cfg AgentModelConfig) (*llmClient, error) {
	base, err := normalizeEndpointURL(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", cfg.Name, err)
	}
	c := &llmClient{
		cfg:   cfg,
		base:  base,
		model: cfg.Model,
		http:  newAgentHTTPClient(cfg.TimeoutSeconds, cfg.InsecureSkipVerify, 10*time.Minute),
	}
	if s := strings.ToLower(cfg.Style); s != "" && s != "auto" {
		c.style = s
		c.endpoint = c.candidates(s)[0]
	}
	return c, nil
}

// resetAutoDetected undoes what detect cached, for whichever of style and
// model this client was left to find on its own (style "" or "auto", model
// left blank) - an explicitly configured one is untouched. /retry calls this
// before trying again: a service that just restarted may now be serving a
// different model than the one detected last time, and reusing that stale
// guess would fail the retry for a reason that has nothing to do with what
// the caller actually changed.
func (c *llmClient) resetAutoDetected() {
	if c.cfg.Model == "" {
		c.model = ""
	}
	if s := strings.ToLower(c.cfg.Style); s == "" || s == "auto" {
		c.style = ""
		c.endpoint = ""
	}
}

func (c *llmClient) describe() string {
	style := c.style
	if style == "" {
		style = "auto (not yet detected)"
	}
	return fmt.Sprintf("%s  %s  model=%s  style=%s  auth=%s", c.cfg.Name, c.base,
		firstNonEmpty(c.model, "(first listed)"), style, c.cfg.Auth.describe())
}

// root is the base URL with a trailing /v1 removed.
func (c *llmClient) root() string {
	return strings.TrimSuffix(c.base, "/v1")
}

// candidates are the URLs a style may be served at, most likely first. Base
// URLs are given both with and without /v1, and Anthropic is commonly mounted
// under an /anthropic prefix on gateways that serve several dialects.
func (c *llmClient) candidates(style string) []string {
	hasV1 := strings.HasSuffix(c.base, "/v1")
	both := func(suffix string) []string {
		if hasV1 {
			return []string{c.base + suffix}
		}
		return []string{c.base + "/v1" + suffix, c.base + suffix}
	}
	switch style {
	case styleOpenAI:
		return both("/chat/completions")
	case styleResponses:
		return both("/responses")
	case styleCompletions:
		return both("/completions")
	case styleAnthropic:
		out := both("/messages")
		if !strings.HasSuffix(c.root(), "/anthropic") {
			out = append(out, c.root()+"/anthropic/v1/messages")
		}
		return out
	case styleOllama:
		return []string{c.root() + "/api/chat"}
	}
	return nil
}

func (c *llmClient) newRequest(ctx context.Context, method, target string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	defaultHeader := ""
	if c.style == styleAnthropic || strings.HasSuffix(target, "/messages") {
		req.Header.Set("anthropic-version", "2023-06-01")
		defaultHeader = "x-api-key"
	}
	c.cfg.Auth.apply(req, defaultHeader)
	applyHeaders(req, c.cfg.Headers)
	return req, nil
}

type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, truncateText(e.Body, 600))
}

func (c *llmClient) do(ctx context.Context, method, target string, body, out any) error {
	req, err := c.newRequest(ctx, method, target, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpStatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s answered with something that is not JSON: %s", target, truncateText(string(data), 200))
	}
	return nil
}

const (
	defaultRetryLimit              = 3
	defaultExponentialBackoffLimit = 30 * time.Second
)

// retryParams resolves agent.models[].retry_limit and
// exponential_backoff_limit to their effective values: a nil retry_limit
// (the field left out) defaults to 3, and a backoff limit that is zero or
// unset defaults to 30 seconds. Split out from doWithRetry so the defaulting
// itself is testable without waiting on a real timer.
func retryParams(cfg AgentModelConfig) (limit int, backoffLimit time.Duration) {
	limit = defaultRetryLimit
	if cfg.RetryLimit != nil {
		limit = *cfg.RetryLimit
	}
	backoffLimit = defaultExponentialBackoffLimit
	if cfg.ExponentialBackoffLimit > 0 {
		backoffLimit = time.Duration(cfg.ExponentialBackoffLimit) * time.Second
	}
	return limit, backoffLimit
}

// doWithRetry POSTs the completion request, retrying a transient failure with
// exponential backoff: 1s, 2s, 4s, ... doubling up to exponential_backoff_limit.
// retry_limit counts retries beyond the first attempt, so the default of 3
// means up to 4 requests in total. Detection (/model, /connect) is not
// retried here - it already tries every candidate route, and a hung endpoint
// there fails fast rather than blocking a console command.
func (c *llmClient) doWithRetry(ctx context.Context, body, out any) error {
	limit, backoffLimit := retryParams(c.cfg)
	return retryingRequest(ctx, limit, backoffLimit, func() error {
		return c.do(ctx, http.MethodPost, c.endpoint, body, out)
	})
}

// retryingRequest runs fn, retrying up to limit times on a retryable failure
// and waiting retryBackoff(attempt, backoffLimit) between tries. Kept apart
// from doWithRetry so a test can drive it with a backoff limit far shorter
// than the whole seconds a config file can express.
func retryingRequest(ctx context.Context, limit int, backoffLimit time.Duration, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= limit; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return lastErr
			case <-time.After(retryBackoff(attempt, backoffLimit)):
			}
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil || !isRetryableLLMError(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// retryBackoff doubles from 1 second on the first retry, capped at limit.
func retryBackoff(attempt int, limit time.Duration) time.Duration {
	// attempt is 1 on the first retry; a shift of 30+ would already overflow
	// well past any sane limit, so this is capped long before that matters.
	if attempt > 30 {
		return limit
	}
	if d := time.Second << (attempt - 1); d > 0 && d < limit {
		return d
	}
	return limit
}

// isRetryableLLMError reports whether a failed request is worth trying again:
// a network-level error (the request never got an HTTP response at all), a
// 429, or a 5xx. Anything else - a 4xx like a bad request or bad credentials -
// will fail exactly the same way a second time.
func isRetryableLLMError(err error) bool {
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		return statusErr.Status == http.StatusTooManyRequests || statusErr.Status >= 500
	}
	return true
}

// listModels asks the endpoint what it serves: the OpenAI (and Anthropic)
// /models list first, then Ollama's /api/tags.
func (c *llmClient) listModels(ctx context.Context) ([]string, error) {
	var targets []string
	if strings.HasSuffix(c.base, "/v1") {
		targets = []string{c.base + "/models"}
	} else {
		targets = []string{c.base + "/v1/models", c.base + "/models"}
	}
	var firstErr error
	for _, target := range targets {
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := c.do(ctx, http.MethodGet, target, nil, &list); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var ids []string
		for _, m := range list.Data {
			ids = append(ids, m.ID)
		}
		for _, m := range list.Models {
			ids = append(ids, m.Name)
		}
		if len(ids) > 0 {
			return ids, nil
		}
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := c.do(ctx, http.MethodGet, c.root()+"/api/tags", nil, &tags); err == nil && len(tags.Models) > 0 {
		var ids []string
		for _, m := range tags.Models {
			ids = append(ids, m.Name)
		}
		return ids, nil
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("the endpoint lists no models")
	}
	return nil, firstErr
}

// detect works out the endpoint style by sending a one-token request to each
// candidate. A 404 or 405 means the route is not there; a success, or a 400 or
// 422 complaining about the body, means it is. An authorization failure stops
// the search, since every other route will fail the same way.
func (c *llmClient) detect(ctx context.Context) error {
	if c.model == "" {
		ids, err := c.listModels(ctx)
		if err != nil {
			return fmt.Errorf("no model is configured for %s and the endpoint did not list any: %w", c.cfg.Name, err)
		}
		c.model = ids[0]
	}
	if c.style != "" {
		return nil
	}
	order := []string{styleOpenAI, styleAnthropic, styleResponses, styleOllama, styleCompletions}
	var tried []string
	for _, style := range order {
		for _, target := range c.candidates(style) {
			status, err := c.probe(ctx, style, target)
			tried = append(tried, fmt.Sprintf("%s -> %s", target, statusText(status, err)))
			if err != nil && status == 0 {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				continue
			}
			switch {
			case status >= 200 && status < 300, status == 400, status == 422:
				c.style, c.endpoint = style, target
				return nil
			case status == 401 || status == 403:
				return fmt.Errorf("%s refused the credentials (HTTP %d); check the auth section of model %s",
					target, status, c.cfg.Name)
			}
		}
	}
	return fmt.Errorf("could not detect the API style of %s; tried:\n  %s", c.base, strings.Join(tried, "\n  "))
}

func statusText(status int, err error) string {
	if status == 0 && err != nil {
		return err.Error()
	}
	return fmt.Sprintf("HTTP %d", status)
}

func (c *llmClient) probe(ctx context.Context, style, target string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	saved := c.style
	c.style = style
	body := c.buildBody(style, "", []chatMessage{{Role: "user", Content: "ping"}}, nil, 1)
	req, err := c.newRequest(ctx, http.MethodPost, target, body)
	c.style = saved
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// A 200 that is not JSON is a web page answering every path, not an API.
	if resp.StatusCode == 200 && !json.Valid(data) {
		return 404, nil
	}
	// A 400 from a server that routes everything to a catch-all mentions the
	// path rather than the body; treat "not found"-style 400s as absent.
	if resp.StatusCode == 400 && bytes.Contains(bytes.ToLower(data), []byte("not found")) {
		return 404, nil
	}
	return resp.StatusCode, nil
}

// complete sends the conversation and returns the model's reply.
func (c *llmClient) complete(ctx context.Context, system string, msgs []chatMessage, tools []toolSpec) (*llmReply, error) {
	return c.completeWithStream(ctx, system, msgs, tools, nil)
}

// completeWithStream is complete, additionally calling onDelta with each
// fragment of text or reasoning as it arrives, tagged "text" or "reasoning",
// for a style in streamingStyles. onDelta nil, or a style not in that set,
// runs the plain unary request complete always has - responses and
// completions are requested whole either way, streaming or not.
func (c *llmClient) completeWithStream(ctx context.Context, system string, msgs []chatMessage, tools []toolSpec, onDelta func(kind, delta string)) (*llmReply, error) {
	if c.style == "" || c.model == "" {
		if err := c.detect(ctx); err != nil {
			return nil, err
		}
	}
	maxTokens := c.cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	if onDelta != nil && streamingStyles[c.style] {
		return c.streamRequest(ctx, system, msgs, tools, maxTokens, onDelta)
	}
	body := c.buildBody(c.style, system, msgs, tools, maxTokens)
	var raw json.RawMessage
	if err := c.doWithRetry(ctx, body, &raw); err != nil {
		return nil, err
	}
	return parseFullReply(c.style, raw)
}

// parseFullReply is the non-streaming half of complete: one wire response,
// parsed whole, for whichever style sent it.
func parseFullReply(style string, raw json.RawMessage) (*llmReply, error) {
	switch style {
	case styleAnthropic:
		return parseAnthropic(raw)
	case styleResponses:
		return parseResponses(raw)
	case styleOllama:
		return parseOllama(raw)
	case styleCompletions:
		return parseCompletions(raw)
	default:
		return parseOpenAIChat(raw)
	}
}

// streamingStyles are the styles streamRequest knows how to read
// incrementally. A style missing here is simply run through the ordinary
// unary request even when streaming was asked for.
var streamingStyles = map[string]bool{styleOpenAI: true, styleAnthropic: true, styleOllama: true}

// streamRequest POSTs the same body complete would, with stream: true, and
// reads the response incrementally, calling onDelta as text or reasoning
// arrives. The connection attempt itself is retried exactly as doWithRetry
// would; once bytes start arriving they are not, since replaying a
// half-printed reply would show the caller a garbled repeat rather than a
// clean retry.
func (c *llmClient) streamRequest(ctx context.Context, system string, msgs []chatMessage, tools []toolSpec, maxTokens int, onDelta func(kind, delta string)) (*llmReply, error) {
	body := c.buildBody(c.style, system, msgs, tools, maxTokens)
	body["stream"] = true

	limit, backoffLimit := retryParams(c.cfg)
	var resp *http.Response
	err := retryingRequest(ctx, limit, backoffLimit, func() error {
		req, err := c.newRequest(ctx, http.MethodPost, c.endpoint, body)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "text/event-stream")
		r, err := c.http.Do(req)
		if err != nil {
			return err
		}
		if r.StatusCode < 200 || r.StatusCode > 299 {
			data, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
			r.Body.Close()
			return &httpStatusError{Status: r.StatusCode, Body: strings.TrimSpace(string(data))}
		}
		resp = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch c.style {
	case styleOllama:
		return readOllamaStream(resp.Body, onDelta)
	case styleAnthropic:
		return readAnthropicStream(resp.Body, onDelta)
	default:
		return readOpenAIStream(resp.Body, onDelta)
	}
}

// scanSSE reads a server-sent-events body and calls handle with the payload
// of each "data:" line, stopping cleanly on a "data: [DONE]" line or the end
// of the stream. Every other line (blank separators, "event:", "id:", a
// comment) is not this format's payload and is skipped rather than failing
// the stream over it.
func scanSSE(body io.Reader, handle func(data string) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return nil
		}
		if err := handle(data); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (c *llmClient) buildBody(style, system string, msgs []chatMessage, tools []toolSpec, maxTokens int) map[string]any {
	body := map[string]any{"model": c.model}
	if c.cfg.Temperature != nil {
		body["temperature"] = *c.cfg.Temperature
	}
	switch style {
	case styleAnthropic:
		body["max_tokens"] = maxTokens
		body["messages"] = anthropicMessages(msgs)
		if system != "" {
			body["system"] = system
		}
		if len(tools) > 0 {
			var list []any
			for _, t := range tools {
				list = append(list, map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Schema})
			}
			body["tools"] = list
		}
	case styleResponses:
		body["max_output_tokens"] = max(maxTokens, 16)
		body["store"] = false
		body["input"] = responsesInput(msgs)
		if system != "" {
			body["instructions"] = system
		}
		if len(tools) > 0 {
			var list []any
			for _, t := range tools {
				list = append(list, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Schema})
			}
			body["tools"] = list
		}
	case styleOllama:
		body["stream"] = false
		body["messages"] = ollamaMessages(system, msgs)
		body["options"] = map[string]any{"num_predict": maxTokens}
		// Ollama only puts a "thinking" field on the response when asked to;
		// a model that has none just ignores this rather than erroring, so
		// it is safe to always ask.
		body["think"] = true
		if len(tools) > 0 {
			body["tools"] = openAITools(tools)
		}
	case styleCompletions:
		body["max_tokens"] = maxTokens
		body["prompt"] = completionPrompt(system, msgs, tools)
		body["stop"] = []string{"\nUSER:", "\nTOOL RESULT"}
	default:
		body["max_tokens"] = maxTokens
		body["messages"] = openAIMessages(system, msgs)
		if len(tools) > 0 {
			body["tools"] = openAITools(tools)
		}
	}
	return body
}

// argsObject is a tool call's arguments as a JSON object, which is what every
// dialect except OpenAI's string form wants.
func argsObject(raw json.RawMessage) any {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return map[string]any{}
	}
	return obj
}

func argsString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// rawArguments accepts arguments given either as a JSON object or as a string
// holding one, which is how different servers disagree.
func rawArguments(v json.RawMessage) json.RawMessage {
	v = bytes.TrimSpace(v)
	if len(v) == 0 || string(v) == "null" {
		return json.RawMessage(`{}`)
	}
	if v[0] == '"' {
		var s string
		if json.Unmarshal(v, &s) == nil {
			s = strings.TrimSpace(s)
			if s == "" {
				return json.RawMessage(`{}`)
			}
			if json.Valid([]byte(s)) {
				return json.RawMessage(s)
			}
		}
		return json.RawMessage(`{}`)
	}
	return v
}

var callCounter int

func newCallID() string {
	callCounter++
	return fmt.Sprintf("call_%d_%d", time.Now().UnixNano()%1_000_000, callCounter)
}

// --- OpenAI chat completions ---

func openAITools(tools []toolSpec) []any {
	var list []any
	for _, t := range tools {
		list = append(list, map[string]any{
			"type":     "function",
			"function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Schema},
		})
	}
	return list
}

func openAIMessages(system string, msgs []chatMessage) []any {
	var out []any
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			out = append(out, map[string]any{"role": "tool", "tool_call_id": m.ToolCallID, "content": m.Content})
		case "assistant":
			msg := map[string]any{"role": "assistant", "content": m.Content}
			if len(m.ToolCalls) > 0 {
				var calls []any
				for _, tc := range m.ToolCalls {
					calls = append(calls, map[string]any{
						"id": tc.ID, "type": "function",
						"function": map[string]any{"name": tc.Name, "arguments": argsString(tc.Arguments)},
					})
				}
				msg["tool_calls"] = calls
			}
			out = append(out, msg)
		default:
			out = append(out, map[string]any{"role": "user", "content": m.Content})
		}
	}
	return out
}

func parseOpenAIChat(raw json.RawMessage) (*llmReply, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content          *string `json:"content"`
				ReasoningContent string  `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("the response has no choices: %s", truncateText(string(raw), 300))
	}
	msg := resp.Choices[0].Message
	reply := &llmReply{InTokens: resp.Usage.PromptTokens, OutTokens: resp.Usage.CompletionTokens, Reasoning: msg.ReasoningContent}
	if msg.Content != nil {
		reply.Text = *msg.Content
	}
	for _, tc := range msg.ToolCalls {
		reply.ToolCalls = append(reply.ToolCalls, toolCall{
			ID: firstNonEmpty(tc.ID, newCallID()), Name: tc.Function.Name, Arguments: rawArguments(tc.Function.Arguments),
		})
	}
	return reply, nil
}

// readOpenAIStream reads an OpenAI-style chat/completions SSE stream: each
// event is a chunk carrying one choice's delta. Streamed tool calls arrive
// piece by piece, keyed by index - a name and id up front, then the
// arguments' JSON one fragment at a time - so they are accumulated by index
// and only assembled into toolCalls once the stream ends.
//
// Not every OpenAI-compatible server sends usage while streaming (it needs
// stream_options.include_usage, which this deliberately does not set, since
// some self-hosted servers reject a field they do not recognize), so
// InTokens and OutTokens are commonly left at zero here - printUsage already
// omits its line when both are.
func readOpenAIStream(body io.Reader, onDelta func(kind, delta string)) (*llmReply, error) {
	reply := &llmReply{}
	var texts, thinking strings.Builder
	type pendingCall struct {
		id, name string
		args     strings.Builder
	}
	var calls []*pendingCall
	byIndex := map[int]*pendingCall{}

	err := scanSSE(body, func(data string) error {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			// A keepalive or a line this server sends that is not a chunk;
			// nothing here is worth failing the whole stream over.
			return nil
		}
		if chunk.Usage != nil {
			reply.InTokens, reply.OutTokens = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			return nil
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			texts.WriteString(delta.Content)
			onDelta("text", delta.Content)
		}
		if delta.ReasoningContent != "" {
			thinking.WriteString(delta.ReasoningContent)
			onDelta("reasoning", delta.ReasoningContent)
		}
		for _, tc := range delta.ToolCalls {
			p, ok := byIndex[tc.Index]
			if !ok {
				p = &pendingCall{}
				byIndex[tc.Index] = p
				calls = append(calls, p)
			}
			if tc.ID != "" {
				p.id = tc.ID
			}
			if tc.Function.Name != "" {
				p.name = tc.Function.Name
			}
			p.args.WriteString(tc.Function.Arguments)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	reply.Text = texts.String()
	reply.Reasoning = thinking.String()
	for _, p := range calls {
		reply.ToolCalls = append(reply.ToolCalls, toolCall{
			ID: firstNonEmpty(p.id, newCallID()), Name: p.name, Arguments: rawArguments(json.RawMessage(p.args.String())),
		})
	}
	return reply, nil
}

// --- Anthropic messages ---

func anthropicMessages(msgs []chatMessage) []any {
	var out []any
	var pendingResults []any
	flush := func() {
		if len(pendingResults) > 0 {
			out = append(out, map[string]any{"role": "user", "content": pendingResults})
			pendingResults = nil
		}
	}
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			pendingResults = append(pendingResults, map[string]any{
				"type": "tool_result", "tool_use_id": m.ToolCallID, "content": nonEmpty(m.Content),
			})
		case "assistant":
			flush()
			var blocks []any
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": argsObject(tc.Arguments)})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]any{"type": "text", "text": "(no content)"})
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		default:
			flush()
			out = append(out, map[string]any{"role": "user", "content": nonEmpty(m.Content)})
		}
	}
	flush()
	return out
}

func nonEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty)"
	}
	return s
}

func parseAnthropic(raw json.RawMessage) (*llmReply, error) {
	var resp struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	reply := &llmReply{InTokens: resp.Usage.InputTokens, OutTokens: resp.Usage.OutputTokens}
	var texts, thinking []string
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			texts = append(texts, block.Text)
		case "thinking":
			thinking = append(thinking, block.Thinking)
		case "redacted_thinking":
			thinking = append(thinking, "[redacted]")
		case "tool_use":
			reply.ToolCalls = append(reply.ToolCalls, toolCall{
				ID: firstNonEmpty(block.ID, newCallID()), Name: block.Name, Arguments: rawArguments(block.Input),
			})
		}
	}
	reply.Text = strings.Join(texts, "\n")
	reply.Reasoning = strings.Join(thinking, "\n")
	return reply, nil
}

// readAnthropicStream reads an Anthropic messages SSE stream. Each event's
// own "type" field says what it is - content_block_start names a new
// block's type and, for tool_use, its id and name; content_block_delta then
// carries that block's actual content by index (text_delta, thinking_delta
// or input_json_delta); message_start and message_delta carry usage. The
// "event:" line duplicates that type and is not otherwise needed, so
// scanSSE's plain "data:" scan is enough.
func readAnthropicStream(body io.Reader, onDelta func(kind, delta string)) (*llmReply, error) {
	reply := &llmReply{}
	var texts, thinking strings.Builder
	type pendingCall struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*pendingCall{}
	var order []int

	err := scanSSE(body, func(data string) error {
		var evt struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Message struct {
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(data), &evt) != nil {
			return nil
		}
		switch evt.Type {
		case "message_start":
			reply.InTokens = evt.Message.Usage.InputTokens
		case "content_block_start":
			if evt.ContentBlock.Type == "tool_use" {
				p := &pendingCall{id: evt.ContentBlock.ID, name: evt.ContentBlock.Name}
				calls[evt.Index] = p
				order = append(order, evt.Index)
			}
		case "content_block_delta":
			switch evt.Delta.Type {
			case "text_delta":
				texts.WriteString(evt.Delta.Text)
				onDelta("text", evt.Delta.Text)
			case "thinking_delta":
				thinking.WriteString(evt.Delta.Thinking)
				onDelta("reasoning", evt.Delta.Thinking)
			case "input_json_delta":
				if p, ok := calls[evt.Index]; ok {
					p.args.WriteString(evt.Delta.PartialJSON)
				}
			}
		case "message_delta":
			if evt.Usage.OutputTokens > 0 {
				reply.OutTokens = evt.Usage.OutputTokens
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	reply.Text = texts.String()
	reply.Reasoning = thinking.String()
	for _, idx := range order {
		p := calls[idx]
		reply.ToolCalls = append(reply.ToolCalls, toolCall{
			ID: firstNonEmpty(p.id, newCallID()), Name: p.name, Arguments: rawArguments(json.RawMessage(p.args.String())),
		})
	}
	return reply, nil
}

// --- OpenAI Responses ---

func responsesInput(msgs []chatMessage) []any {
	var out []any
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			out = append(out, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
		case "assistant":
			if strings.TrimSpace(m.Content) != "" {
				out = append(out, map[string]any{"role": "assistant", "content": m.Content})
			}
			for _, tc := range m.ToolCalls {
				out = append(out, map[string]any{
					"type": "function_call", "call_id": tc.ID, "name": tc.Name, "arguments": argsString(tc.Arguments),
				})
			}
		default:
			out = append(out, map[string]any{"role": "user", "content": m.Content})
		}
	}
	return out
}

func parseResponses(raw json.RawMessage) (*llmReply, error) {
	var resp struct {
		Output []struct {
			Type    string `json:"type"`
			CallID  string `json:"call_id"`
			Name    string `json:"name"`
			Args    string `json:"arguments"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}
	reply := &llmReply{InTokens: resp.Usage.InputTokens, OutTokens: resp.Usage.OutputTokens}
	var texts, thinking []string
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" || part.Type == "text" {
					texts = append(texts, part.Text)
				}
			}
		case "reasoning":
			for _, part := range item.Summary {
				if part.Text != "" {
					thinking = append(thinking, part.Text)
				}
			}
		case "function_call":
			encoded, _ := json.Marshal(item.Args)
			reply.ToolCalls = append(reply.ToolCalls, toolCall{
				ID: firstNonEmpty(item.CallID, newCallID()), Name: item.Name, Arguments: rawArguments(encoded),
			})
		}
	}
	reply.Text = strings.Join(texts, "\n")
	reply.Reasoning = strings.Join(thinking, "\n")
	return reply, nil
}

// --- Ollama native chat ---

func ollamaMessages(system string, msgs []chatMessage) []any {
	var out []any
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			out = append(out, map[string]any{"role": "tool", "content": m.Content, "tool_name": m.ToolName})
		case "assistant":
			msg := map[string]any{"role": "assistant", "content": m.Content}
			if len(m.ToolCalls) > 0 {
				var calls []any
				for _, tc := range m.ToolCalls {
					calls = append(calls, map[string]any{"function": map[string]any{"name": tc.Name, "arguments": argsObject(tc.Arguments)}})
				}
				msg["tool_calls"] = calls
			}
			out = append(out, msg)
		default:
			out = append(out, map[string]any{"role": "user", "content": m.Content})
		}
	}
	return out
}

func parseOllama(raw json.RawMessage) (*llmReply, error) {
	var resp struct {
		Message struct {
			Content   string `json:"content"`
			Thinking  string `json:"thinking"`
			ToolCalls []struct {
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	reply := &llmReply{Text: resp.Message.Content, Reasoning: resp.Message.Thinking, InTokens: resp.PromptEvalCount, OutTokens: resp.EvalCount}
	for _, tc := range resp.Message.ToolCalls {
		reply.ToolCalls = append(reply.ToolCalls, toolCall{ID: newCallID(), Name: tc.Function.Name, Arguments: rawArguments(tc.Function.Arguments)})
	}
	return reply, nil
}

// readOllamaStream reads Ollama's native streaming format: one complete JSON
// object per line (no "data:" framing), each carrying the message so far
// built up in content and thinking - accumulated here the same way an
// OpenAI-style delta is - until a final line with done: true, which carries
// the token counts instead. Ollama sends a tool call whole rather than in
// fragments, so those are just appended as they arrive.
func readOllamaStream(body io.Reader, onDelta func(kind, delta string)) (*llmReply, error) {
	reply := &llmReply{}
	var texts, thinking strings.Builder
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var chunk struct {
			Message struct {
				Content   string `json:"content"`
				Thinking  string `json:"thinking"`
				ToolCalls []struct {
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			Done            bool `json:"done"`
			PromptEvalCount int  `json:"prompt_eval_count"`
			EvalCount       int  `json:"eval_count"`
		}
		if json.Unmarshal([]byte(line), &chunk) != nil {
			continue
		}
		if chunk.Message.Content != "" {
			texts.WriteString(chunk.Message.Content)
			onDelta("text", chunk.Message.Content)
		}
		if chunk.Message.Thinking != "" {
			thinking.WriteString(chunk.Message.Thinking)
			onDelta("reasoning", chunk.Message.Thinking)
		}
		for _, tc := range chunk.Message.ToolCalls {
			reply.ToolCalls = append(reply.ToolCalls, toolCall{ID: newCallID(), Name: tc.Function.Name, Arguments: rawArguments(tc.Function.Arguments)})
		}
		if chunk.Done {
			reply.InTokens, reply.OutTokens = chunk.PromptEvalCount, chunk.EvalCount
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	reply.Text = texts.String()
	reply.Reasoning = thinking.String()
	return reply, nil
}

// --- Legacy text completions ---
//
// There is no tool calling in this API, so the tools are described in the
// prompt and the model is asked to answer with a fenced tool_call block.

func completionPrompt(system string, msgs []chatMessage, tools []toolSpec) string {
	var b strings.Builder
	if system != "" {
		b.WriteString("SYSTEM:\n" + system + "\n\n")
	}
	if len(tools) > 0 {
		b.WriteString("TOOLS:\nTo call a tool, reply with exactly one block and nothing after it:\n" +
			"```tool_call\n{\"name\": \"<tool name>\", \"arguments\": {...}}\n```\nAvailable tools:\n")
		for _, t := range tools {
			schema, _ := json.Marshal(t.Schema)
			fmt.Fprintf(&b, "- %s: %s\n  arguments schema: %s\n", t.Name, firstLine(t.Description), schema)
		}
		b.WriteString("\n")
	}
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			fmt.Fprintf(&b, "TOOL RESULT (%s):\n%s\n\n", m.ToolName, m.Content)
		case "assistant":
			b.WriteString("ASSISTANT:\n" + m.Content)
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "\n```tool_call\n{\"name\": %q, \"arguments\": %s}\n```", tc.Name, argsString(tc.Arguments))
			}
			b.WriteString("\n\n")
		default:
			b.WriteString("USER:\n" + m.Content + "\n\n")
		}
	}
	b.WriteString("ASSISTANT:\n")
	return b.String()
}

var toolCallBlock = regexp.MustCompile("(?s)```tool_call\\s*(\\{.*?\\})\\s*```")

func parseCompletions(raw json.RawMessage) (*llmReply, error) {
	var resp struct {
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("the response has no choices: %s", truncateText(string(raw), 300))
	}
	text := resp.Choices[0].Text
	reply := &llmReply{InTokens: resp.Usage.PromptTokens, OutTokens: resp.Usage.CompletionTokens}
	for _, match := range toolCallBlock.FindAllStringSubmatch(text, -1) {
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal([]byte(match[1]), &call) == nil && call.Name != "" {
			reply.ToolCalls = append(reply.ToolCalls, toolCall{ID: newCallID(), Name: call.Name, Arguments: rawArguments(call.Arguments)})
		}
	}
	reply.Text = strings.TrimSpace(toolCallBlock.ReplaceAllString(text, ""))
	return reply, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("... [%d more bytes]", len(s)-n)
}
