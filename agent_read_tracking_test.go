package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func writeNumberedFile(t *testing.T, root, name string, lines int) {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCall(id, args string) toolCall {
	return toolCall{ID: id, Name: "read_file", Arguments: json.RawMessage(args)}
}

// Reading a long file in chunks must keep every chunk: a later range does not
// cover an earlier one, so it supersedes nothing. This was the main cause of
// garbled writes - the model only ever saw the last chunk it read.
func TestChunkedReadsKeepEarlierChunks(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	writeNumberedFile(t, root, "big.txt", 100)

	first := runToolAsATurnWould(t, a, readCall("c1", `{"path":"big.txt","start_line":1,"end_line":50}`))
	runToolAsATurnWould(t, a, readCall("c2", `{"path":"big.txt","start_line":51,"end_line":100}`))

	if a.history[1].Content != first {
		t.Errorf("lines 1-50 were replaced after reading lines 51-100: %q", a.history[1].Content)
	}
}

func TestWholeReadSupersedesEarlierChunk(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	writeNumberedFile(t, root, "big.txt", 100)

	first := runToolAsATurnWould(t, a, readCall("c1", `{"path":"big.txt","start_line":10,"end_line":20}`))
	runToolAsATurnWould(t, a, readCall("c2", `{"path":"big.txt"}`))

	if a.history[1].Content == first || !isReadNudge(a.history[1].Content) {
		t.Errorf("a whole-file read should supersede an earlier chunk, got %q", a.history[1].Content)
	}
}

func TestChunkDoesNotSupersedeEarlierWholeRead(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	writeNumberedFile(t, root, "big.txt", 100)

	first := runToolAsATurnWould(t, a, readCall("c1", `{"path":"big.txt"}`))
	runToolAsATurnWould(t, a, readCall("c2", `{"path":"big.txt","start_line":10,"end_line":20}`))

	if a.history[1].Content != first {
		t.Errorf("a partial read must not replace a whole one: %q", a.history[1].Content)
	}
}

func TestReadCoversRanges(t *testing.T) {
	for _, tc := range []struct {
		newer, older readTarget
		want         bool
	}{
		{readTarget{}, readTarget{Start: 5, End: 9}, true},
		{readTarget{Start: 1, End: 50}, readTarget{Start: 10, End: 20}, true},
		{readTarget{Start: 1, End: 50}, readTarget{Start: 40, End: 60}, false},
		{readTarget{Start: 1, End: 50}, readTarget{}, false},
		{readTarget{Start: 51, End: 100}, readTarget{Start: 1, End: 50}, false},
		{readTarget{Start: 0, End: 0}, readTarget{Start: 1, End: 0}, true},
		{readTarget{Start: 20}, readTarget{Start: 30, End: 40}, true},
	} {
		if got := tc.newer.covers(tc.older); got != tc.want {
			t.Errorf("%+v covers %+v = %v, want %v", tc.newer, tc.older, got, tc.want)
		}
	}
}

// After a write, an earlier read's contents and line numbers are wrong, so it
// is replaced with a note naming the tool that changed the file.
func TestWritesInvalidateEarlierReads(t *testing.T) {
	for _, tc := range []struct {
		tool, args string
	}{
		{"write_file", `{"path":"f.txt","content":"new\n"}`},
		{"edit_file", `{"path":"f.txt","old_string":"line 2","new_string":"LINE 2"}`},
		{"edit_lines", `{"path":"f.txt","start_line":2,"new_text":"LINE 2"}`},
		{"insert_text", `{"path":"f.txt","line":1,"text":"zero"}`},
		{"multi_edit", `{"edits":[{"path":"./f.txt","old_string":"line 3","new_string":"LINE 3"}]}`},
		{"remove_sections", `{"path":"f.txt","sections":[{"old_string":"line 3\n"}]}`},
		{"apply_diff", "{\"diff\":\"--- a/f.txt\\n+++ b/f.txt\\n@@ -1,2 +1,2 @@\\n line 1\\n-line 2\\n+LINE 2\\n\"}"},
		{"replace_in_files", `{"pattern":"line 4","replacement":"LINE 4","literal":true}`},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			a, root := newStaleReadTestAgent(t)
			a.skipPerms = true
			writeNumberedFile(t, root, "f.txt", 5)
			first := runToolAsATurnWould(t, a, readCall("c1", `{"path":"f.txt","start_line":1,"end_line":3}`))
			result := runToolAsATurnWould(t, a, toolCall{ID: "c2", Name: tc.tool, Arguments: json.RawMessage(tc.args)})
			if strings.HasPrefix(result, "Error") || strings.HasPrefix(result, "There is no tool") {
				t.Fatalf("%s failed: %s", tc.tool, result)
			}
			got := a.history[1].Content
			if got == first || !strings.Contains(got, "changed by "+tc.tool) {
				t.Errorf("read after %s = %q, want a changed-file note", tc.tool, got)
			}
		})
	}
}

func TestFailedOrDryRunWriteKeepsEarlierRead(t *testing.T) {
	for name, args := range map[string]string{
		"no match": `{"edits":[{"path":"f.txt","old_string":"absent","new_string":"x"}]}`,
		"dry run":  `{"edits":[{"path":"f.txt","old_string":"line 2","new_string":"x"}],"dry_run":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			a, root := newStaleReadTestAgent(t)
			a.skipPerms = true
			writeNumberedFile(t, root, "f.txt", 5)
			first := runToolAsATurnWould(t, a, readCall("c1", `{"path":"f.txt"}`))
			runToolAsATurnWould(t, a, toolCall{ID: "c2", Name: "multi_edit", Arguments: json.RawMessage(args)})
			if a.history[1].Content != first {
				t.Errorf("the read was replaced although nothing was written: %q", a.history[1].Content)
			}
		})
	}
}

func TestWriteToOtherFileKeepsRead(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	a.skipPerms = true
	writeNumberedFile(t, root, "f.txt", 5)
	first := runToolAsATurnWould(t, a, readCall("c1", `{"path":"f.txt"}`))
	runToolAsATurnWould(t, a, toolCall{ID: "c2", Name: "write_file", Arguments: json.RawMessage(`{"path":"g.txt","content":"x"}`)})
	if a.history[1].Content != first {
		t.Errorf("writing g.txt replaced the read of f.txt: %q", a.history[1].Content)
	}
}

// A read_files result is replaced whole, so the note must name the files it
// also held, or the model loses them without knowing.
func TestReadFilesNudgeNamesOtherFiles(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	a.skipPerms = true
	writeNumberedFile(t, root, "f.txt", 3)
	writeNumberedFile(t, root, "g.txt", 3)
	runToolAsATurnWould(t, a, toolCall{ID: "c1", Name: "read_files", Arguments: json.RawMessage(`{"files":["f.txt","g.txt"]}`)})
	runToolAsATurnWould(t, a, toolCall{ID: "c2", Name: "write_file", Arguments: json.RawMessage(`{"path":"f.txt","content":"x"}`)})
	got := a.history[1].Content
	if !strings.Contains(got, "f.txt was changed") || !strings.Contains(got, "also held g.txt") {
		t.Errorf("note = %q, want it to name f.txt as changed and g.txt as also held", got)
	}
}

func TestDiffPaths(t *testing.T) {
	diff := "diff --git a/x/one.go b/x/one.go\n--- a/x/one.go\t2024-01-01\n+++ b/x/one.go\n@@ -1 +1 @@\n" +
		"--- /dev/null\n+++ b/two.go\n"
	got := diffPaths(diff, 1)
	want := []string{"x/one.go", "two.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("diffPaths = %v, want %v", got, want)
	}
}

func TestTruncateToolResultCutsOnLineAndWarnsReads(t *testing.T) {
	text := strings.Repeat("abcdefghi\n", 20)
	got, truncated := truncateToolResult("read_file", text, 55)
	if !truncated {
		t.Fatal("expected truncation")
	}
	body, note, _ := strings.Cut(got, "\n[TRUNCATED")
	if body != strings.Repeat("abcdefghi\n", 5) {
		t.Errorf("kept %q, want five whole lines", body)
	}
	if !strings.Contains(note, "do not rewrite the file with write_file") {
		t.Errorf("read note = %q", note)
	}

	// No usable newline: the cut must still not split a character.
	got, _ = truncateToolResult("other", strings.Repeat("é", 40), 31)
	body, _, _ = strings.Cut(got, "\n[...")
	if !utf8.ValidString(body) {
		t.Errorf("cut split a character: %q", body)
	}
}

// A truncated read did not return the whole file, so it must not replace an
// earlier read it claims to cover.
func TestTruncatedReadDoesNotSupersede(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	line := strings.Repeat("x", 99) + "\n"
	os.WriteFile(filepath.Join(root, "huge.txt"), []byte(strings.Repeat(line, 1000)), 0o644)
	first := runToolAsATurnWould(t, a, readCall("c1", `{"path":"huge.txt","start_line":900,"end_line":910}`))
	second := runToolAsATurnWould(t, a, readCall("c2", `{"path":"huge.txt"}`))
	if !strings.Contains(second, "[TRUNCATED") {
		t.Fatalf("expected the whole read to be truncated, it is %d bytes", len(second))
	}
	if a.history[1].Content != first {
		t.Errorf("a truncated read replaced an earlier chunk: %q", a.history[1].Content)
	}
}

func TestParsersReportOutputLimit(t *testing.T) {
	cases := map[string]func() (*llmReply, error){
		"openai": func() (*llmReply, error) {
			return parseOpenAIChat(json.RawMessage(`{"choices":[{"message":{"content":"x"},"finish_reason":"length"}]}`))
		},
		"anthropic": func() (*llmReply, error) {
			return parseAnthropic(json.RawMessage(`{"content":[{"type":"text","text":"x"}],"stop_reason":"max_tokens"}`))
		},
		"responses": func() (*llmReply, error) {
			return parseResponses(json.RawMessage(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`))
		},
		"ollama": func() (*llmReply, error) {
			return parseOllama(json.RawMessage(`{"message":{"content":"x"},"done":true,"done_reason":"length"}`))
		},
		"completions": func() (*llmReply, error) {
			return parseCompletions(json.RawMessage(`{"choices":[{"text":"x","finish_reason":"length"}]}`))
		},
		"openai stream": func() (*llmReply, error) {
			return readOpenAIStream(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"), func(string, string) {})
		},
		"anthropic stream": func() (*llmReply, error) {
			return readAnthropicStream(strings.NewReader(
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":5}}\n\n"), func(string, string) {})
		},
		"ollama stream": func() (*llmReply, error) {
			return readOllamaStream(strings.NewReader(`{"message":{"content":"x"},"done":true,"done_reason":"length"}`+"\n"), func(string, string) {})
		},
	}
	for name, parse := range cases {
		reply, err := parse()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !reply.Truncated {
			t.Errorf("%s: Truncated = false, want true", name)
		}
	}
	reply, _ := parseOpenAIChat(json.RawMessage(`{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}]}`))
	if reply.Truncated {
		t.Error("a normal stop was reported as truncated")
	}
}

// When the reply is cut off inside a tool call, that call is not run - even
// when its arguments happen to parse - and the model is told why.
func TestCutOffToolCallIsNotRun(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	a.skipPerms = true
	var requests atomic.Int32
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"m1"}]}`)
			return
		}
		if requests.Add(1) == 1 {
			io.WriteString(w, `{"choices":[{"finish_reason":"length","message":{"content":"","tool_calls":[`+
				`{"id":"w1","function":{"name":"write_file","arguments":"{\"path\":\"a.txt\",\"content\":\"half\"}"}}]}}]}`)
			return
		}
		io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(llm.Close)
	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: llm.URL + "/v1", Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	a.model, a.models, a.maxTurns, a.opts = client, []*llmClient{client}, 4, &agentOptions{}

	if err := a.runTurn("write a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Error("the cut-off write_file call was run")
	}
	var toolMsg string
	for _, m := range a.history {
		if m.Role == "tool" && m.ToolCallID == "w1" {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "output token limit") {
		t.Errorf("tool result = %q, want it to explain the output limit", toolMsg)
	}
}
