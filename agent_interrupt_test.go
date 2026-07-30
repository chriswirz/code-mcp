package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a.out for a test that needs to see what repl printed from a
// different goroutine than the one running it - a plain bytes.Buffer is not
// safe for that.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForOutput polls buf until it contains want, so a test can wait for
// repl to have actually reached the state it is about to act on instead of
// racing a fixed sleep against it.
func waitForOutput(t *testing.T, buf *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for output to contain %q; got %q", want, buf.String())
}

func newInterruptTestAgent(t *testing.T, baseURL string, out io.Writer) *agent {
	t.Helper()
	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: baseURL, Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	// A command tested here might write to the workspace (/savesession,
	// /save's family) - without this it would default to "" and land such
	// a write in the process's own working directory instead.
	cfg.Workspace.Root = t.TempDir()
	return &agent{
		cfg:      cfg,
		opts:     &agentOptions{},
		out:      out,
		allowed:  map[string]bool{},
		model:    client,
		models:   []*llmClient{client},
		maxTurns: 5,
		lines:    make(chan string),
		signals:  make(chan os.Signal, 1),
	}
}

// TestReplCtrlCAtEmptyPromptExitsImmediately is the rule this file is named
// for: nothing is running and nothing has been typed, so there is nothing
// left for Ctrl-C to clear - it quits on the first press.
func TestReplCtrlCAtEmptyPromptExitsImmediately(t *testing.T) {
	a := newInterruptTestAgent(t, "http://127.0.0.1:0", io.Discard)

	done := make(chan error, 1)
	go func() { done <- a.repl() }()

	a.signals <- os.Interrupt

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repl: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for repl to exit on Ctrl-C at an empty prompt")
	}
}

// TestReplCtrlCClearsTypedInputWithoutExiting covers the other half: a line
// still being continued with \ counts as something typed, so Ctrl-C clears it
// and repl keeps running rather than quitting out from under unsaved input.
// A second, genuinely empty Ctrl-C right after then confirms the clear really
// emptied it rather than leaving the continuation behind.
func TestReplCtrlCClearsTypedInputWithoutExiting(t *testing.T) {
	out := &syncBuffer{}
	a := newInterruptTestAgent(t, "http://127.0.0.1:0", out)
	pr, pw := io.Pipe()
	go a.readInput(pr)

	done := make(chan error, 1)
	go func() { done <- a.repl() }()

	// A continuation line: repl is now sitting on non-empty, unsubmitted
	// input, waiting for the rest of it. Wait for its own continuation
	// prompt to show up before sending Ctrl-C, so the signal cannot race the
	// line into repl's select and be seen first.
	if _, err := pw.Write([]byte("half a command\\\n")); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, out, ". ")

	// Ctrl-C must clear that, not quit.
	a.signals <- os.Interrupt
	select {
	case err := <-done:
		t.Fatalf("repl exited on Ctrl-C with typed input pending (err=%v), want it cleared instead", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Now the prompt is genuinely empty, so this one quits.
	a.signals <- os.Interrupt
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repl: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for repl to exit on Ctrl-C at the now-empty prompt")
	}
}

// TestReplCtrlCDuringTurnReturnsToPromptThenCtrlCExits: a Ctrl-C while a turn
// is running only stops that turn and returns to an empty prompt - it must
// not exit outright - but the prompt it returns to has nothing typed at it,
// so a second Ctrl-C there exits exactly as it would at any other empty
// prompt.
func TestReplCtrlCDuringTurnReturnsToPromptThenCtrlCExits(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "m1"}}})
			return
		}
		close(started)
		// The client aborts this request well before either case fires; the
		// short fallback just bounds how long srv.Close() waits for this
		// handler to return once abandoned, since a canceled client request
		// does not always make r.Context() done promptly on the server side.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	a := newInterruptTestAgent(t, srv.URL+"/v1", io.Discard)
	pr, pw := io.Pipe()
	go a.readInput(pr)

	done := make(chan error, 1)
	go func() { done <- a.repl() }()

	if _, err := pw.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the turn's request to reach the server")
	}

	// First Ctrl-C: stop the running turn. repl must not exit yet.
	a.signals <- os.Interrupt

	select {
	case err := <-done:
		t.Fatalf("repl exited after only one Ctrl-C (err=%v), want it to return to the prompt instead", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Second Ctrl-C, now that the turn has been stopped and repl is back at
	// the empty prompt: this one exits.
	a.signals <- os.Interrupt

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repl: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for repl to exit after the second Ctrl-C")
	}
}
