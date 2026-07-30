package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCommandRetryReportsErrorWithNothingToRetry(t *testing.T) {
	a, out := newConversationTestAgent(t, "irrelevant")
	a.command("/retry")
	if !strings.Contains(out.String(), "error:") {
		t.Errorf("output = %q, want an error when there is nothing to retry", out.String())
	}
}

// flakyLLM fails every completion request until told to stop, and always
// reports whatever models list it is currently given - the retry limit is
// forced to zero so a failing call returns immediately rather than going
// through the real exponential backoff.
type flakyLLM struct {
	mu     sync.Mutex
	fail   bool
	models []string
}

func newFlakyLLM(t *testing.T, initialModels []string) (*httptest.Server, *flakyLLM) {
	t.Helper()
	f := &flakyLLM{fail: true, models: initialModels}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			f.mu.Lock()
			ids := append([]string(nil), f.models...)
			f.mu.Unlock()
			data := make([]any, len(ids))
			for i, id := range ids {
				data[i] = map[string]any{"id": id}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
			return
		}
		f.mu.Lock()
		fail := f.fail
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

func (f *flakyLLM) recover(models []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = false
	if models != nil {
		f.models = models
	}
}

func newRetryTestAgent(t *testing.T, client *llmClient) (*agent, *strings.Builder) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Workspace.Root = t.TempDir()
	var out strings.Builder
	return &agent{
		cfg:      cfg,
		opts:     &agentOptions{},
		out:      &out,
		allowed:  map[string]bool{},
		model:    client,
		models:   []*llmClient{client},
		maxTurns: 5,
	}, &out
}

// TestCommandRetryContinuesAfterFailedTurn is /retry's main job: a turn that
// never got a reply is continued, not asked again - the history must not
// end up with the same prompt twice.
func TestCommandRetryContinuesAfterFailedTurn(t *testing.T) {
	srv, flaky := newFlakyLLM(t, []string{"m1"})
	zero := 0
	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: srv.URL + "/v1", Model: "m1", Style: styleOpenAI, RetryLimit: &zero})
	if err != nil {
		t.Fatal(err)
	}
	a, out := newRetryTestAgent(t, client)

	if err := a.runTurn("hello"); err == nil {
		t.Fatal("expected the first turn to fail while the service is down")
	}
	if len(a.history) != 1 || a.history[0].Content != "hello" {
		t.Fatalf("history after the failed turn = %+v, want just the unanswered prompt", a.history)
	}

	flaky.recover(nil)
	a.command("/retry")

	if !strings.Contains(out.String(), "retrying with") {
		t.Errorf("output = %q, want a line announcing the retry", out.String())
	}
	if len(a.history) != 2 {
		t.Fatalf("history = %+v, want the prompt continued, not repeated", a.history)
	}
	if a.history[0].Content != "hello" || a.history[1].Role != "assistant" {
		t.Errorf("history = %+v", a.history)
	}
}

// TestCommandRetryResendsAfterASuccessfulTurn covers the other case: nothing
// failed, the conversation just moved on, so /retry means "ask that again"
// and a fresh user message is appended.
func TestCommandRetryResendsAfterASuccessfulTurn(t *testing.T) {
	srv, flaky := newFlakyLLM(t, []string{"m1"})
	flaky.recover(nil)
	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: srv.URL + "/v1", Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := newRetryTestAgent(t, client)

	if err := a.runTurn("hello"); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if len(a.history) != 2 {
		t.Fatalf("history after a successful turn = %+v", a.history)
	}

	a.command("/retry")
	if len(a.history) != 4 {
		t.Fatalf("history = %+v, want a second user/assistant pair appended", a.history)
	}
	if a.history[2].Role != "user" || a.history[2].Content != "hello" {
		t.Errorf("history[2] = %+v, want the same prompt asked again", a.history[2])
	}
}

// TestCommandRetryUsesTheCurrentModelAfterASwitch: /retry must not hang on to
// whatever model was current when the failed turn ran - a /model switch in
// between has to be honored.
func TestCommandRetryUsesTheCurrentModelAfterASwitch(t *testing.T) {
	badSrv, _ := newFlakyLLM(t, []string{"m1"}) // stays failing throughout
	zero := 0
	bad, err := newLLMClient(AgentModelConfig{Name: "bad", BaseURL: badSrv.URL + "/v1", Model: "m1", Style: styleOpenAI, RetryLimit: &zero})
	if err != nil {
		t.Fatal(err)
	}
	goodSrv, goodFlaky := newFlakyLLM(t, []string{"m1"})
	goodFlaky.recover(nil)
	good, err := newLLMClient(AgentModelConfig{Name: "good", BaseURL: goodSrv.URL + "/v1", Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}

	a, _ := newRetryTestAgent(t, bad)
	a.models = []*llmClient{bad, good}

	if err := a.runTurn("hello"); err == nil {
		t.Fatal("expected the first turn to fail against the bad endpoint")
	}

	a.command("/model good")
	a.command("/retry")

	if a.model != good {
		t.Fatalf("a.model = %v, want the switched-to model", a.model.cfg.Name)
	}
	if len(a.history) != 2 || a.history[1].Role != "assistant" {
		t.Fatalf("history = %+v, want the retried turn to have succeeded against the new model", a.history)
	}
}

// TestCommandRetryResetsAutoDetectedModelAfterServiceChange: a model left to
// auto-detect (no model id configured) that fails, then comes back serving a
// different model than before, must be re-detected on /retry rather than
// reusing the stale id from the first detection.
func TestCommandRetryResetsAutoDetectedModelAfterServiceChange(t *testing.T) {
	srv, flaky := newFlakyLLM(t, []string{"model-a"})
	zero := 0
	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: srv.URL + "/v1", Style: "auto", RetryLimit: &zero})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := newRetryTestAgent(t, client)

	if err := a.runTurn("hello"); err == nil {
		t.Fatal("expected the first turn to fail")
	}
	if client.model != "model-a" {
		t.Fatalf("client.model = %q, want model-a detected from the first attempt", client.model)
	}

	// The service "restarts" onto a different model.
	flaky.recover([]string{"model-b"})
	a.command("/retry")

	if client.model != "model-b" {
		t.Errorf("client.model = %q, want model-b re-detected after /retry", client.model)
	}
	if len(a.history) != 2 {
		t.Errorf("history = %+v, want the prompt continued once, not repeated", a.history)
	}
}
