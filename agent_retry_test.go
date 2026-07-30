package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRetryParamsDefaults(t *testing.T) {
	limit, backoff := retryParams(AgentModelConfig{})
	if limit != 3 {
		t.Errorf("default retry limit = %d, want 3", limit)
	}
	if backoff != 30*time.Second {
		t.Errorf("default backoff limit = %v, want 30s", backoff)
	}
}

func TestRetryParamsExplicitZeroDisablesRetries(t *testing.T) {
	zero := 0
	limit, _ := retryParams(AgentModelConfig{RetryLimit: &zero})
	if limit != 0 {
		t.Errorf("explicit retry_limit: 0 should stay 0, got %d", limit)
	}
}

func TestRetryParamsHonorsExplicitValues(t *testing.T) {
	five := 5
	limit, backoff := retryParams(AgentModelConfig{RetryLimit: &five, ExponentialBackoffLimit: 10})
	if limit != 5 {
		t.Errorf("limit = %d, want 5", limit)
	}
	if backoff != 10*time.Second {
		t.Errorf("backoff = %v, want 10s", backoff)
	}
}

func TestRetryBackoffDoublesAndCaps(t *testing.T) {
	limit := 10 * time.Second
	cases := map[int]time.Duration{
		1: time.Second,
		2: 2 * time.Second,
		3: 4 * time.Second,
		4: 8 * time.Second,
		5: limit, // 16s would exceed the 10s cap
		6: limit,
	}
	for attempt, want := range cases {
		if got := retryBackoff(attempt, limit); got != want {
			t.Errorf("retryBackoff(%d, %v) = %v, want %v", attempt, limit, got, want)
		}
	}
}

func TestIsRetryableLLMError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"network error", errors.New("dial tcp: connection refused"), true},
		{"429", &httpStatusError{Status: http.StatusTooManyRequests}, true},
		{"500", &httpStatusError{Status: 500}, true},
		{"503", &httpStatusError{Status: 503}, true},
		{"400 bad request", &httpStatusError{Status: 400}, false},
		{"401 unauthorized", &httpStatusError{Status: 401}, false},
		{"404 not found", &httpStatusError{Status: 404}, false},
		{"422 unprocessable", &httpStatusError{Status: 422}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableLLMError(tc.err); got != tc.want {
				t.Errorf("isRetryableLLMError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// retryingRequest is exercised directly with a near-zero backoff limit so the
// test runs fast regardless of what a real config (whole seconds only) could
// express.
func TestRetryingRequestSucceedsAfterTransientFailures(t *testing.T) {
	attempts := 0
	err := retryingRequest(context.Background(), 3, time.Millisecond, func() error {
		attempts++
		if attempts < 3 {
			return &httpStatusError{Status: 503}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (2 failures then a success)", attempts)
	}
}

func TestRetryingRequestStopsOnNonRetryableError(t *testing.T) {
	attempts := 0
	err := retryingRequest(context.Background(), 3, time.Millisecond, func() error {
		attempts++
		return &httpStatusError{Status: 400}
	})
	if err == nil {
		t.Fatal("expected the 400 to surface as an error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a 4xx should not be retried)", attempts)
	}
}

func TestRetryingRequestStopsAtLimit(t *testing.T) {
	attempts := 0
	err := retryingRequest(context.Background(), 2, time.Millisecond, func() error {
		attempts++
		return &httpStatusError{Status: 503}
	})
	if err == nil {
		t.Fatal("expected an error once the limit is exhausted")
	}
	if attempts != 3 { // the first attempt plus 2 retries
		t.Errorf("attempts = %d, want 3 (1 + retry_limit of 2)", attempts)
	}
}

func TestRetryingRequestZeroLimitMakesOneAttempt(t *testing.T) {
	attempts := 0
	err := retryingRequest(context.Background(), 0, time.Millisecond, func() error {
		attempts++
		return &httpStatusError{Status: 503}
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (retry_limit: 0 disables retries)", attempts)
	}
}

func TestRetryingRequestStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := retryingRequest(ctx, 5, time.Hour, func() error {
		attempts++
		cancel() // cancel during the "request" so the loop must not sleep an hour
		return &httpStatusError{Status: 503}
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (cancellation should stop further retries)", attempts)
	}
}

// TestModelRetriesOnServerError exercises the real wiring - newLLMClient,
// complete, doWithRetry - against a server that fails twice before answering,
// with retry_limit and exponential_backoff_limit set low enough that the test
// still completes quickly.
func TestModelRetriesOnServerError(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
		})
	}))
	defer srv.Close()

	two := 2
	client, err := newLLMClient(AgentModelConfig{
		Name: "t", BaseURL: srv.URL + "/v1", Model: "m1", Style: styleOpenAI,
		RetryLimit: &two, ExponentialBackoffLimit: 1, // 1s cap keeps this well under a couple of seconds total
	})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := client.complete(context.Background(), "sys", []chatMessage{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if reply.Text != "ok" {
		t.Errorf("Text = %q, want ok", reply.Text)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (2 failures then a success)", attempts)
	}
}

// A 4xx must not be retried even when retry_limit allows it - the model
// endpoint would fail the same way every time.
func TestModelDoesNotRetryOnBadRequest(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	client, err := newLLMClient(AgentModelConfig{Name: "t", BaseURL: srv.URL + "/v1", Model: "m1", Style: styleOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.complete(context.Background(), "sys", []chatMessage{{Role: "user", Content: "hi"}}, nil); err == nil {
		t.Fatal("expected an error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a 400 should not be retried)", attempts)
	}
}

func TestAgentConfigValidateRejectsNegativeRetrySettings(t *testing.T) {
	neg := -1
	cfg := AgentConfig{Models: []AgentModelConfig{{Name: "m", BaseURL: "http://x", RetryLimit: &neg}}}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected an error for a negative retry_limit")
	}

	cfg = AgentConfig{Models: []AgentModelConfig{{Name: "m", BaseURL: "http://x", ExponentialBackoffLimit: -1}}}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected an error for a negative exponential_backoff_limit")
	}

	cfg = AgentConfig{Models: []AgentModelConfig{{Name: "m", BaseURL: "http://x", ContextWindow: -1}}}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected an error for a negative context_window")
	}
}

// Ollama only includes a "thinking" field on the response when the request
// asks for it - without this, /thinking has nothing to show against a real
// Ollama endpoint even though the model supports it.
func TestOllamaRequestBodyAsksForThinking(t *testing.T) {
	c := &llmClient{cfg: AgentModelConfig{Name: "t"}, model: "m1"}
	body := c.buildBody(styleOllama, "sys", []chatMessage{{Role: "user", Content: "hi"}}, nil, 100)
	think, ok := body["think"].(bool)
	if !ok || !think {
		t.Errorf(`body["think"] = %v, want true`, body["think"])
	}
}
