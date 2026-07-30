package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSummarizerLLM always answers with the same fixed text, regardless of
// what is asked - enough to test what the agent does with a summary, not the
// summarization itself.
func fakeSummarizerLLM(t *testing.T, text string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "m1"}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": text}}},
		})
	}))
}

func newConversationTestAgent(t *testing.T, summary string) (*agent, *bytes.Buffer) {
	t.Helper()
	srv := fakeSummarizerLLM(t, summary)
	t.Cleanup(srv.Close)
	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: srv.URL + "/v1", Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Workspace.Root = t.TempDir()
	var buf bytes.Buffer
	a := &agent{
		cfg:     cfg,
		opts:    &agentOptions{},
		out:     &buf,
		allowed: map[string]bool{},
		model:   client,
		models:  []*llmClient{client},
	}
	return a, &buf
}

func TestSummarizeConversationErrorsOnEmptyHistory(t *testing.T) {
	a, _ := newConversationTestAgent(t, "irrelevant")
	if _, err := a.summarizeConversation(t.Context()); err == nil {
		t.Fatal("expected an error when there is nothing to summarize")
	}
}

func TestSummarizeConversationReturnsModelText(t *testing.T) {
	a, _ := newConversationTestAgent(t, "  the user asked X, we did Y  ")
	a.history = []chatMessage{{Role: "user", Content: "do X"}, {Role: "assistant", Content: "did Y"}}
	got, err := a.summarizeConversation(t.Context())
	if err != nil {
		t.Fatalf("summarizeConversation: %v", err)
	}
	if got != "the user asked X, we did Y" {
		t.Errorf("summary = %q, want it trimmed", got)
	}
}

func TestCommandSummaryDoesNotChangeHistory(t *testing.T) {
	a, out := newConversationTestAgent(t, "a summary of what happened")
	a.history = []chatMessage{{Role: "user", Content: "hello"}}

	if quit := a.command("/summary"); quit {
		t.Fatal("/summary should not quit")
	}
	if !strings.Contains(out.String(), "a summary of what happened") {
		t.Errorf("output = %q, want the summary", out.String())
	}
	if len(a.history) != 1 {
		t.Errorf("history changed: %+v", a.history)
	}
}

func TestCommandCompressReplacesHistoryWithSummary(t *testing.T) {
	a, out := newConversationTestAgent(t, "a summary of what happened")
	a.history = []chatMessage{
		{Role: "user", Content: "do X"},
		{Role: "assistant", Content: "did Y"},
		{Role: "user", Content: "do Z"},
	}

	a.command("/compress")
	if !strings.Contains(out.String(), "compressed 3 messages") {
		t.Errorf("output = %q, want a report of what was compressed", out.String())
	}
	if len(a.history) != 1 {
		t.Fatalf("history after compress = %+v, want exactly one message", a.history)
	}
	if a.history[0].Role != "assistant" {
		t.Errorf("compressed history role = %q, want assistant", a.history[0].Role)
	}
	if !strings.Contains(a.history[0].Content, "a summary of what happened") {
		t.Errorf("compressed history content = %q, want the summary", a.history[0].Content)
	}
}

func TestCommandCompressOnEmptyHistoryReportsError(t *testing.T) {
	a, out := newConversationTestAgent(t, "irrelevant")
	a.command("/compress")
	if !strings.Contains(out.String(), "error:") {
		t.Errorf("output = %q, want an error for nothing to compress", out.String())
	}
}

func TestCommandSaveWritesJSONLWithDefaultName(t *testing.T) {
	a, out := newConversationTestAgent(t, "irrelevant")
	a.history = []chatMessage{
		{Role: "user", Content: "do X"},
		{Role: "assistant", Content: "", ToolCalls: []toolCall{{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)}}},
		{Role: "tool", ToolCallID: "c1", ToolName: "echo", Content: "hi"},
	}

	a.command("/save")
	msg := out.String()
	if !strings.Contains(msg, "saved 3 messages") {
		t.Fatalf("output = %q", msg)
	}

	entries, err := os.ReadDir(a.cfg.Workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	var jsonlFiles []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			jsonlFiles = append(jsonlFiles, e.Name())
		}
	}
	if len(jsonlFiles) != 1 {
		t.Fatalf("expected exactly one .jsonl file, got %v", jsonlFiles)
	}
	if !strings.HasPrefix(jsonlFiles[0], "codemcp-conversation-") {
		t.Errorf("file name = %q, want the codemcp-conversation- prefix", jsonlFiles[0])
	}

	data, err := os.ReadFile(filepath.Join(a.cfg.Workspace.Root, jsonlFiles[0]))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 JSONL lines, got %d: %q", len(lines), data)
	}
	var first transcriptLine
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 is not valid JSON: %v", err)
	}
	if first.Role != "user" || first.Content != "do X" {
		t.Errorf("line 1 = %+v", first)
	}
	var second transcriptLine
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 2 is not valid JSON: %v", err)
	}
	if len(second.ToolCalls) != 1 || second.ToolCalls[0].Name != "echo" {
		t.Errorf("line 2 tool calls = %+v", second.ToolCalls)
	}
	var third transcriptLine
	if err := json.Unmarshal([]byte(lines[2]), &third); err != nil {
		t.Fatalf("line 3 is not valid JSON: %v", err)
	}
	if third.Role != "tool" || third.ToolCallID != "c1" || third.ToolName != "echo" {
		t.Errorf("line 3 = %+v", third)
	}
}

func TestCommandSaveWithRelativePathUnderWorkspace(t *testing.T) {
	a, _ := newConversationTestAgent(t, "irrelevant")
	a.history = []chatMessage{{Role: "user", Content: "hi"}}

	a.command("/save transcript.jsonl")
	want := filepath.Join(a.cfg.Workspace.Root, "transcript.jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected the file at %s: %v", want, err)
	}
}

func TestCommandSaveWithAbsolutePath(t *testing.T) {
	a, _ := newConversationTestAgent(t, "irrelevant")
	a.history = []chatMessage{{Role: "user", Content: "hi"}}

	elsewhere := filepath.Join(t.TempDir(), "out.jsonl")
	a.command("/save " + elsewhere)
	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatalf("expected the file at %s: %v", elsewhere, err)
	}
}

func TestCommandSaveOnEmptyHistoryReportsError(t *testing.T) {
	a, out := newConversationTestAgent(t, "irrelevant")
	a.command("/save")
	if !strings.Contains(out.String(), "error:") {
		t.Errorf("output = %q, want an error for nothing to save", out.String())
	}
}

func TestCommandSaveSummaryWritesTextFile(t *testing.T) {
	a, out := newConversationTestAgent(t, "the final summary")
	a.history = []chatMessage{{Role: "user", Content: "hi"}}

	a.command("/savesummary")
	if !strings.Contains(out.String(), "saved summary to") {
		t.Fatalf("output = %q", out.String())
	}

	entries, err := os.ReadDir(a.cfg.Workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "codemcp-summary-") {
			found = e.Name()
		}
	}
	if found == "" {
		t.Fatal("no codemcp-summary- file was written")
	}
	data, err := os.ReadFile(filepath.Join(a.cfg.Workspace.Root, found))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "the final summary" {
		t.Errorf("file content = %q", data)
	}
	// /savesummary must not have replaced the conversation the way /compress does.
	if len(a.history) != 1 {
		t.Errorf("history changed: %+v", a.history)
	}
}

func TestFindLatestSummaryReturnsNewestByName(t *testing.T) {
	a, _ := newConversationTestAgent(t, "irrelevant")
	root := a.cfg.Workspace.Root

	for _, name := range []string{"codemcp-summary-20260101-120000.md", "codemcp-summary-20260914-153000.md", "codemcp-summary-20260601-000000.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A file that isn't a default-named summary must not be picked up.
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := a.findLatestSummary()
	if !ok {
		t.Fatal("expected a summary to be found")
	}
	if filepath.Base(got) != "codemcp-summary-20260914-153000.md" {
		t.Errorf("found = %q, want the most recent timestamp", got)
	}
}

func TestFindLatestSummaryNoneFound(t *testing.T) {
	a, _ := newConversationTestAgent(t, "irrelevant")
	if _, ok := a.findLatestSummary(); ok {
		t.Fatal("expected no summary to be found in an empty workspace")
	}
}

func TestCommandLoadSummaryDefaultsToLatest(t *testing.T) {
	a, out := newConversationTestAgent(t, "irrelevant")
	root := a.cfg.Workspace.Root
	os.WriteFile(filepath.Join(root, "codemcp-summary-20260101-120000.md"), []byte("older summary"), 0o644)
	os.WriteFile(filepath.Join(root, "codemcp-summary-20260914-153000.md"), []byte("the latest summary"), 0o644)

	a.command("/loadsummary")
	if !strings.Contains(out.String(), "loaded summary from") {
		t.Fatalf("output = %q", out.String())
	}
	if len(a.history) != 1 {
		t.Fatalf("history = %+v, want exactly the loaded summary", a.history)
	}
	if a.history[0].Role != "assistant" || !strings.Contains(a.history[0].Content, "the latest summary") {
		t.Errorf("history[0] = %+v, want the latest summary's content", a.history[0])
	}
}

func TestCommandLoadSummaryPrependsToExistingHistory(t *testing.T) {
	a, _ := newConversationTestAgent(t, "irrelevant")
	root := a.cfg.Workspace.Root
	os.WriteFile(filepath.Join(root, "codemcp-summary-20260101-120000.md"), []byte("prior context"), 0o644)
	a.history = []chatMessage{{Role: "user", Content: "still here"}}

	a.command("/loadsummary")
	if len(a.history) != 2 {
		t.Fatalf("history = %+v, want the summary prepended, not replacing", a.history)
	}
	if !strings.Contains(a.history[0].Content, "prior context") {
		t.Errorf("history[0] = %+v", a.history[0])
	}
	if a.history[1].Content != "still here" {
		t.Errorf("history[1] = %+v, want the original message preserved", a.history[1])
	}
}

func TestCommandLoadSummaryWithExplicitPath(t *testing.T) {
	a, out := newConversationTestAgent(t, "irrelevant")
	custom := filepath.Join(a.cfg.Workspace.Root, "my-notes.md")
	os.WriteFile(custom, []byte("custom summary"), 0o644)

	a.command("/loadsummary my-notes.md")
	if !strings.Contains(out.String(), "loaded summary from") {
		t.Fatalf("output = %q", out.String())
	}
	if len(a.history) != 1 || !strings.Contains(a.history[0].Content, "custom summary") {
		t.Errorf("history = %+v", a.history)
	}
}

func TestCommandLoadSummaryNoneFoundReportsError(t *testing.T) {
	a, out := newConversationTestAgent(t, "irrelevant")
	a.command("/loadsummary")
	if !strings.Contains(out.String(), "error:") {
		t.Errorf("output = %q, want an error when nothing is saved", out.String())
	}
	if len(a.history) != 0 {
		t.Errorf("history = %+v, want unchanged", a.history)
	}
}

func TestPrintBannerMentionsSavedSummary(t *testing.T) {
	a, out := newConversationTestAgent(t, "irrelevant")
	os.WriteFile(filepath.Join(a.cfg.Workspace.Root, "codemcp-summary-20260914-153000.md"), []byte("x"), 0o644)

	a.printBanner()
	if !strings.Contains(out.String(), "/loadsummary to resume") {
		t.Errorf("banner = %q, want a hint about the saved summary", out.String())
	}
}

func TestPrintBannerSilentWithoutSavedSummary(t *testing.T) {
	a, out := newConversationTestAgent(t, "irrelevant")
	a.printBanner()
	if strings.Contains(out.String(), "loadsummary") {
		t.Errorf("banner = %q, want no summary hint", out.String())
	}
}

func TestResolveSavePath(t *testing.T) {
	a, _ := newConversationTestAgent(t, "irrelevant")

	if got := a.resolveSavePath("relative.jsonl", "prefix", "jsonl"); got != filepath.Join(a.cfg.Workspace.Root, "relative.jsonl") {
		t.Errorf("relative path = %q", got)
	}

	abs := filepath.Join(t.TempDir(), "abs.jsonl")
	if got := a.resolveSavePath(abs, "prefix", "jsonl"); got != abs {
		t.Errorf("absolute path = %q, want unchanged %q", got, abs)
	}

	def := a.resolveSavePath("", "codemcp-conversation", "jsonl")
	if !strings.HasPrefix(filepath.Base(def), "codemcp-conversation-") || !strings.HasSuffix(def, ".jsonl") {
		t.Errorf("default path = %q", def)
	}
	if filepath.Dir(def) != a.cfg.Workspace.Root {
		t.Errorf("default path should live under the workspace root: %q", def)
	}
}
