package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// collectLines drains a.lines until it closes, or fails the test if that
// takes too long - readInput runs in its own goroutine, so a bug that stops
// it from ever closing the channel would otherwise hang the test suite.
func collectLines(t *testing.T, a *agent) []string {
	t.Helper()
	var got []string
	for {
		select {
		case line, ok := <-a.lines:
			if !ok {
				return got
			}
			got = append(got, line)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for readInput to finish")
		}
	}
}

func newPasteTestAgent() *agent {
	return &agent{lines: make(chan string)}
}

// The ordinary case, unaffected by any of this: no paste markers at all.
func TestReadInputPlainLinesUnaffected(t *testing.T) {
	a := newPasteTestAgent()
	go a.readInput(strings.NewReader("one\ntwo\nthree\n"))
	got := collectLines(t, a)
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The bug this fixes: a paste with no terminal support for bracketed paste
// (no markers at all) still arrives as separate lines - there is nothing
// more readInput itself can do about that case, since nothing distinguishes
// it from fast typing. This test documents that boundary rather than testing
// a fix for it.
func TestReadInputWithoutMarkersStaysLineByLine(t *testing.T) {
	a := newPasteTestAgent()
	go a.readInput(strings.NewReader("line one\nline two\nline three\n"))
	got := collectLines(t, a)
	if len(got) != 3 {
		t.Fatalf("got %v, want 3 separate lines", got)
	}
}

// A multi-line paste, wrapped the way a terminal in bracketed paste mode
// sends it, arrives as one line with the embedded newlines intact.
func TestReadInputCollectsBracketedPasteAsOneLine(t *testing.T) {
	a := newPasteTestAgent()
	input := "\x1b[200~line one\nline two\nline three\x1b[201~\n"
	go a.readInput(strings.NewReader(input))
	got := collectLines(t, a)
	if len(got) != 1 {
		t.Fatalf("got %d lines, want exactly 1: %q", len(got), got)
	}
	if want := "line one\nline two\nline three"; got[0] != want {
		t.Errorf("got %q, want %q", got[0], want)
	}
}

// A single-line paste still gets wrapped by the terminal even though it has
// no embedded newline; both markers land in the same read.
func TestReadInputCollectsSingleLineBracketedPaste(t *testing.T) {
	a := newPasteTestAgent()
	input := "\x1b[200~just one line\x1b[201~\n"
	go a.readInput(strings.NewReader(input))
	got := collectLines(t, a)
	if len(got) != 1 || got[0] != "just one line" {
		t.Fatalf("got %v, want [\"just one line\"]", got)
	}
}

// Text typed immediately before a paste, on the same line with no Enter in
// between, belongs at the front of the pasted block.
func TestReadInputPreservesTextTypedBeforePaste(t *testing.T) {
	a := newPasteTestAgent()
	input := "look at this: \x1b[200~pasted line one\nline two\x1b[201~\n"
	go a.readInput(strings.NewReader(input))
	got := collectLines(t, a)
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if want := "look at this: pasted line one\nline two"; got[0] != want {
		t.Errorf("got %q, want %q", got[0], want)
	}
}

// Text typed immediately after a paste ends, still on the same terminal row
// (no Enter yet), continues that last pasted line rather than starting a
// second one.
func TestReadInputPreservesTextTypedAfterPaste(t *testing.T) {
	a := newPasteTestAgent()
	input := "\x1b[200~line one\nline two\x1b[201~ please explain\n"
	go a.readInput(strings.NewReader(input))
	got := collectLines(t, a)
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if want := "line one\nline two please explain"; got[0] != want {
		t.Errorf("got %q, want %q", got[0], want)
	}
}

// A normal line, a paste, and another normal line, in one session: only the
// pasted block is collapsed into one entry.
func TestReadInputMixesPlainLinesAndPaste(t *testing.T) {
	a := newPasteTestAgent()
	input := "before\n\x1b[200~pasted one\npasted two\x1b[201~\nafter\n"
	go a.readInput(strings.NewReader(input))
	got := collectLines(t, a)
	want := []string{"before", "pasted one\npasted two", "after"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A paste that never sees its closing marker (the stream ends mid-paste)
// must not hang the reader or leave a stray line behind.
func TestReadInputUnterminatedPasteDoesNotHang(t *testing.T) {
	a := newPasteTestAgent()
	input := "\x1b[200~line one\nline two"
	go a.readInput(strings.NewReader(input))
	got := collectLines(t, a)
	if len(got) != 0 {
		t.Errorf("got %v, want nothing sent for an unterminated paste", got)
	}
}

// TestReplTreatsAPasteAsOneTurn drives the real pipeline end to end -
// readInput, the repl's collect loop, runTurn - and checks the model
// received the pasted lines as one prompt with the newlines intact, not
// three separate turns.
func TestReplTreatsAPasteAsOneTurn(t *testing.T) {
	var lastUserContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "m1"}}})
			return
		}
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &body)
		for _, m := range body.Messages {
			if m.Role == "user" {
				lastUserContent = m.Content
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
		})
	}))
	defer srv.Close()

	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: srv.URL + "/v1", Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	a := &agent{
		cfg:      DefaultConfig(),
		opts:     &agentOptions{},
		out:      io.Discard,
		allowed:  map[string]bool{},
		model:    client,
		models:   []*llmClient{client},
		maxTurns: 5,
		lines:    make(chan string),
		signals:  make(chan os.Signal, 1),
	}
	go a.readInput(pr)

	done := make(chan error, 1)
	go func() { done <- a.repl() }()

	pasted := "\x1b[200~func broken() {\n\treturn nil\n}\x1b[201~\n"
	if _, err := pw.Write([]byte(pasted)); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write([]byte("/exit\n")); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repl: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for repl to exit")
	}

	if want := "func broken() {\n\treturn nil\n}"; lastUserContent != want {
		t.Errorf("model received %q, want %q", lastUserContent, want)
	}
	if len(a.history) != 2 {
		t.Errorf("history = %+v, want exactly one user/assistant pair", a.history)
	}
}

// TestReplTreatsMultiLinePasteWithManyLinesAsOneTurn is TestReplTreatsAPasteAsOneTurn's companion: a longer, more
// realistic paste with blank lines, indentation and embedded backslashes,
// confirming the whole block still reaches the model as one turn with its
// structure intact.
func TestReplTreatsMultiLinePasteWithManyLinesAsOneTurn(t *testing.T) {
	var lastUserContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "m1"}}})
			return
		}
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &body)
		for _, m := range body.Messages {
			if m.Role == "user" {
				lastUserContent = m.Content
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
		})
	}))
	defer srv.Close()

	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: srv.URL + "/v1", Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	a := &agent{
		cfg:      DefaultConfig(),
		opts:     &agentOptions{},
		out:      io.Discard,
		allowed:  map[string]bool{},
		model:    client,
		models:   []*llmClient{client},
		maxTurns: 5,
		lines:    make(chan string),
		signals:  make(chan os.Signal, 1),
	}
	go a.readInput(pr)

	done := make(chan error, 1)
	go func() { done <- a.repl() }()

	pastedLines := "func example() {\n\n\tpath := `C:\\Users\\admin\\`\n\treturn path\n}"
	pasted := "\x1b[200~" + pastedLines + "\x1b[201~\n"
	if _, err := pw.Write([]byte(pasted)); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write([]byte("/exit\n")); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repl: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for repl to exit")
	}

	if lastUserContent != pastedLines {
		t.Errorf("model received %q, want %q", lastUserContent, pastedLines)
	}
	if len(a.history) != 2 {
		t.Errorf("history = %+v, want exactly one user/assistant pair", a.history)
	}
}
