package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestNoteInterruptFirstCallIsNotSecond(t *testing.T) {
	a := &agent{}
	if a.noteInterrupt() {
		t.Error("the very first interrupt should not report as a second press")
	}
}

func TestNoteInterruptWithinWindowReportsSecond(t *testing.T) {
	a := &agent{}
	a.noteInterrupt()
	if !a.noteInterrupt() {
		t.Error("an interrupt right after the first one should report as the second press")
	}
}

func TestNoteInterruptOutsideWindowDoesNotReportSecond(t *testing.T) {
	a := &agent{lastInterrupt: time.Now().Add(-2 * quitWindow)}
	if a.noteInterrupt() {
		t.Error("an interrupt long after the last one should not report as a second press")
	}
}

// TestReplTwoQuickCtrlCAtIdlePromptExits documents the pre-existing
// double-Ctrl-C-to-quit behaviour at an idle prompt: nothing is running, so
// the first Ctrl-C only warns and the second one exits.
func TestReplTwoQuickCtrlCAtIdlePromptExits(t *testing.T) {
	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: "http://127.0.0.1:0", Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	a := &agent{
		cfg:     DefaultConfig(),
		opts:    &agentOptions{},
		out:     io.Discard,
		allowed: map[string]bool{},
		model:   client,
		models:  []*llmClient{client},
		lines:   make(chan string),
		signals: make(chan os.Signal, 1),
	}

	done := make(chan error, 1)
	go func() { done <- a.repl() }()

	a.signals <- os.Interrupt
	// Give repl a moment to process the first Ctrl-C and re-enter its
	// idle select before the second one arrives.
	time.Sleep(100 * time.Millisecond)
	a.signals <- os.Interrupt

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("repl: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for repl to exit after two Ctrl-C at the idle prompt")
	}
}

// TestReplCtrlCDuringTurnStopsProcessingThenSecondCtrlCAtPromptExits is the
// behaviour this test file exists for: a Ctrl-C while a turn is running must
// only stop that turn and return to the prompt, not exit outright - but a
// second Ctrl-C once back at the prompt should exit, the same as if both
// presses had happened while idle.
func TestReplCtrlCDuringTurnStopsProcessingThenSecondCtrlCAtPromptExits(t *testing.T) {
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

	// Second Ctrl-C, now that the turn has been stopped and repl is back
	// at the idle prompt: this one should exit.
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
