package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// AgentConfig is the "agent" section of config.json: what `codemcp agent`
// talks to. The local tools need no configuration here - they are the same
// tool set the server exposes, built from the rest of the file.
type AgentConfig struct {
	// Models are the LLM endpoints the agent may use. The first one, or the
	// one DefaultModel names, is selected at startup; /model switches.
	Models       []AgentModelConfig `json:"models"`
	DefaultModel string             `json:"default_model"`

	// SystemPrompt is appended to the agent's built-in instructions.
	SystemPrompt string `json:"system_prompt"`
	// MaxTurns bounds the model/tool round trips a single prompt may take.
	MaxTurns int `json:"max_turns"`
	// WallclockSeconds bounds the real time a single prompt may take, across
	// however many model/tool round trips it needs, the same way MaxTurns
	// bounds the count of them. 0, the default, means no limit.
	WallclockSeconds int `json:"wallclock_seconds,omitempty"`
	// DisableLocalTools leaves only the external connections as tools.
	DisableLocalTools bool `json:"disable_local_tools"`
	// AlwaysAllow names tools that never prompt for permission, local or
	// external (external tools by their prefixed name, e.g. "github__search").
	AlwaysAllow []string `json:"always_allow"`
	// Stream prints a model's reply as it arrives instead of waiting for the
	// whole thing - and, while /thinking is on, its reasoning too - for the
	// endpoint styles that support it (openai, anthropic, ollama; responses
	// and completions are always requested whole, streaming or not).
	// /stream toggles it at runtime.
	Stream bool `json:"stream,omitempty"`

	MCPServers     []AgentMCPConfig     `json:"mcp_servers"`
	OpenAPIServers []AgentOpenAPIConfig `json:"openapi_servers"`
}

// AgentAuth describes how a request is authorized. Every secret can be given
// inline, through an environment variable (the *_env field), or as a
// "${NAME}" reference inside the inline value.
type AgentAuth struct {
	// Type is "bearer", "basic", "header", "query" or "none". Empty means
	// bearer when a token is set, basic when a username is, and nothing
	// otherwise - except for the Anthropic style, whose default is x-api-key.
	Type     string `json:"type,omitempty"`
	Token    string `json:"token,omitempty"`
	TokenEnv string `json:"token_env,omitempty"`
	// Username and Password are for basic auth.
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	PasswordEnv string `json:"password_env,omitempty"`
	// Name is the header ("header" type, e.g. X-API-Key) or query parameter
	// ("query" type, e.g. api_key) the token is sent in.
	Name string `json:"name,omitempty"`
}

// AgentModelConfig is one LLM endpoint.
type AgentModelConfig struct {
	// Name is what /model and --model select it by.
	Name string `json:"name"`
	// BaseURL is the endpoint root, e.g. http://10.0.0.200:11434/v1 or
	// https://llmapi.example.com/v1. A missing scheme is filled in.
	BaseURL string `json:"base_url"`
	// Model is the model id sent in requests. Empty picks the first model the
	// endpoint lists.
	Model string `json:"model"`
	// Style is the API dialect: "auto" (the default, probed on first use),
	// "openai" (chat completions), "responses", "anthropic", "completions"
	// (legacy text completions) or "ollama" (native /api/chat).
	Style              string            `json:"style,omitempty"`
	Auth               AgentAuth         `json:"auth"`
	Headers            map[string]string `json:"headers,omitempty"`
	MaxTokens          int               `json:"max_tokens,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	TimeoutSeconds     int               `json:"timeout_seconds,omitempty"`
	InsecureSkipVerify bool              `json:"insecure_skip_verify,omitempty"`
	// RetryLimit is how many times a failed completion request is retried,
	// beyond the first attempt, with exponential backoff between tries. Only
	// a transient failure is retried - a network error, HTTP 429, or a 5xx -
	// since a 4xx like a bad request or bad credentials will not succeed on a
	// second try. nil (the field left out) means the default of 3; an
	// explicit 0 disables retries.
	RetryLimit *int `json:"retry_limit,omitempty"`
	// ExponentialBackoffLimit caps the delay, in seconds, between retries.
	// The delay itself starts at 1 second and doubles each attempt, so this
	// is the ceiling it climbs toward rather than a fixed wait. Zero or
	// unset uses the default of 30.
	ExponentialBackoffLimit int `json:"exponential_backoff_limit,omitempty"`
	// ContextWindow is this model's context length in tokens, for reporting
	// how full it is alongside token counts and generation speed. Nothing
	// here reports it on its own - a model endpoint's /v1/models listing
	// does not carry it in any style this agent speaks - so it is unset
	// (0) and left out of that line until given explicitly.
	ContextWindow int `json:"context_window,omitempty"`
}

// AgentMCPConfig is one external MCP server, reached over Streamable HTTP
// (URL) or by launching it as a subprocess speaking stdio (Command).
type AgentMCPConfig struct {
	Name     string            `json:"name"`
	Disabled bool              `json:"disabled,omitempty"`
	URL      string            `json:"url,omitempty"`
	Command  string            `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Auth     AgentAuth         `json:"auth"`
	Headers  map[string]string `json:"headers,omitempty"`
	// ReadOnlyTools never prompt for permission; the server's own
	// readOnlyHint annotations are honoured as well.
	ReadOnlyTools      []string `json:"read_only_tools,omitempty"`
	TimeoutSeconds     int      `json:"timeout_seconds,omitempty"`
	InsecureSkipVerify bool     `json:"insecure_skip_verify,omitempty"`
}

// AgentOpenAPIConfig is one external REST API described by an OpenAPI (3.x)
// or Swagger (2.0) JSON document; each operation becomes a tool.
type AgentOpenAPIConfig struct {
	Name     string `json:"name"`
	Disabled bool   `json:"disabled,omitempty"`
	// SpecURL is where the JSON specification is fetched from. A local file
	// path works too.
	SpecURL string `json:"spec_url"`
	// BaseURL overrides the server the specification names.
	BaseURL string            `json:"base_url,omitempty"`
	Auth    AgentAuth         `json:"auth"`
	Headers map[string]string `json:"headers,omitempty"`
	// Include, when set, is the only operationIds exposed; Exclude removes
	// operations from what is left.
	Include            []string `json:"include,omitempty"`
	Exclude            []string `json:"exclude,omitempty"`
	TimeoutSeconds     int      `json:"timeout_seconds,omitempty"`
	InsecureSkipVerify bool     `json:"insecure_skip_verify,omitempty"`
}

var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvRefs replaces ${NAME} references only; a bare $ inside a secret is
// left alone, which os.ExpandEnv would not do.
func expandEnvRefs(value string) string {
	return envRefPattern.ReplaceAllStringFunc(value, func(ref string) string {
		return os.Getenv(envRefPattern.FindStringSubmatch(ref)[1])
	})
}

func secretValue(inline, env string) string {
	if env != "" {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return expandEnvRefs(inline)
}

// apply adds the authorization to a request. defaultHeader, when not empty,
// is the header a bare token goes in instead of Authorization: Bearer.
func (a AgentAuth) apply(req *http.Request, defaultHeader string) {
	token := secretValue(a.Token, a.TokenEnv)
	switch strings.ToLower(a.Type) {
	case "none":
	case "bearer":
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	case "basic":
		req.SetBasicAuth(expandEnvRefs(a.Username), secretValue(a.Password, a.PasswordEnv))
	case "header":
		if name := firstNonEmpty(a.Name, defaultHeader, "X-API-Key"); token != "" {
			req.Header.Set(name, token)
		}
	case "query":
		if token != "" {
			q := req.URL.Query()
			q.Set(firstNonEmpty(a.Name, "api_key"), token)
			req.URL.RawQuery = q.Encode()
		}
	default:
		switch {
		case a.Username != "":
			req.SetBasicAuth(expandEnvRefs(a.Username), secretValue(a.Password, a.PasswordEnv))
		case token != "" && defaultHeader != "":
			req.Header.Set(defaultHeader, token)
		case token != "":
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
}

// describe says how a request is authorized, without the secret.
func (a AgentAuth) describe() string {
	token := secretValue(a.Token, a.TokenEnv)
	switch {
	case strings.EqualFold(a.Type, "none"):
		return "none"
	case a.Type != "":
		return a.Type
	case a.Username != "":
		return "basic"
	case token != "":
		return "bearer"
	}
	return "none"
}

func applyHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		req.Header.Set(k, expandEnvRefs(v))
	}
}

// normalizeEndpointURL accepts "10.0.0.200:11434/v1" as readily as a full URL.
// A bare address gets http:// when it looks local (an IP, localhost, or an
// explicit port) and https:// otherwise.
func normalizeEndpointURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("no URL given")
	}
	if !strings.Contains(raw, "://") {
		hostport, _, _ := strings.Cut(raw, "/")
		host := hostport
		hasPort := false
		if h, _, err := net.SplitHostPort(hostport); err == nil {
			host, hasPort = h, true
		}
		scheme := "https://"
		if hasPort || host == "localhost" || net.ParseIP(host) != nil {
			scheme = "http://"
		}
		raw = scheme + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid URL %q", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func newAgentHTTPClient(timeoutSeconds int, insecure bool, def time.Duration) *http.Client {
	timeout := def
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in per endpoint
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// validate checks the agent section for mistakes worth refusing to start over.
func (c *AgentConfig) validate() error {
	seen := map[string]bool{}
	for i, m := range c.Models {
		if m.BaseURL == "" {
			return fmt.Errorf("agent.models[%d]: base_url is required", i)
		}
		if m.Name == "" {
			c.Models[i].Name = firstNonEmpty(m.Model, fmt.Sprintf("model%d", i+1))
		}
		switch strings.ToLower(m.Style) {
		case "", "auto", styleOpenAI, styleResponses, styleAnthropic, styleCompletions, styleOllama:
		default:
			return fmt.Errorf("agent.models[%d].style: unknown style %q", i, m.Style)
		}
		if m.RetryLimit != nil && *m.RetryLimit < 0 {
			return fmt.Errorf("agent.models[%d].retry_limit: must be zero or more", i)
		}
		if m.ExponentialBackoffLimit < 0 {
			return fmt.Errorf("agent.models[%d].exponential_backoff_limit: must be zero or more", i)
		}
		if m.ContextWindow < 0 {
			return fmt.Errorf("agent.models[%d].context_window: must be zero or more", i)
		}
	}
	for i, s := range c.MCPServers {
		if s.Name == "" || (s.URL == "" && s.Command == "") {
			return fmt.Errorf("agent.mcp_servers[%d]: name and one of url or command are required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("agent: connection name %q is used twice", s.Name)
		}
		seen[s.Name] = true
	}
	for i, s := range c.OpenAPIServers {
		if s.Name == "" || s.SpecURL == "" {
			return fmt.Errorf("agent.openapi_servers[%d]: name and spec_url are required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("agent: connection name %q is used twice", s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}
