package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// controllableLLM answers /v1/models normally, but every completion request
// blocks until release is closed, so a test can hold a turn open for as long
// as it needs to exercise what happens while the model is "thinking". It
// records each request's last user message, in order, for a test to check
// what runTurn actually sent.
type controllableLLM struct {
	release chan struct{}
	started chan struct{}

	mu       sync.Mutex
	prompts  []string
	requests int
}

func newControllableLLM(t *testing.T) (*httptest.Server, *controllableLLM) {
	t.Helper()
	c := &controllableLLM{release: make(chan struct{}), started: make(chan struct{})}
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
		var last string
		for _, m := range body.Messages {
			if m.Role == "user" {
				last = m.Content
			}
		}
		c.mu.Lock()
		c.prompts = append(c.prompts, last)
		c.requests++
		first := c.requests == 1
		c.mu.Unlock()
		if first {
			close(c.started)
		}
		<-c.release
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func (c *controllableLLM) seenPrompts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.prompts...)
}

// TestHotCommandTogglesThinkingDuringTurn is the point of the feature: /thinking
// takes effect while the model is still generating, not only once Ctrl-C has
// stopped it.
func TestHotCommandTogglesThinkingDuringTurn(t *testing.T) {
	srv, llm := newControllableLLM(t)
	out := &syncBuffer{}
	a := newInterruptTestAgent(t, srv.URL+"/v1", out)
	pr, pw := io.Pipe()
	go a.readInput(pr)

	done := make(chan error, 1)
	go func() { done <- a.repl() }()

	if _, err := pw.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-llm.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the turn's request to reach the server")
	}

	if a.showThinking.Load() {
		t.Fatal("showThinking should start false")
	}
	if _, err := pw.Write([]byte("/thinking on\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, out, "thinking: shown")

	// The whole point: this took effect while the turn above is still
	// blocked in the handler, well before it has any answer to return.
	if !a.showThinking.Load() {
		t.Fatal("showThinking should already be true, before the turn finished")
	}

	close(llm.release)
	select {
	case err := <-done:
		t.Fatalf("repl exited unexpectedly: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	pw.Write([]byte("/exit\n"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repl: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for repl to exit")
	}
}

// TestHotCommandQueuesFreshPromptForAfterTurn: a prompt typed while a turn is
// running cannot be sent to the model right then - runTurn's own goroutine is
// using a.history - so it must not be lost either. It runs automatically once
// the first turn is done.
func TestHotCommandQueuesFreshPromptForAfterTurn(t *testing.T) {
	srv, llm := newControllableLLM(t)
	a := newInterruptTestAgent(t, srv.URL+"/v1", io.Discard)
	pr, pw := io.Pipe()
	go a.readInput(pr)

	done := make(chan error, 1)
	go func() { done <- a.repl() }()

	if _, err := pw.Write([]byte("first prompt\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-llm.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first request to reach the server")
	}

	if _, err := pw.Write([]byte("second prompt\n")); err != nil {
		t.Fatal(err)
	}
	// Give it a moment to (incorrectly) reach the server if it were not
	// queued, before releasing the first turn.
	time.Sleep(150 * time.Millisecond)
	if got := llm.seenPrompts(); len(got) != 1 {
		t.Fatalf("server saw %v while the first turn was still open, want only the first prompt", got)
	}

	close(llm.release)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(llm.seenPrompts()) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := llm.seenPrompts()
	if len(got) != 2 || got[0] != "first prompt" || got[1] != "second prompt" {
		t.Fatalf("prompts seen = %v, want [first prompt, second prompt]", got)
	}

	// The second turn's own request is now blocked in the handler; release
	// it too so repl can be told to exit cleanly.
	close2 := make(chan struct{})
	go func() {
		defer close(close2)
		pw.Write([]byte("/exit\n"))
	}()
	<-close2
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repl: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for repl to exit")
	}
}

// TestHotCommandQueuesUnsafeCommandForAfterTurn: /save touches a.history,
// which runTurn's own goroutine is using while a turn is open, so it cannot
// run right then either - it is deferred exactly like a fresh prompt would
// be, and runs once repl is next free to read a line at all.
func TestHotCommandQueuesUnsafeCommandForAfterTurn(t *testing.T) {
	srv, llm := newControllableLLM(t)
	out := &syncBuffer{}
	a := newInterruptTestAgent(t, srv.URL+"/v1", out)
	pr, pw := io.Pipe()
	go a.readInput(pr)

	done := make(chan error, 1)
	go func() { done <- a.repl() }()

	if _, err := pw.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-llm.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the turn's request to reach the server")
	}

	if _, err := pw.Write([]byte("/save\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if strings.Contains(out.String(), "saved") {
		t.Fatalf("/save ran while the turn was still open: %q", out.String())
	}

	close(llm.release)
	waitForOutput(t, out, "saved 2 messages")

	pw.Write([]byte("/exit\n"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repl: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for repl to exit")
	}
}

// TestHotCommandDuringTurnUnaffectedByHistoryRace exists mainly for the race
// detector: it drives /thinking, a queued prompt and a queued command all
// through the same turn, so `go test -race` has something to check besides
// the happy path.
func TestHotCommandDuringTurnUnaffectedByHistoryRace(t *testing.T) {
	srv, llm := newControllableLLM(t)
	a := newInterruptTestAgent(t, srv.URL+"/v1", io.Discard)
	pr, pw := io.Pipe()
	go a.readInput(pr)

	done := make(chan error, 1)
	go func() { done <- a.repl() }()

	pw.Write([]byte("first\n"))
	select {
	case <-llm.started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first request")
	}
	pw.Write([]byte("/thinking on\n"))
	pw.Write([]byte("/stats\n"))
	pw.Write([]byte("second\n"))
	close(llm.release)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(llm.seenPrompts()) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := llm.seenPrompts(); len(got) != 2 {
		t.Fatalf("prompts seen = %v, want 2", got)
	}

	go pw.Write([]byte("/exit\n"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repl: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for repl to exit")
	}
}
