package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSanitizeText(t *testing.T) {
	for in, want := range map[string]string{
		"a\u2014b":                            "a-b",
		"a \u2013 b":                          "a - b",
		"x \u2192 y \u2190 z":                 "x -> y <- z",
		"a \u2194 b \u21D2 c":                 "a <-> b => c",
		"\u201Chi\u201D \u2018x\u2019":        `"hi" 'x'`,
		"wait\u2026":                          "wait...",
		"a\u00A0b\u200Bc":                     "a bc",
		"x \u2264 y \u2265 z \u2260 w":        "x <= y >= z != w",
		"plain ascii -> stays":                "plain ascii -> stays",
		"caf\u00E9 \u00FC\u00DF \u4E16\u754C": "caf\u00E9 \u00FC\u00DF \u4E16\u754C",
	} {
		if got := sanitizeText(in); got != want {
			t.Errorf("sanitizeText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeArgsCleansWrittenTextOnly(t *testing.T) {
	raw := json.RawMessage(`{"path":"a\u2014b.md","old_string":"x \u2014 y","new_string":"x \u2014 y \u2192 z",` +
		`"edits":[{"old_string":"\u2014","new_string":"\u2014"}],"content":"\u201Cq\u201D"}`)
	var got map[string]any
	if err := json.Unmarshal(sanitizeArgs(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got["path"] != "a\u2014b.md" || got["old_string"] != "x \u2014 y" {
		t.Errorf("path/old_string must be left alone: %v", got)
	}
	if got["new_string"] != "x - y -> z" || got["content"] != `"q"` {
		t.Errorf("new_string/content not cleaned: %v", got)
	}
	edit := got["edits"].([]any)[0].(map[string]any)
	if edit["old_string"] != "\u2014" || edit["new_string"] != "-" {
		t.Errorf("nested edit = %v, want only new_string cleaned", edit)
	}

	untouched := json.RawMessage(`{"path":"x","content":"plain"}`)
	if string(sanitizeArgs(untouched)) != string(untouched) {
		t.Error("arguments with nothing to clean should be returned byte for byte")
	}
}

func TestSanitizeDiffOnlyAddedLines(t *testing.T) {
	diff := "--- a/f\u2014.md\n+++ b/f\u2014.md\n@@ -1,2 +1,2 @@\n ctx \u2014\n-old \u2014\n+new \u2014 \u2192\n"
	want := "--- a/f\u2014.md\n+++ b/f\u2014.md\n@@ -1,2 +1,2 @@\n ctx \u2014\n-old \u2014\n+new - ->\n"
	if got := sanitizeDiff(diff); got != want {
		t.Errorf("sanitizeDiff = %q, want %q", got, want)
	}
}

func TestSetSanitizeToggles(t *testing.T) {
	a := &agent{out: io.Discard}
	a.setSanitize(nil)
	if !a.sanitize.Load() {
		t.Error("no argument should toggle it on")
	}
	a.setSanitize([]string{"off"})
	if a.sanitize.Load() {
		t.Error("/sanitize off left it on")
	}
	a.setSanitize([]string{"bogus"})
	if a.sanitize.Load() {
		t.Error("a bad argument changed the setting")
	}
}

// With /sanitize on, a streamed reply and the file its write_file call
// writes both come out in plain ASCII, and the history holds the cleaned
// version the model will see next.
func TestSanitizeAppliesToReplyAndWrites(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	a.skipPerms = true
	a.sanitize.Store(true)
	a.streamOutput.Store(true)
	var out strings.Builder
	a.out = &out

	var requests atomic.Int32
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"m1"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Writing \u2014 now \u2192\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"w1\",\"function\":{\"name\":\"write_file\","+
				"\"arguments\":\"{\\\"path\\\":\\\"a.md\\\",\\\"content\\\":\\\"one \u2014 two\\\\n\\\"}\"}}]}}]}\n\n"+
				"data: [DONE]\n\n")
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(llm.Close)
	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: llm.URL + "/v1", Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	a.model, a.models, a.maxTurns, a.opts = client, []*llmClient{client}, 4, &agentOptions{}

	if err := a.runTurn("go"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one - two\n" {
		t.Errorf("file = %q, want the em dash replaced", data)
	}
	if !strings.Contains(out.String(), "Writing - now ->") || strings.ContainsAny(out.String(), "\u2014\u2192") {
		t.Errorf("printed output not sanitized: %q", out.String())
	}
	if a.history[1].Content != "Writing - now ->" {
		t.Errorf("history holds %q, want the sanitized text", a.history[1].Content)
	}
}
