package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAgentFlagsAcceptsFlagsAroundThePrompt(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		config string
		model  string
		prompt string
	}{
		{
			name:   "flags first",
			args:   []string{"--config", "other.json", "-m", "gateway", "fix the build"},
			config: "other.json",
			model:  "gateway",
			prompt: "fix the build",
		},
		{
			name:   "flags after the bare prompt",
			args:   []string{"fix the build", "--config", "other.json", "-m", "gateway"},
			config: "other.json",
			model:  "gateway",
			prompt: "fix the build",
		},
		{
			name:   "flags either side of it",
			args:   []string{"--config=other.json", "fix the build", "--model", "gateway"},
			config: "other.json",
			model:  "gateway",
			prompt: "fix the build",
		},
		{
			name:   "a bool flag does not swallow the next word",
			args:   []string{"--no-local-tools", "fix", "the", "build", "-c", "other.json"},
			config: "other.json",
			prompt: "fix the build",
		},
		{
			name:   "everything after -- is prompt",
			args:   []string{"-c", "other.json", "--", "explain", "--config"},
			config: "other.json",
			prompt: "explain --config",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseAgentFlags(tc.args)
			if err != nil {
				t.Fatalf("parseAgentFlags(%q): %v", tc.args, err)
			}
			if o.configPath != tc.config {
				t.Errorf("config = %q, want %q", o.configPath, tc.config)
			}
			if o.model != tc.model {
				t.Errorf("model = %q, want %q", o.model, tc.model)
			}
			if o.prompt != tc.prompt {
				t.Errorf("prompt = %q, want %q", o.prompt, tc.prompt)
			}
		})
	}
}

func TestParseAgentFlagsRejectsTwoPrompts(t *testing.T) {
	if _, err := parseAgentFlags([]string{"-p", "one", "two"}); err == nil {
		t.Fatal("expected an error when the prompt is given twice")
	}
}

// The secrets and headers a connection sends are all configurable inline, so
// a config file needs no environment at all.
func TestAgentAuthInlineAndCustomHeaders(t *testing.T) {
	cases := []struct {
		name    string
		auth    AgentAuth
		headers map[string]string
		want    map[string]string
	}{
		{
			name: "plain token defaults to bearer",
			auth: AgentAuth{Token: "sk-plain"},
			want: map[string]string{"Authorization": "Bearer sk-plain"},
		},
		{
			name: "plain token in a named header",
			auth: AgentAuth{Type: "header", Name: "X-API-Key", Token: "sk-plain"},
			want: map[string]string{"X-API-Key": "sk-plain", "Authorization": ""},
		},
		{
			name: "inline basic credentials",
			auth: AgentAuth{Type: "basic", Username: "me", Password: "pw"},
			want: map[string]string{"Authorization": "Basic bWU6cHc="},
		},
		{
			name:    "custom headers alongside the token",
			auth:    AgentAuth{Token: "sk-plain"},
			headers: map[string]string{"X-Tenant": "acme", "X-Trace": "on"},
			want: map[string]string{
				"Authorization": "Bearer sk-plain",
				"X-Tenant":      "acme",
				"X-Trace":       "on",
			},
		},
		{
			name: "env references still expand inline",
			auth: AgentAuth{Type: "header", Name: "X-Api-Key", Token: "${CODEMCP_TEST_KEY}"},
			want: map[string]string{"X-Api-Key": "from-env"},
		},
	}
	t.Setenv("CODEMCP_TEST_KEY", "from-env")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://example.com/mcp", nil)
			tc.auth.apply(req, "")
			applyHeaders(req, tc.headers)
			for k, want := range tc.want {
				if got := req.Header.Get(k); got != want {
					t.Errorf("header %s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestAgentAuthTokenEnvFallsBackToInline(t *testing.T) {
	// token_env wins when the variable is set, and an inline token covers the
	// machine where it is not.
	auth := AgentAuth{Token: "inline", TokenEnv: "CODEMCP_TEST_MISSING"}
	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	auth.apply(req, "")
	if got := req.Header.Get("Authorization"); got != "Bearer inline" {
		t.Errorf("Authorization = %q, want the inline token", got)
	}

	t.Setenv("CODEMCP_TEST_MISSING", "from-env")
	req = httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	auth.apply(req, "")
	if got := req.Header.Get("Authorization"); got != "Bearer from-env" {
		t.Errorf("Authorization = %q, want the environment token", got)
	}
}

// A config file is commonly shared between `codemcp` (the server) and
// `codemcp agent`. The agent forces stdio transport since it reaches its
// tools in process, so anything that requires the HTTP handler - the tunnel,
// downloads - must be forced off too, or a config with e.g. tunnel.enabled
// true fails Normalize before the agent ever starts.
func TestPrepareAgentConfigDisablesTunnelAndDownloads(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.Transport = "http"
	cfg.Downloads.Enabled = true
	cfg.Tunnel.Enabled = true
	cfg.Tunnel.ServerURL = "https://tunnel.example.com"
	cfg.Tunnel.APIKey = "x"

	if err := prepareAgentConfig(&cfg, t.TempDir()); err != nil {
		t.Fatalf("prepareAgentConfig: %v", err)
	}
	if cfg.Server.Transport != "stdio" {
		t.Errorf("Transport = %q, want stdio", cfg.Server.Transport)
	}
	if cfg.Downloads.Enabled {
		t.Error("Downloads.Enabled should be forced off")
	}
	if cfg.Tunnel.Enabled {
		t.Error("Tunnel.Enabled should be forced off")
	}
}

// newTestAgent builds an agent the way runAgent does, minus the local tool
// server (which needs a database) - reloadConfig exercises the rest:
// models, connections and the config file itself.
func newTestAgent(t *testing.T, configPath string) *agent {
	t.Helper()
	cfg, err := LoadConfig(configPath, true)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := prepareAgentConfig(&cfg, t.TempDir()); err != nil {
		t.Fatalf("prepareAgentConfig: %v", err)
	}
	a := &agent{
		cfg:        cfg,
		opts:       &agentOptions{},
		out:        io.Discard,
		allowed:    map[string]bool{},
		configPath: configPath,
		explicit:   true,
		workspace:  t.TempDir(),
	}
	if err := a.setupModels(); err != nil {
		t.Fatalf("setupModels: %v", err)
	}
	a.setupConnections()
	return a
}

func writeTestConfig(t *testing.T, path, modelName string) {
	t.Helper()
	body := fmt.Sprintf(`{"agent":{"models":[{"name":%q,"base_url":"http://127.0.0.1:1/v1"}]}}`, modelName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestReloadConfigPicksUpChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeTestConfig(t, path, "local")
	a := newTestAgent(t, path)

	writeTestConfig(t, path, "renamed")
	if err := a.reloadConfig(); err != nil {
		t.Fatalf("reloadConfig: %v", err)
	}
	if len(a.models) != 1 || a.models[0].cfg.Name != "renamed" {
		t.Fatalf("models after reload = %+v", a.models)
	}
	// The previously selected model is gone, so the new first (only) model
	// is picked up in its place.
	if a.model.cfg.Name != "renamed" {
		t.Errorf("model = %q, want renamed", a.model.cfg.Name)
	}
}

func TestReloadConfigKeepsModelSelectionByName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"agent":{"models":[
		{"name":"local","base_url":"http://127.0.0.1:1/v1"},
		{"name":"gateway","base_url":"http://127.0.0.1:2/v1"}
	]}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	a := newTestAgent(t, path)
	a.model = a.findModel("gateway")
	if a.model == nil {
		t.Fatal("setup did not configure the gateway model")
	}

	if err := a.reloadConfig(); err != nil {
		t.Fatalf("reloadConfig: %v", err)
	}
	if a.model.cfg.Name != "gateway" {
		t.Errorf("model after reload = %q, want gateway to stay selected", a.model.cfg.Name)
	}
}

func TestReloadConfigLeavesStateOnInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeTestConfig(t, path, "local")
	a := newTestAgent(t, path)
	wantModel := a.model

	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := a.reloadConfig(); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
	if a.model != wantModel {
		t.Error("model changed even though the reload failed")
	}
	if len(a.models) != 1 {
		t.Errorf("models = %+v, want the original one untouched", a.models)
	}
}

func TestReloadConfigLeavesStateWhenNoModelsRemain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeTestConfig(t, path, "local")
	a := newTestAgent(t, path)
	wantModel := a.model

	if err := os.WriteFile(path, []byte(`{"agent":{"models":[]}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := a.reloadConfig()
	if err == nil {
		t.Fatal("expected an error when the file names no model endpoint")
	}
	if a.model != wantModel {
		t.Error("model changed even though the reload failed")
	}
}

func TestPrintUsageReportsTokensAndRate(t *testing.T) {
	var buf bytes.Buffer
	a := &agent{out: &buf, model: &llmClient{model: "m1"}}

	a.printUsage(&llmReply{InTokens: 100, OutTokens: 50}, 2*time.Second)
	got := buf.String()
	if !strings.Contains(got, "100 in / 50 out tokens") {
		t.Errorf("missing token counts: %q", got)
	}
	if !strings.Contains(got, "25.0 tok/s") {
		t.Errorf("expected 25.0 tok/s (50 tokens / 2s), got %q", got)
	}
}

func TestPrintUsageOmitsRateWithoutOutputTokens(t *testing.T) {
	var buf bytes.Buffer
	a := &agent{out: &buf, model: &llmClient{model: "m1"}}

	a.printUsage(&llmReply{InTokens: 40}, time.Second)
	got := buf.String()
	if !strings.Contains(got, "40 in / 0 out tokens") {
		t.Errorf("missing token counts: %q", got)
	}
	if strings.Contains(got, "tok/s") {
		t.Errorf("should not report a rate with zero output tokens: %q", got)
	}
}

func TestPrintUsagePrintsNothingWithoutUsage(t *testing.T) {
	var buf bytes.Buffer
	a := &agent{out: &buf, model: &llmClient{model: "m1"}}

	a.printUsage(&llmReply{}, time.Second)
	if buf.Len() != 0 {
		t.Errorf("expected no output, got %q", buf.String())
	}
}

func TestPrintUsageReportsContextWhenConfigured(t *testing.T) {
	var buf bytes.Buffer
	a := &agent{out: &buf, model: &llmClient{model: "m1", cfg: AgentModelConfig{ContextWindow: 1000}}}

	a.printUsage(&llmReply{InTokens: 250, OutTokens: 50}, 2*time.Second)
	got := buf.String()
	if !strings.Contains(got, "context 250/1000 (25%)") {
		t.Errorf("missing context use: %q", got)
	}
}

func TestPrintUsageOmitsContextWithoutConfiguredWindow(t *testing.T) {
	var buf bytes.Buffer
	a := &agent{out: &buf, model: &llmClient{model: "m1"}}

	a.printUsage(&llmReply{InTokens: 250, OutTokens: 50}, 2*time.Second)
	got := buf.String()
	if strings.Contains(got, "context") {
		t.Errorf("should not report context use without context_window configured: %q", got)
	}
}

func TestPrintUsageOmitsContextWithoutInputTokens(t *testing.T) {
	var buf bytes.Buffer
	a := &agent{out: &buf, model: &llmClient{model: "m1", cfg: AgentModelConfig{ContextWindow: 1000}}}

	// Output tokens alone (no reported input tokens) is not enough to say
	// anything meaningful about how full the context window is.
	a.printUsage(&llmReply{OutTokens: 50}, time.Second)
	got := buf.String()
	if strings.Contains(got, "context") {
		t.Errorf("should not report context use without input tokens: %q", got)
	}
}
