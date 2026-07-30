package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseServer serves /v1/models normally and streams the given lines (already
// "data: ..." formatted, one per write) as the completion response, with a
// short pause between each so a reader has something to observe arriving
// incrementally rather than all at once.
func sseServer(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"m1"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, line := range lines {
			fmt.Fprintf(w, "%s\n\n", line)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLLMClientCompleteWithStreamCallsOnDeltaIncrementally(t *testing.T) {
	srv := sseServer(t, []string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: [DONE]`,
	})
	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: srv.URL + "/v1", Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}

	var deltas []string
	reply, err := client.completeWithStream(context.Background(), "sys", []chatMessage{{Role: "user", Content: "hi"}}, nil,
		func(kind, delta string) { deltas = append(deltas, kind+":"+delta) })
	if err != nil {
		t.Fatalf("completeWithStream: %v", err)
	}
	if reply.Text != "Hello" {
		t.Errorf("Text = %q", reply.Text)
	}
	want := []string{"text:Hel", "text:lo"}
	if len(deltas) != len(want) {
		t.Fatalf("deltas = %v, want %v", deltas, want)
	}
	for i := range want {
		if deltas[i] != want[i] {
			t.Errorf("delta %d = %q, want %q", i, deltas[i], want[i])
		}
	}
}

func TestLLMClientCompleteWithStreamNilOnDeltaBehavesLikeComplete(t *testing.T) {
	srv := sseServer(t, nil) // never reached; a nil onDelta must not stream at all
	srv.Close()
	nonStream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"m1"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer nonStream.Close()

	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: nonStream.URL + "/v1", Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := client.completeWithStream(context.Background(), "sys", []chatMessage{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("completeWithStream: %v", err)
	}
	if reply.Text != "ok" {
		t.Errorf("Text = %q, want the plain non-streaming response", reply.Text)
	}
}

// TestCommandStreamTogglesOnOffAndToggle exercises /stream the same way
// /thinking is tested.
func TestCommandStreamTogglesOnOffAndToggle(t *testing.T) {
	a, out := newConversationTestAgent(t, "irrelevant")

	a.command("/stream")
	if !a.streamOutput.Load() {
		t.Fatal("a bare /stream from off should turn it on")
	}
	if !strings.Contains(out.String(), "streaming: on") {
		t.Errorf("output = %q", out.String())
	}
	out.Reset()

	a.command("/stream off")
	if a.streamOutput.Load() {
		t.Fatal("/stream off should turn it off")
	}
	if !strings.Contains(out.String(), "streaming: off") {
		t.Errorf("output = %q", out.String())
	}
	out.Reset()

	a.command("/stream on")
	if !a.streamOutput.Load() {
		t.Fatal("/stream on should turn it on")
	}
	out.Reset()

	a.command("/stream sideways")
	if !strings.Contains(out.String(), "usage:") {
		t.Errorf("output = %q, want a usage message for a bad argument", out.String())
	}
	if !a.streamOutput.Load() {
		t.Error("a bad argument should not have changed the setting")
	}
}

func TestAgentConfigStreamDefaultsFalse(t *testing.T) {
	if DefaultConfig().Agent.Stream {
		t.Error("agent.stream should default to false")
	}
}

// TestRunTurnStreamsTextIncrementallyWhenEnabled drives a real turn through
// repl's own machinery with streaming on and checks the reply text shows up
// in the output before the stream has finished sending it - the point of
// the feature - rather than only checking the final aggregated result.
func TestRunTurnStreamsTextIncrementallyWhenEnabled(t *testing.T) {
	srv := sseServer(t, []string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: [DONE]`,
	})
	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: srv.URL + "/v1", Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Workspace.Root = t.TempDir()
	out := &syncBuffer{}
	a := &agent{cfg: cfg, opts: &agentOptions{}, out: out, allowed: map[string]bool{}, model: client, models: []*llmClient{client}, maxTurns: 5}
	a.streamOutput.Store(true)

	done := make(chan error, 1)
	go func() { done <- a.runTurn("hi") }()

	// The server paces its chunks 5ms apart; "Hel" must be visible well
	// before the second chunk, and therefore the full reply, has arrived.
	waitForOutput(t, out, "Hel")
	if strings.Contains(out.String(), "Hello") {
		t.Fatalf("output already has the full reply before the stream finished: %q", out.String())
	}

	if err := <-done; err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if !strings.Contains(out.String(), "Hello") {
		t.Errorf("output after the turn finished = %q, want the complete reply", out.String())
	}
}

// TestRunTurnStreamsReasoningOnlyWhenThinkingIsOn checks the interaction the
// feature request named specifically: reasoning is streamed live only while
// /thinking is on, and otherwise still ends up as the usual collapsed
// indicator once the turn is done.
func TestRunTurnStreamsReasoningOnlyWhenThinkingIsOn(t *testing.T) {
	newAgent := func(t *testing.T) (*agent, *syncBuffer) {
		srv := sseServer(t, []string{
			`data: {"choices":[{"delta":{"reasoning_content":"pondering"}}]}`,
			`data: {"choices":[{"delta":{"content":"done"}}]}`,
			`data: [DONE]`,
		})
		client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: srv.URL + "/v1", Model: "m1", Style: styleOpenAI})
		if err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		cfg.Workspace.Root = t.TempDir()
		out := &syncBuffer{}
		a := &agent{cfg: cfg, opts: &agentOptions{}, out: out, allowed: map[string]bool{}, model: client, models: []*llmClient{client}, maxTurns: 5}
		a.streamOutput.Store(true)
		return a, out
	}

	t.Run("thinking on", func(t *testing.T) {
		a, out := newAgent(t)
		a.showThinking.Store(true)
		if err := a.runTurn("hi"); err != nil {
			t.Fatalf("runTurn: %v", err)
		}
		if !strings.Contains(out.String(), "pondering") {
			t.Errorf("output = %q, want the reasoning streamed live", out.String())
		}
	})

	t.Run("thinking off", func(t *testing.T) {
		a, out := newAgent(t)
		if err := a.runTurn("hi"); err != nil {
			t.Fatalf("runTurn: %v", err)
		}
		if strings.Contains(out.String(), "pondering") {
			t.Errorf("output = %q, want the reasoning collapsed, not streamed", out.String())
		}
		if !strings.Contains(out.String(), "thinking,") {
			t.Errorf("output = %q, want the usual collapsed indicator", out.String())
		}
	})
}
