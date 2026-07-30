package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeLLM serves one API style at one path and answers every request with a
// tool call first and text once a tool result is in the conversation.
func fakeLLM(t *testing.T, path, style string, wantAuth string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" && style != styleOllama {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "m1"}}})
			return
		}
		if r.URL.Path != path || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		got := r.Header.Get("Authorization")
		if style == styleAnthropic {
			got = r.Header.Get("x-api-key")
		}
		if got != wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		hasResult := strings.Contains(string(body), "TOOL-OUTPUT")
		var resp any
		switch style {
		case styleOpenAI:
			msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"id": "c1", "type": "function", "function": map[string]any{"name": "echo", "arguments": `{"text":"hi"}`}}}}
			if hasResult {
				msg = map[string]any{"role": "assistant", "content": "done"}
			}
			resp = map[string]any{"choices": []any{map[string]any{"message": msg}}}
		case styleAnthropic:
			content := []any{map[string]any{"type": "tool_use", "id": "c1", "name": "echo", "input": map[string]any{"text": "hi"}}}
			if hasResult {
				content = []any{map[string]any{"type": "text", "text": "done"}}
			}
			resp = map[string]any{"content": content}
		case styleResponses:
			out := []any{map[string]any{"type": "function_call", "call_id": "c1", "name": "echo", "arguments": `{"text":"hi"}`}}
			if hasResult {
				out = []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}}
			}
			resp = map[string]any{"output": out}
		case styleOllama:
			msg := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}}}}}
			if hasResult {
				msg = map[string]any{"role": "assistant", "content": "done"}
			}
			resp = map[string]any{"message": msg}
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestLLMStylesAutoDetect(t *testing.T) {
	cases := []struct {
		style, path, suffix string
	}{
		{styleOpenAI, "/v1/chat/completions", "/v1"},
		{styleOpenAI, "/v1/chat/completions", ""},
		{styleAnthropic, "/anthropic/v1/messages", "/v1"},
		{styleAnthropic, "/v1/messages", ""},
		{styleResponses, "/v1/responses", "/v1"},
		{styleOllama, "/api/chat", ""},
	}
	for _, tc := range cases {
		t.Run(tc.style+tc.path, func(t *testing.T) {
			srv := fakeLLM(t, tc.path, tc.style, map[bool]string{true: "secret", false: "Bearer secret"}[tc.style == styleAnthropic])
			defer srv.Close()
			client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: srv.URL + tc.suffix, Model: "m1", Auth: AgentAuth{Token: "secret"}})
			if err != nil {
				t.Fatal(err)
			}
			tools := []toolSpec{{Name: "echo", Schema: map[string]any{"type": "object"}}}
			msgs := []chatMessage{{Role: "user", Content: "go"}}
			reply, err := client.complete(context.Background(), "sys", msgs, tools)
			if err != nil {
				t.Fatal(err)
			}
			if client.style != tc.style {
				t.Fatalf("detected %q, want %q", client.style, tc.style)
			}
			if len(reply.ToolCalls) != 1 || reply.ToolCalls[0].Name != "echo" || !strings.Contains(string(reply.ToolCalls[0].Arguments), "hi") {
				t.Fatalf("tool calls: %+v", reply.ToolCalls)
			}
			msgs = append(msgs, chatMessage{Role: "assistant", ToolCalls: reply.ToolCalls},
				chatMessage{Role: "tool", ToolCallID: reply.ToolCalls[0].ID, ToolName: "echo", Content: "TOOL-OUTPUT"})
			reply, err = client.complete(context.Background(), "sys", msgs, tools)
			if err != nil || reply.Text != "done" {
				t.Fatalf("second reply %+v, %v", reply, err)
			}
		})
	}
}

func TestLLMDetectReportsBadCredentials(t *testing.T) {
	srv := fakeLLM(t, "/v1/chat/completions", styleOpenAI, "Bearer right")
	defer srv.Close()
	client, _ := newLLMClient(AgentModelConfig{Name: "t", BaseURL: srv.URL + "/v1", Model: "m1", Auth: AgentAuth{Token: "wrong"}})
	err := client.detect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("want a credentials error, got %v", err)
	}
}

func TestCompletionsToolCallParsing(t *testing.T) {
	raw := json.RawMessage(`{"choices":[{"text":"Let me look.\n` + "```tool_call\\n{\\\"name\\\": \\\"read_file\\\", \\\"arguments\\\": {\\\"path\\\": \\\"a.go\\\"}}\\n```" + `"}]}`)
	reply, err := parseCompletions(raw)
	if err != nil || len(reply.ToolCalls) != 1 || reply.ToolCalls[0].Name != "read_file" || reply.Text != "Let me look." {
		t.Fatalf("%+v %v", reply, err)
	}
}

func TestNormalizeEndpointURL(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.200:11434/v1":            "http://10.0.0.200:11434/v1",
		"localhost/v1":                   "http://localhost/v1",
		"llmapi.chriswirz.com/v1":        "https://llmapi.chriswirz.com/v1",
		"https://llmapi.example.com/v1/": "https://llmapi.example.com/v1",
	} {
		if got, err := normalizeEndpointURL(in); err != nil || got != want {
			t.Errorf("%s: got %q (%v), want %q", in, got, err, want)
		}
	}
}

func testToolServer() *Server {
	srv := NewServer("test", "1", "", true)
	srv.logger = log.New(io.Discard, "", 0)
	srv.RegisterTool(Tool{
		Name:        "echo",
		InputSchema: schema([]string{"text"}, map[string]any{"text": prop("string", "text")}),
		Annotations: &ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Text string `json:"text"`
		}
		decodeArgs(raw, &args)
		return toolResult("echo: " + args.Text), nil
	})
	return srv
}

func TestMCPClientAgainstOwnServerWithAuth(t *testing.T) {
	srv := testToolServer()
	cfg := DefaultConfig()
	cfg.Server.AuthToken = "tok"
	transport := NewHTTPTransport(srv, cfg.Server, "/mcp", log.New(io.Discard, "", 0))
	ts := httptest.NewServer(transport.Handler())
	defer ts.Close()

	bad, _ := newMCPClient(AgentMCPConfig{Name: "x", URL: ts.URL + "/mcp", Auth: AgentAuth{Token: "nope"}})
	if err := bad.connect(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401, got %v", err)
	}

	client, _ := newMCPClient(AgentMCPConfig{Name: "x", URL: ts.URL + "/mcp", Auth: AgentAuth{Type: "bearer", Token: "tok"}})
	if err := client.connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if len(client.tools) != 1 || client.tools[0].Name != "echo" {
		t.Fatalf("tools: %+v", client.tools)
	}
	res, err := client.callTool(context.Background(), "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil || resultText(res) != "echo: hi" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestOpenAPIClientAgainstOwnServer(t *testing.T) {
	srv := testToolServer()
	cfg := DefaultConfig()
	cfg.Server.AuthToken = "tok"
	api := newOpenAPIServer(srv, cfg)
	mux := http.NewServeMux()
	api.register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := newOpenAPIClient(AgentOpenAPIConfig{Name: "own", SpecURL: ts.URL + "/api/openapi.json", Auth: AgentAuth{Token: "tok"}})
	if err := client.connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.ops) != 1 || client.ops[0].name != "echo" {
		t.Fatalf("ops: %+v", client.ops)
	}
	res, err := client.invoke(context.Background(), client.ops[0], json.RawMessage(`{"body":{"text":"hi"}}`))
	if err != nil || res.IsError || !strings.Contains(resultText(res), "echo: hi") {
		t.Fatalf("%+v %v", res, err)
	}

	noAuth := newOpenAPIClient(AgentOpenAPIConfig{Name: "own", SpecURL: ts.URL + "/api/openapi.json"})
	if err := noAuth.connect(context.Background()); err == nil {
		t.Fatal("want an authorization error without the token")
	}
}

func TestToolNameSanitized(t *testing.T) {
	if got := toolName("my server", "get/thing"); got != "my_server__get_thing" {
		t.Fatal(got)
	}
	if got := toolName("x", strings.Repeat("a", 100)); len(got) != 64 {
		t.Fatal(len(got))
	}
}
