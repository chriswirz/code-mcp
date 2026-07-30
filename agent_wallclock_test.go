package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAgentConfigWallclockSecondsDefaultsToZero(t *testing.T) {
	if DefaultConfig().Agent.WallclockSeconds != 0 {
		t.Errorf("agent.wallclock_seconds should default to 0 (unlimited), got %d", DefaultConfig().Agent.WallclockSeconds)
	}
}

// TestContinueTurnStopsAtWallclockLimit: a model call that never returns
// must still be cut off once the turn's wall-clock limit is reached, with a
// message naming that as the reason rather than Ctrl-C's "interrupted".
func TestContinueTurnStopsAtWallclockLimit(t *testing.T) {
	srv, llm := newControllableLLM(t) // never released - the call blocks forever
	out := &syncBuffer{}
	a := newInterruptTestAgent(t, srv.URL+"/v1", out)
	a.wallclock = 60 * time.Millisecond

	err := a.runTurn("hello")
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if !strings.Contains(out.String(), "wall-clock limit") {
		t.Errorf("output = %q, want a message naming the wall-clock limit", out.String())
	}
	if strings.Contains(out.String(), "interrupted") {
		t.Errorf("output = %q, should not claim Ctrl-C interrupted it", out.String())
	}
	if len(a.history) != 1 || a.history[0].Role != "user" {
		t.Errorf("history = %+v, want just the unanswered prompt", a.history)
	}
	llm.recoverUnused()
}

// recoverUnused lets a controllableLLM's server shut down cleanly even
// though nothing ever released it - see newControllableLLM's t.Cleanup.
func (c *controllableLLM) recoverUnused() {
	select {
	case <-c.release:
	default:
		close(c.release)
	}
}

// slowToolCallLLM answers /v1/models normally and, for every completion
// request, sleeps for step before asking the agent to call the "noop" tool -
// forcing continueTurn through another round trip each time, so a test can
// check the wall-clock limit is enforced across several of them, not just
// within one call.
func slowToolCallLLM(t *testing.T, step time.Duration) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "m1"}}})
			return
		}
		mu.Lock()
		requests++
		mu.Unlock()
		time.Sleep(step)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{
				"content": "",
				"tool_calls": []any{map[string]any{
					"id": "c1", "type": "function",
					"function": map[string]any{"name": "noop", "arguments": "{}"},
				}},
			}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// TestContinueTurnWallclockAppliesAcrossRoundTrips: no single model call is
// slow enough on its own to trip the limit, but several of them together
// are - proving the limit bounds the whole turn, not just one request.
func TestContinueTurnWallclockAppliesAcrossRoundTrips(t *testing.T) {
	srv, requests := slowToolCallLLM(t, 30*time.Millisecond)
	out := &syncBuffer{}
	a := newInterruptTestAgent(t, srv.URL+"/v1", out)
	a.wallclock = 120 * time.Millisecond
	a.maxTurns = 1000 // high enough that max_turns is never what stops this
	a.toolIndex = map[string]*agentTool{
		"noop": {
			spec:     toolSpec{Name: "noop", Description: "does nothing"},
			readOnly: true,
			run: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
				return &CallToolResult{Content: textContent("ok")}, nil
			},
		},
	}
	for _, t := range a.toolIndex {
		a.tools = append(a.tools, t)
	}

	err := a.runTurn("hello")
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if !strings.Contains(out.String(), "wall-clock limit") {
		t.Errorf("output = %q, want a message naming the wall-clock limit", out.String())
	}
	if *requests < 2 {
		t.Errorf("requests = %d, want more than one round trip before the limit stopped it", *requests)
	}
	if *requests >= 1000 {
		t.Errorf("requests = %d, want it stopped well short of max_turns", *requests)
	}
}

// TestContinueTurnNoWallclockLimitByDefault: with wallclock left at zero, a
// turn with several round trips is not cut short by it.
func TestContinueTurnNoWallclockLimitByDefault(t *testing.T) {
	srv, requests := slowToolCallLLM(t, 5*time.Millisecond)
	out := &syncBuffer{}
	a := newInterruptTestAgent(t, srv.URL+"/v1", out)
	a.maxTurns = 3

	if err := a.runTurn("hello"); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if strings.Contains(out.String(), "wall-clock limit") {
		t.Errorf("output = %q, should not mention a wall-clock limit when it is unset", out.String())
	}
	if !strings.Contains(out.String(), "--max-turns") {
		t.Errorf("output = %q, want it to have stopped on max_turns instead", out.String())
	}
	if *requests != 3 {
		t.Errorf("requests = %d, want exactly max_turns (3)", *requests)
	}
}
