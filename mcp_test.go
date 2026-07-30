package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// newTestServer builds a server over a temporary workspace with one command.
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Workspace.Root = root
	cfg.Commands = []CommandConfig{{Name: "build", Description: "Build it.", Command: "echo built"}}
	if err := cfg.Normalize(root); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg.Server.Name, "test", cfg.Server.Instructions, cfg.Server.LegacyCompatibility)
	s.ws = NewWorkspace(cfg.Workspace)
	s.registerSystemTools(cfg)
	s.registerFileTools()
	s.registerEditTools()
	s.registerMarkdownTools()
	s.registerGrepTools()
	s.registerDiffTools()
	s.registerCommandTools(cfg.Commands)
	s.registerShellTool()
	return s, root
}

// call sends one request through the server and returns the decoded result.
func call(t *testing.T, s *Server, method string, params any) map[string]any {
	t.Helper()
	raw := withMeta(t, params)
	result, rpcErr := s.Handle(context.Background(), &Request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: method, Params: raw,
	})
	if rpcErr != nil {
		t.Fatalf("%s: %v", method, rpcErr)
	}
	return roundTrip(t, result)
}

// withMeta adds the per-request metadata every modern request must carry.
func withMeta(t *testing.T, params any) json.RawMessage {
	t.Helper()
	m := map[string]any{}
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
	}
	m["_meta"] = map[string]any{
		MetaProtocolVersion:    ProtocolVersion,
		MetaClientInfo:         map[string]string{"name": "test-client", "version": "1.0.0"},
		MetaClientCapabilities: map[string]any{},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func roundTrip(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDiscoverAdvertisesThisRevision(t *testing.T) {
	s, _ := newTestServer(t)
	got := call(t, s, "server/discover", nil)

	if got["resultType"] != ResultComplete {
		t.Errorf("resultType = %v, want %q", got["resultType"], ResultComplete)
	}
	versions, _ := got["supportedVersions"].([]any)
	if len(versions) == 0 || versions[0] != ProtocolVersion {
		t.Errorf("supportedVersions = %v, want %q first", versions, ProtocolVersion)
	}
	if got["cacheScope"] == nil || got["ttlMs"] == nil {
		t.Error("discover result is missing the cache hints this revision requires")
	}
	meta, _ := got["_meta"].(map[string]any)
	if _, ok := meta[MetaServerInfo]; !ok {
		t.Errorf("result _meta is missing %s", MetaServerInfo)
	}
}

func TestUnsupportedProtocolVersionIsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	params, err := json.Marshal(map[string]any{
		"_meta": map[string]any{MetaProtocolVersion: "1900-01-01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, rpcErr := s.Handle(context.Background(), &Request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list", Params: params,
	})
	if rpcErr == nil || rpcErr.Code != CodeUnsupportedProtocolVersion {
		t.Fatalf("want UnsupportedProtocolVersion, got %v", rpcErr)
	}
	data, _ := rpcErr.Data.(map[string]any)
	if data["requested"] != "1900-01-01" {
		t.Errorf("error data does not name the requested version: %v", rpcErr.Data)
	}
}

func TestToolsListIsDeterministicAndCacheable(t *testing.T) {
	s, _ := newTestServer(t)
	first := call(t, s, "tools/list", nil)
	second := call(t, s, "tools/list", nil)

	if first["cacheScope"] == nil || first["ttlMs"] == nil {
		t.Error("tools/list is missing the required cache hints")
	}
	a, _ := json.Marshal(first["tools"])
	b, _ := json.Marshal(second["tools"])
	if !bytes.Equal(a, b) {
		t.Error("tools/list is not returning a stable order")
	}
	tools, _ := first["tools"].([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		names[tool["name"].(string)] = true
		if tool["inputSchema"] == nil {
			t.Errorf("tool %v has no inputSchema", tool["name"])
		}
	}
	// The user-defined command must be a tool of its own name.
	for _, want := range []string{"build", "project_commands", "read_file", "run_command"} {
		if !names[want] {
			t.Errorf("tools/list is missing %q", want)
		}
	}
}

func TestCallToolReadsAndEditsTheWorkspace(t *testing.T) {
	s, root := newTestServer(t)

	got := call(t, s, "tools/call", map[string]any{
		"name":      "read_file",
		"arguments": map[string]any{"path": "hello.txt", "start_line": 2, "end_line": 2},
	})
	content, _ := got["content"].([]any)
	block, _ := content[0].(map[string]any)
	if block["text"] != "two" {
		t.Errorf("read_file returned %q, want %q", block["text"], "two")
	}

	call(t, s, "tools/call", map[string]any{
		"name":      "edit_file",
		"arguments": map[string]any{"path": "hello.txt", "old_string": "two", "new_string": "TWO"},
	})
	data, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "TWO") {
		t.Errorf("edit_file did not write the change: %q", data)
	}
}

func TestToolErrorsAreReportedInTheResult(t *testing.T) {
	s, _ := newTestServer(t)
	got := call(t, s, "tools/call", map[string]any{
		"name":      "read_file",
		"arguments": map[string]any{"path": "does-not-exist.txt"},
	})
	if got["isError"] != true {
		t.Errorf("a missing file should be a tool execution error, got %v", got)
	}
}

func TestUnknownToolIsAProtocolError(t *testing.T) {
	s, _ := newTestServer(t)
	_, rpcErr := s.Handle(context.Background(), &Request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: withMeta(t, map[string]any{"name": "no_such_tool"}),
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("want InvalidParams for an unknown tool, got %v", rpcErr)
	}
}

func TestWorkspaceRefusesEscapes(t *testing.T) {
	s, _ := newTestServer(t)
	for _, path := range []string{"../outside.txt", "..", filepath.Join("sub", "..", "..", "escape")} {
		if _, err := s.ws.Resolve(path); err == nil {
			t.Errorf("Resolve(%q) should have been refused", path)
		}
	}
}

func TestCommandToolRuns(t *testing.T) {
	s, _ := newTestServer(t)
	got := call(t, s, "tools/call", map[string]any{"name": "build"})
	if got["isError"] == true {
		t.Fatalf("build failed: %v", got)
	}
	content, _ := got["content"].([]any)
	block, _ := content[0].(map[string]any)
	if !strings.Contains(block["text"].(string), "built") {
		t.Errorf("build output does not contain the command output: %q", block["text"])
	}
}

func TestSystemInfo(t *testing.T) {
	s, root := newTestServer(t)
	got := call(t, s, "tools/call", map[string]any{
		"name":      "system_info",
		"arguments": map[string]any{"check_programs": false},
	})
	if got["isError"] == true {
		t.Fatalf("system_info failed: %v", got)
	}

	structured, _ := got["structuredContent"].(map[string]any)
	if structured["os"] != runtime.GOOS {
		t.Errorf("os = %v, want %q", structured["os"], runtime.GOOS)
	}
	if structured["arch"] != runtime.GOARCH {
		t.Errorf("arch = %v, want %q", structured["arch"], runtime.GOARCH)
	}
	if structured["workspace_root"] != root {
		t.Errorf("workspace_root = %v, want %q", structured["workspace_root"], root)
	}
	// The shell and separator are the point of the tool: they tell the model
	// which syntax to write.
	if structured["shell"] == "" || structured["shell_flavor"] == "" {
		t.Error("system_info did not report a shell")
	}
	if structured["path_separator"] != string(filepath.Separator) {
		t.Errorf("path_separator = %v", structured["path_separator"])
	}

	content, _ := got["content"].([]any)
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	if !strings.Contains(text, osDisplayName(runtime.GOOS)) {
		t.Errorf("the text summary does not name the OS: %q", text)
	}
}

func TestSystemInfoProgramProbe(t *testing.T) {
	s, _ := newTestServer(t)
	got := call(t, s, "tools/call", map[string]any{"name": "system_info"})
	structured, _ := got["structuredContent"].(map[string]any)

	found, _ := structured["programs"].([]any)
	missing, _ := structured["missing"].([]any)
	if len(found)+len(missing) == 0 {
		t.Fatal("the program probe reported nothing either way")
	}
	// go built this test, so it is definitely on PATH.
	var all []string
	for _, p := range found {
		all = append(all, p.(string))
	}
	if !strings.Contains(strings.Join(all, " "), "go (") {
		t.Errorf("go should have been found on PATH: %v", all)
	}
}

func TestOSDisplayName(t *testing.T) {
	cases := map[string]string{
		"windows": "Windows",
		"darwin":  "macOS",
		"linux":   "Linux",
		"weird":   "weird", // an unknown GOOS is reported as itself
	}
	for goos, want := range cases {
		if got := osDisplayName(goos); got != want {
			t.Errorf("osDisplayName(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestFindPrograms(t *testing.T) {
	found, missing := findPrograms([]string{"go", "go", "", "definitely-not-a-real-program-xyz"})
	if len(found) != 1 || !strings.HasPrefix(found[0], "go (") {
		t.Errorf("found = %v, want go once", found)
	}
	if len(missing) != 1 || missing[0] != "definitely-not-a-real-program-xyz" {
		t.Errorf("missing = %v", missing)
	}
}

// --- HTTP transport -------------------------------------------------------

func newTestTransport(t *testing.T) (*HTTPTransport, *Server) {
	t.Helper()
	s, _ := newTestServer(t)
	cfg := DefaultConfig().Server
	return NewHTTPTransport(s, cfg, "/mcp", log.New(io.Discard, "", 0)), s
}

func post(t *testing.T, tr *HTTPTransport, body json.RawMessage, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	tr.Handler().ServeHTTP(rec, req)
	return rec
}

func rpcBody(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": json.RawMessage(withMeta(t, params)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestHTTPRoundTrip(t *testing.T) {
	tr, _ := newTestTransport(t)
	rec := post(t, tr, rpcBody(t, "tools/list", nil), map[string]string{
		"MCP-Protocol-Version": ProtocolVersion,
		"Mcp-Method":           "tools/list",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result["resultType"] != ResultComplete {
		t.Errorf("resultType = %v", resp.Result["resultType"])
	}
}

func TestHTTPRequiresMirroredHeaders(t *testing.T) {
	tr, _ := newTestTransport(t)
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no protocol version", map[string]string{"Mcp-Method": "tools/list"}},
		{"no method header", map[string]string{"MCP-Protocol-Version": ProtocolVersion}},
		{"method mismatch", map[string]string{
			"MCP-Protocol-Version": ProtocolVersion, "Mcp-Method": "tools/call",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, tr, rpcBody(t, "tools/list", nil), tc.headers)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body)
			}
			var resp struct {
				Error *RPCError `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Error == nil || resp.Error.Code != CodeHeaderMismatch {
				t.Fatalf("want HeaderMismatch, got %+v", resp.Error)
			}
		})
	}
}

func TestHTTPValidatesMcpName(t *testing.T) {
	tr, _ := newTestTransport(t)
	body := rpcBody(t, "tools/call", map[string]any{
		"name": "read_file", "arguments": map[string]any{"path": "hello.txt"},
	})
	base := map[string]string{"MCP-Protocol-Version": ProtocolVersion, "Mcp-Method": "tools/call"}

	rec := post(t, tr, body, base)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a missing Mcp-Name should be rejected, got %d", rec.Code)
	}

	wrong := map[string]string{"Mcp-Name": "write_file"}
	for k, v := range base {
		wrong[k] = v
	}
	if rec := post(t, tr, body, wrong); rec.Code != http.StatusBadRequest {
		t.Errorf("a mismatched Mcp-Name should be rejected, got %d", rec.Code)
	}

	right := map[string]string{"Mcp-Name": "read_file"}
	for k, v := range base {
		right[k] = v
	}
	if rec := post(t, tr, body, right); rec.Code != http.StatusOK {
		t.Errorf("status = %d, body = %s", rec.Code, rec.Body)
	}

	// The base64 sentinel form must be decoded before comparison.
	encoded := map[string]string{
		"Mcp-Name": "=?base64?" + base64.StdEncoding.EncodeToString([]byte("read_file")) + "?=",
	}
	for k, v := range base {
		encoded[k] = v
	}
	if rec := post(t, tr, body, encoded); rec.Code != http.StatusOK {
		t.Errorf("base64 Mcp-Name rejected: %d %s", rec.Code, rec.Body)
	}
}

func TestHTTPRejectsGETAndDELETE(t *testing.T) {
	tr, _ := newTestTransport(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		rec := httptest.NewRecorder()
		tr.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/mcp", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s returned %d, want 405 in protocol version %s", method, rec.Code, ProtocolVersion)
		}
	}
}

func TestHTTPRejectsForeignOrigin(t *testing.T) {
	tr, _ := newTestTransport(t)
	rec := post(t, tr, rpcBody(t, "tools/list", nil), map[string]string{
		"MCP-Protocol-Version": ProtocolVersion,
		"Mcp-Method":           "tools/list",
		"Origin":               "https://evil.example",
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// newOriginTransport builds a transport with a specific origin allowlist.
func newOriginTransport(t *testing.T, origins ...string) *HTTPTransport {
	t.Helper()
	s, _ := newTestServer(t)
	cfg := DefaultConfig().Server
	cfg.AllowedOrigins = origins
	return NewHTTPTransport(s, cfg, "/mcp", log.New(io.Discard, "", 0))
}

func preflight(t *testing.T, tr *HTTPTransport, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodOptions, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	tr.Handler().ServeHTTP(rec, req)
	return rec
}

func TestCORSPreflightIsAnswered(t *testing.T) {
	tr := newOriginTransport(t, "https://inspector.example")
	rec := preflight(t, tr, "/mcp", map[string]string{
		"Origin":                         "https://inspector.example",
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "content-type, mcp-method, mcp-param-region",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	h := rec.Header()
	if got := h.Get("Access-Control-Allow-Origin"); got != "https://inspector.example" {
		t.Errorf("Allow-Origin = %q", got)
	}
	if !strings.Contains(h.Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("Allow-Methods = %q", h.Get("Access-Control-Allow-Methods"))
	}
	// The requested headers are echoed, so a client mirroring a tool argument
	// into an Mcp-Param-* header it named itself is not blocked.
	if got := h.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "mcp-param-region") {
		t.Errorf("Allow-Headers = %q, want the requested headers echoed", got)
	}
	if h.Get("Access-Control-Max-Age") == "" {
		t.Error("no Access-Control-Max-Age on the preflight")
	}
	if h.Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("an explicitly listed origin should be allowed credentials")
	}
}

func TestCORSPreflightWithoutRequestedHeaders(t *testing.T) {
	tr := newOriginTransport(t, "*")
	rec := preflight(t, tr, "/mcp", map[string]string{
		"Origin":                        "https://anywhere.example",
		"Access-Control-Request-Method": "POST",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	allow := rec.Header().Get("Access-Control-Allow-Headers")
	for _, want := range []string{"MCP-Protocol-Version", "Mcp-Method", "Mcp-Name", "Authorization"} {
		if !strings.Contains(allow, want) {
			t.Errorf("Allow-Headers = %q, missing %s", allow, want)
		}
	}
}

func TestCORSExposesTheSessionHeader(t *testing.T) {
	// A browser can only read the headers named in Expose-Headers. Without the
	// session id there, a legacy client in a page completes initialize and then
	// cannot tell the server which session it belongs to.
	tr := newOriginTransport(t, "*")
	rec := post(t, tr, legacyBody(t, 1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]string{"name": "inspector", "version": "1"},
	}), map[string]string{"Origin": "http://localhost:6274"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get(sessionHeader) == "" {
		t.Fatal("initialize minted no session id")
	}
	exposed := rec.Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(exposed, sessionHeader) {
		t.Errorf("Expose-Headers = %q, must name %s or a browser cannot read it", exposed, sessionHeader)
	}
}

func TestCORSPreflightAllowsTheSessionHeader(t *testing.T) {
	// The follow-up requests send the session id back, so a preflight that
	// names no headers must still advertise it as allowed.
	tr := newOriginTransport(t, "*")
	rec := preflight(t, tr, "/mcp", map[string]string{
		"Origin":                        "http://localhost:6274",
		"Access-Control-Request-Method": "POST",
	})
	if allow := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(allow, sessionHeader) {
		t.Errorf("Allow-Headers = %q, missing %s", allow, sessionHeader)
	}
}

func TestCORSAllowedHeadersConfig(t *testing.T) {
	newTransport := func(headers ...string) *HTTPTransport {
		s, _ := newTestServer(t)
		cfg := DefaultConfig().Server
		cfg.AllowedOrigins = []string{"*"}
		cfg.AllowedHeaders = headers
		return NewHTTPTransport(s, cfg, "/mcp", log.New(io.Discard, "", 0))
	}
	ask := map[string]string{
		"Origin":                         "http://localhost:6274",
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "content-type, x-custom",
	}

	t.Run("wildcard echoes the request", func(t *testing.T) {
		rec := preflight(t, newTransport("*"), "/mcp", ask)
		if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "content-type, x-custom" {
			t.Errorf("Allow-Headers = %q", got)
		}
	})

	t.Run("wildcard with no request asked", func(t *testing.T) {
		bare := map[string]string{"Origin": "http://localhost:6274", "Access-Control-Request-Method": "POST"}
		rec := preflight(t, newTransport("*"), "/mcp", bare)
		if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "*" {
			t.Errorf("Allow-Headers = %q, want the wildcard", got)
		}
	})

	t.Run("an explicit list restricts", func(t *testing.T) {
		rec := preflight(t, newTransport("Content-Type", "Authorization"), "/mcp", ask)
		got := rec.Header().Get("Access-Control-Allow-Headers")
		if got != "Content-Type, Authorization" {
			t.Errorf("Allow-Headers = %q, want the configured list", got)
		}
		if strings.Contains(got, "x-custom") {
			t.Error("an explicit list must not echo unlisted headers")
		}
	})

	t.Run("unset echoes the request", func(t *testing.T) {
		rec := preflight(t, newTransport(), "/mcp", ask)
		if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "content-type, x-custom" {
			t.Errorf("Allow-Headers = %q", got)
		}
	})
}

func TestCORSPrivateNetworkGrant(t *testing.T) {
	// Chrome asks before letting a page on a public address reach 127.0.0.1.
	// Without the grant the request never leaves the browser.
	ask := map[string]string{
		"Origin":                                 "https://app.example",
		"Access-Control-Request-Method":          "POST",
		"Access-Control-Request-Private-Network": "true",
	}

	tr := newOriginTransport(t, "*")
	rec := preflight(t, tr, "/mcp", ask)
	if got := rec.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Errorf("Allow-Private-Network = %q, want true", got)
	}

	// It is not volunteered when the browser did not ask.
	quiet := preflight(t, tr, "/mcp", map[string]string{
		"Origin": "https://app.example", "Access-Control-Request-Method": "POST",
	})
	if quiet.Header().Get("Access-Control-Allow-Private-Network") != "" {
		t.Error("the grant should only answer an actual private-network request")
	}

	// A disallowed origin never reaches the grant.
	strict := newOriginTransport(t, "http://localhost")
	if rec := preflight(t, strict, "/mcp", ask); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a foreign origin", rec.Code)
	}

	// And it can be turned off.
	s, _ := newTestServer(t)
	cfg := DefaultConfig().Server
	cfg.AllowedOrigins = []string{"*"}
	cfg.AllowPrivateNetwork = false
	off := NewHTTPTransport(s, cfg, "/mcp", log.New(io.Discard, "", 0))
	if rec := preflight(t, off, "/mcp", ask); rec.Header().Get("Access-Control-Allow-Private-Network") != "" {
		t.Error("the grant should be withheld when allow_private_network is false")
	}
}

func TestCORSWildcardAllowsAnyOrigin(t *testing.T) {
	tr := newOriginTransport(t, "*")
	rec := post(t, tr, rpcBody(t, "tools/list", nil), map[string]string{
		"MCP-Protocol-Version": ProtocolVersion,
		"Mcp-Method":           "tools/list",
		"Origin":               "https://anywhere.example",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://anywhere.example" {
		t.Errorf("Allow-Origin = %q, want the origin echoed", got)
	}
	// A wildcard means every origin qualifies, so credentials are not granted.
	if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Error("the wildcard should not grant credentials")
	}
}

func TestCORSPreflightFromForeignOriginIsRefused(t *testing.T) {
	tr := newOriginTransport(t, "http://localhost")
	rec := preflight(t, tr, "/mcp", map[string]string{
		"Origin":                        "https://evil.example",
		"Access-Control-Request-Method": "POST",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("a refused preflight must not carry Allow-Origin")
	}
}

func TestCORSPreflightNeedsNoAuthToken(t *testing.T) {
	s, _ := newTestServer(t)
	cfg := DefaultConfig().Server
	cfg.AuthToken = "s3cret"
	cfg.AllowedOrigins = []string{"*"}
	tr := NewHTTPTransport(s, cfg, "/mcp", log.New(io.Discard, "", 0))

	// Browsers never attach credentials to a preflight, so requiring the token
	// here would make the server unreachable from a page.
	rec := preflight(t, tr, "/mcp", map[string]string{
		"Origin":                        "https://anywhere.example",
		"Access-Control-Request-Method": "POST",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

func TestCORSHeadersOnRealResponses(t *testing.T) {
	tr := newOriginTransport(t, "http://localhost")
	rec := post(t, tr, rpcBody(t, "tools/list", nil), map[string]string{
		"MCP-Protocol-Version": ProtocolVersion,
		"Mcp-Method":           "tools/list",
		"Origin":               "http://localhost:5173",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Allow-Origin = %q", got)
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary = %q, want Origin", rec.Header().Get("Vary"))
	}
}

func TestCORSServerWideOptions(t *testing.T) {
	tr := newOriginTransport(t, "*")
	// "OPTIONS *" reaches the handler because the server disables net/http's
	// own general OPTIONS handler.
	rec := preflight(t, tr, "*", map[string]string{
		"Origin":                        "https://anywhere.example",
		"Access-Control-Request-Method": "POST",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Error("OPTIONS * should carry the CORS headers")
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" https://a.example , http://localhost:5173 ,, ")
	want := []string{"https://a.example", "http://localhost:5173"}
	if len(got) != len(want) {
		t.Fatalf("splitList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitList[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if splitList("") != nil {
		t.Error("an empty value should produce no origins")
	}
}

func TestHTTPUnknownMethodIs404(t *testing.T) {
	tr, _ := newTestTransport(t)
	rec := post(t, tr, rpcBody(t, "does/not/exist", nil), map[string]string{
		"MCP-Protocol-Version": ProtocolVersion,
		"Mcp-Method":           "does/not/exist",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var resp struct {
		Error *RPCError `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != CodeMethodNotFound {
		t.Errorf("want a JSON-RPC method-not-found body, got %+v", resp.Error)
	}
}

func TestHTTPNotificationIsAccepted(t *testing.T) {
	tr, _ := newTestTransport(t)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled"})
	rec := post(t, tr, body, nil)
	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a notification should get an empty body, got %q", rec.Body)
	}
}

func TestHTTPAuthToken(t *testing.T) {
	s, _ := newTestServer(t)
	cfg := DefaultConfig().Server
	cfg.AuthToken = "s3cret"
	tr := NewHTTPTransport(s, cfg, "/mcp", log.New(io.Discard, "", 0))

	headers := map[string]string{"MCP-Protocol-Version": ProtocolVersion, "Mcp-Method": "tools/list"}
	if rec := post(t, tr, rpcBody(t, "tools/list", nil), headers); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	headers["Authorization"] = "Bearer s3cret"
	if rec := post(t, tr, rpcBody(t, "tools/list", nil), headers); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// --- backwards compatibility ----------------------------------------------

// legacyBody builds a request in the legacy shape: no _meta, no mirrored
// headers, just JSON-RPC.
func legacyBody(t *testing.T, id int, method string, params any) json.RawMessage {
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// legacyInitialize performs the handshake and returns the session id and result.
func legacyInitialize(t *testing.T, tr *HTTPTransport, version string) (string, map[string]any) {
	t.Helper()
	rec := post(t, tr, legacyBody(t, 1, "initialize", map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "legacy-client", "version": "0.9"},
	}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize returned %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  *RPCError      `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("initialize failed: %v", resp.Error)
	}
	id := rec.Header().Get(sessionHeader)
	if id == "" {
		t.Fatal("initialize did not mint a session id")
	}
	return id, resp.Result
}

func TestLegacyInitializeHandshake(t *testing.T) {
	tr, _ := newTestTransport(t)
	_, result := legacyInitialize(t, tr, "2025-11-25")

	if result["protocolVersion"] != "2025-11-25" {
		t.Errorf("protocolVersion = %v, want the requested legacy version echoed", result["protocolVersion"])
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] == nil {
		t.Error("initialize result has no serverInfo")
	}
	if result["capabilities"] == nil {
		t.Error("initialize result has no capabilities")
	}
	// resultType and the cache hints did not exist yet; sending them to a
	// legacy client would be noise at best.
	if _, ok := result["resultType"]; ok {
		t.Error("a legacy initialize result must not carry resultType")
	}
}

func TestLegacyVersionNegotiationFallsBack(t *testing.T) {
	tr, _ := newTestTransport(t)
	_, result := legacyInitialize(t, tr, "2024-11-05")
	if result["protocolVersion"] != LatestLegacyVersion {
		t.Errorf("protocolVersion = %v, want %q for an unsupported request",
			result["protocolVersion"], LatestLegacyVersion)
	}
}

func TestLegacySessionServesTools(t *testing.T) {
	tr, _ := newTestTransport(t)
	id, _ := legacyInitialize(t, tr, "2025-11-25")

	// A legacy client sends no _meta and none of the mirrored headers.
	rec := post(t, tr, legacyBody(t, 2, "tools/list", nil), map[string]string{sessionHeader: id})
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list returned %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get(sessionHeader); got != id {
		t.Errorf("session id = %q, want it echoed as %q", got, id)
	}
	var resp struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.Result["resultType"]; ok {
		t.Error("a legacy result must not carry resultType")
	}
	if _, ok := resp.Result["ttlMs"]; ok {
		t.Error("a legacy result must not carry the cache hints")
	}
	tools, _ := resp.Result["tools"].([]any)
	if len(tools) == 0 {
		t.Error("a legacy client got no tools")
	}
}

func TestLegacyToolCallWorks(t *testing.T) {
	tr, _ := newTestTransport(t)
	id, _ := legacyInitialize(t, tr, "2025-06-18")

	rec := post(t, tr, legacyBody(t, 2, "tools/call", map[string]any{
		"name": "read_file", "arguments": map[string]any{"path": "hello.txt"},
	}), map[string]string{sessionHeader: id})
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/call returned %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Result CallToolResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result.IsError {
		t.Fatalf("tool call failed: %+v", resp.Result.Content)
	}
	if !strings.Contains(resp.Result.Content[0].Text, "two") {
		t.Errorf("content = %q", resp.Result.Content[0].Text)
	}
}

func TestLegacySessionLifecycle(t *testing.T) {
	tr, _ := newTestTransport(t)
	id, _ := legacyInitialize(t, tr, "2025-11-25")
	if tr.sessions.Count() != 1 {
		t.Fatalf("session count = %d, want 1", tr.sessions.Count())
	}

	// DELETE terminates it, as the legacy transport specifies.
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set(sessionHeader, id)
	rec := httptest.NewRecorder()
	tr.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE returned %d, want 204", rec.Code)
	}
	if tr.sessions.Count() != 0 {
		t.Errorf("session count = %d after DELETE, want 0", tr.sessions.Count())
	}

	// A request on the dead session gets 404 so the client knows to start over.
	after := post(t, tr, legacyBody(t, 3, "tools/list", nil), map[string]string{sessionHeader: id})
	if after.Code != http.StatusNotFound {
		t.Errorf("status = %d after termination, want 404", after.Code)
	}
}

func TestLegacyGetStreamIsAccepted(t *testing.T) {
	tr, _ := newTestTransport(t)
	id, _ := legacyInitialize(t, tr, "2025-11-25")

	// The standalone SSE stream a legacy client opens. It stays open, so the
	// request is cancelled once the headers have been checked.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(sessionHeader, id)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		tr.Handler().ServeHTTP(rec, req)
		close(done)
	}()
	cancel()
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("GET returned %d, want 200 for a legacy client", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestLegacyPingAndNoOpMethods(t *testing.T) {
	tr, _ := newTestTransport(t)
	id, _ := legacyInitialize(t, tr, "2025-11-25")
	for _, method := range []string{"ping", "logging/setLevel", "resources/subscribe", "resources/unsubscribe"} {
		rec := post(t, tr, legacyBody(t, 4, method, map[string]any{"level": "info", "uri": "workspace:///hello.txt"}),
			map[string]string{sessionHeader: id})
		if rec.Code != http.StatusOK {
			t.Errorf("%s returned %d: %s", method, rec.Code, rec.Body)
		}
	}
}

func TestModernRequestsStillGetTheModernShape(t *testing.T) {
	// Serving both eras must not change what a modern client sees.
	tr, _ := newTestTransport(t)
	rec := post(t, tr, rpcBody(t, "tools/list", nil), map[string]string{
		"MCP-Protocol-Version": ProtocolVersion,
		"Mcp-Method":           "tools/list",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result["resultType"] != ResultComplete {
		t.Errorf("resultType = %v", resp.Result["resultType"])
	}
	if resp.Result["ttlMs"] == nil {
		t.Error("a modern result lost its cache hints")
	}
	if rec.Header().Get(sessionHeader) != "" {
		t.Error("a modern request must not be given a session id")
	}
}

func TestDiscoverAdvertisesBothEras(t *testing.T) {
	tr, _ := newTestTransport(t)
	rec := post(t, tr, rpcBody(t, "server/discover", nil), map[string]string{
		"MCP-Protocol-Version": ProtocolVersion,
		"Mcp-Method":           "server/discover",
	})
	var resp struct {
		Result DiscoverResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := resp.Result.SupportedVersions
	if len(got) == 0 || got[0] != ProtocolVersion {
		t.Fatalf("supportedVersions = %v, want the current revision first", got)
	}
	found := false
	for _, v := range got {
		if v == "2025-11-25" {
			found = true
		}
	}
	if !found {
		t.Errorf("supportedVersions = %v, want the legacy revisions listed too", got)
	}
}

func TestNoLegacyRefusesInitialize(t *testing.T) {
	s, _ := newTestServer(t)
	s.legacy = false
	cfg := DefaultConfig().Server
	cfg.LegacyCompatibility = false
	tr := NewHTTPTransport(s, cfg, "/mcp", log.New(io.Discard, "", 0))

	rec := post(t, tr, legacyBody(t, 1, "initialize", map[string]any{
		"protocolVersion": "2025-11-25",
	}), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var resp struct {
		Error *RPCError `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil {
		t.Fatal("no error body")
	}
	// A legacy client has no fall-forward mechanism, so the error has to name
	// the versions this server does speak.
	if !strings.Contains(resp.Error.Message, ProtocolVersion) {
		t.Errorf("the refusal should name the supported versions: %q", resp.Error.Message)
	}
	if s.supports("2025-11-25") {
		t.Error("with legacy off, a legacy version must not be supported")
	}
}

func TestNoLegacyRejectsGET(t *testing.T) {
	s, _ := newTestServer(t)
	s.legacy = false
	cfg := DefaultConfig().Server
	cfg.LegacyCompatibility = false
	tr := NewHTTPTransport(s, cfg, "/mcp", log.New(io.Discard, "", 0))

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	tr.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 when legacy compatibility is off", rec.Code)
	}
}

func TestLegacyStdioHandshake(t *testing.T) {
	s, _ := newTestServer(t)
	var in bytes.Buffer
	in.Write(legacyBody(t, 1, "initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"clientInfo":      map[string]string{"name": "legacy", "version": "1"},
	}))
	in.WriteString("\n")
	in.Write(legacyBody(t, 2, "tools/list", nil))
	in.WriteString("\n")

	var out bytes.Buffer
	tr := NewStdioTransport(s, &in, &out, true, log.New(io.Discard, "", 0))
	if err := tr.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 responses, got %d: %q", len(lines), out.String())
	}
	var initResp struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatal(err)
	}
	if initResp.Result["protocolVersion"] != "2025-11-25" {
		t.Errorf("protocolVersion = %v", initResp.Result["protocolVersion"])
	}
	// The negotiated version is process-scoped, so the follow-up request is
	// answered in the legacy shape without repeating anything.
	var listResp struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listResp); err != nil {
		t.Fatal(err)
	}
	if _, ok := listResp.Result["resultType"]; ok {
		t.Error("a legacy stdio result must not carry resultType")
	}
	if listResp.Result["tools"] == nil {
		t.Error("a legacy stdio client got no tools")
	}
}

func TestSessionStoreExpiry(t *testing.T) {
	store := NewSessionStore(time.Millisecond)
	session := store.Create("2025-11-25", Implementation{Name: "c"})
	if _, ok := store.Get(session.ID); !ok {
		t.Fatal("a fresh session should be found")
	}
	time.Sleep(5 * time.Millisecond)
	if _, ok := store.Get(session.ID); ok {
		t.Error("an idle session should have been evicted")
	}
}

func TestSessionIDsAreUnique(t *testing.T) {
	store := NewSessionStore(time.Hour)
	seen := map[string]bool{}
	for range 100 {
		id := store.Create("2025-11-25", Implementation{}).ID
		if seen[id] {
			t.Fatalf("duplicate session id %q", id)
		}
		seen[id] = true
	}
}

// --- config ---------------------------------------------------------------

func TestEndpointSplitsTheURL(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.URL = "http://0.0.0.0:9001/mcp/v1"
	addr, path, err := cfg.Endpoint()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "0.0.0.0:9001" || path != "/mcp/v1" {
		t.Errorf("addr, path = %q, %q", addr, path)
	}
}

func TestTLSConfig(t *testing.T) {
	t.Run("http needs none", func(t *testing.T) {
		cfg := DefaultConfig()
		got, err := cfg.TLSConfig()
		if err != nil || got != nil {
			t.Errorf("TLSConfig = %v, %v; want nil, nil for an http URL", got, err)
		}
	})

	t.Run("https without a certificate is refused", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Server.URL = "https://127.0.0.1:8765/mcp"
		_, err := cfg.TLSConfig()
		if err == nil {
			t.Fatal("an https URL with no certificate should not start")
		}
		// The error has to say how to fix it, since this is the path someone
		// lands on when a browser refuses their plain-http server.
		if !strings.Contains(err.Error(), "tls_self_signed") {
			t.Errorf("the error should name the way out: %v", err)
		}
	})

	t.Run("self-signed", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Server.URL = "https://127.0.0.1:8765/mcp"
		cfg.Server.TLSSelfSigned = true
		got, err := cfg.TLSConfig()
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Certificates) != 1 {
			t.Fatalf("want one certificate, got %d", len(got.Certificates))
		}
		leaf, err := x509.ParseCertificate(got.Certificates[0].Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		// The certificate must cover however the client spells the address.
		if err := leaf.VerifyHostname("localhost"); err != nil {
			t.Errorf("certificate does not cover localhost: %v", err)
		}
		if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
			t.Errorf("certificate does not cover 127.0.0.1: %v", err)
		}
		if leaf.NotAfter.Before(time.Now()) {
			t.Error("the generated certificate is already expired")
		}
	})
}

func TestIsTLS(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:8765/mcp":  false,
		"https://127.0.0.1:8765/mcp": true,
		"https://app.example/mcp":    true,
		"":                           false,
	}
	for url, want := range cases {
		if got := (ServerConfig{URL: url}).IsTLS(); got != want {
			t.Errorf("IsTLS(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestEndpointsSingleURL(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.URL = "http://127.0.0.1:9001/mcp"
	endpoints, err := cfg.Endpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != 1 {
		t.Fatalf("want 1 endpoint, got %d", len(endpoints))
	}
	if endpoints[0].Addr != "127.0.0.1:9001" || endpoints[0].Path != "/mcp" || endpoints[0].TLS {
		t.Errorf("endpoint = %+v", endpoints[0])
	}
}

func TestEndpointsHTTPAndHTTPSTogether(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.AdditionalURLs = []string{
		"http://127.0.0.1:8765/mcp",
		"https://127.0.0.1:8766/mcp",
	}
	endpoints, err := cfg.Endpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != 2 {
		t.Fatalf("want 2 endpoints, got %d", len(endpoints))
	}
	if endpoints[0].TLS {
		t.Error("the first endpoint should be plain http")
	}
	if !endpoints[1].TLS {
		t.Error("the second endpoint should be https")
	}
	// One https endpoint is enough to need a certificate.
	if !cfg.Server.IsTLS() {
		t.Error("IsTLS should be true when any endpoint is https")
	}

	listeners, err := cfg.Listeners()
	if err != nil {
		t.Fatal(err)
	}
	if len(listeners) != 2 {
		t.Fatalf("two addresses means two listeners, got %d", len(listeners))
	}
	if listeners[0].TLS || !listeners[1].TLS {
		t.Errorf("listener TLS flags = %v, %v", listeners[0].TLS, listeners[1].TLS)
	}
}

func TestListenersGroupPathsSharingAnAddress(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.AdditionalURLs = []string{
		"http://127.0.0.1:8765/mcp",
		"http://127.0.0.1:8765/api/mcp",
	}
	listeners, err := cfg.Listeners()
	if err != nil {
		t.Fatal(err)
	}
	// One address is one socket, however many paths it answers on.
	if len(listeners) != 1 {
		t.Fatalf("want 1 listener, got %d", len(listeners))
	}
	if len(listeners[0].Paths) != 2 {
		t.Errorf("paths = %v, want both", listeners[0].Paths)
	}
}

func TestEndpointsRejectMixedSchemesOnOnePort(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.AdditionalURLs = []string{
		"http://127.0.0.1:8765/mcp",
		"https://127.0.0.1:8765/secure",
	}
	_, err := cfg.Endpoints()
	if err == nil {
		t.Fatal("one port cannot serve both http and https")
	}
	if !strings.Contains(err.Error(), "different ports") {
		t.Errorf("the error should say how to fix it: %v", err)
	}
}

func TestEndpointsRejectDuplicates(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.AdditionalURLs = []string{"http://127.0.0.1:8765/mcp", "http://127.0.0.1:8765/mcp"}
	if _, err := cfg.Endpoints(); err == nil {
		t.Error("a duplicated URL should be rejected")
	}
}

func TestSelfSignedCertificateCoversEveryEndpointHost(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.AdditionalURLs = []string{
		"http://127.0.0.1:8765/mcp",
		"https://dev.example:8766/mcp",
	}
	cfg.Server.TLSSelfSigned = true
	tlsConfig, err := cfg.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"dev.example", "localhost", "127.0.0.1"} {
		if err := leaf.VerifyHostname(host); err != nil {
			t.Errorf("certificate does not cover %s: %v", host, err)
		}
	}
}

func TestHandlerForServesSeveralPaths(t *testing.T) {
	s, _ := newTestServer(t)
	cfg := DefaultConfig().Server
	tr := NewHTTPTransport(s, cfg, "/mcp", log.New(io.Discard, "", 0))
	handler := tr.HandlerFor("/mcp", "/api/mcp")

	for _, path := range []string{"/mcp", "/api/mcp"} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(rpcBody(t, "tools/list", nil)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/list")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s returned %d: %s", path, rec.Code, rec.Body)
		}
	}
	// An unserved path still gets the helpful 404 naming the real endpoints.
	req := httptest.NewRequest(http.MethodPost, "/elsewhere", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/api/mcp") {
		t.Errorf("the 404 should name the endpoints: %q", rec.Body.String())
	}
}

func TestServeAllRunsBothEndpoints(t *testing.T) {
	s, _ := newTestServer(t)
	cfg := DefaultConfig()
	cfg.Server.AdditionalURLs = []string{
		"http://127.0.0.1:18765/mcp",
		"https://127.0.0.1:18766/mcp",
	}
	cfg.Server.TLSSelfSigned = true
	tlsConfig, err := cfg.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	listeners, err := cfg.Listeners()
	if err != nil {
		t.Fatal(err)
	}
	tr := NewHTTPTransport(s, cfg.Server, "/mcp", log.New(io.Discard, "", 0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tr.ServeAll(ctx, listeners, tlsConfig) }()
	defer func() {
		cancel()
		<-done
	}()

	waitForPort(t, "127.0.0.1:18765")
	waitForPort(t, "127.0.0.1:18766")

	// The plain endpoint answers over http.
	body := bytes.NewReader(rpcBody(t, "tools/list", nil))
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:18765/mcp", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	req.Header.Set("Mcp-Method", "tools/list")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http endpoint: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("http endpoint returned %d", resp.StatusCode)
	}

	// The other answers the same request over TLS, on the same server.
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // a self-signed test certificate
	}}
	body = bytes.NewReader(rpcBody(t, "tools/list", nil))
	req, _ = http.NewRequest(http.MethodPost, "https://127.0.0.1:18766/mcp", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	req.Header.Set("Mcp-Method", "tools/list")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("https endpoint: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("https endpoint returned %d", resp.StatusCode)
	}
}

func TestServeAllFailsWhenAPortIsTaken(t *testing.T) {
	// Half a server is worse than none: if one listener cannot bind, the whole
	// thing must fail rather than come up silently incomplete.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	taken := blocker.Addr().String()

	s, _ := newTestServer(t)
	tr := NewHTTPTransport(s, DefaultConfig().Server, "/mcp", log.New(io.Discard, "", 0))
	listeners := []Listener{
		{Addr: "127.0.0.1:18767", Paths: []string{"/mcp"}},
		{Addr: taken, Paths: []string{"/mcp"}},
	}
	err = tr.ServeAll(context.Background(), listeners, nil)
	if err == nil {
		t.Fatal("binding a taken port should fail the whole server")
	}
	if !strings.Contains(err.Error(), taken) {
		t.Errorf("the error should name the address that failed: %v", err)
	}
}

// waitForPort blocks until something is listening, so a test does not race the
// server's startup.
func waitForPort(t *testing.T, addr string) {
	t.Helper()
	for range 100 {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}

func TestExampleConfigIsValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Written with a BOM, which is what a PowerShell redirect produces.
	if err := os.WriteFile(path, append([]byte{0xEF, 0xBB, 0xBF}, ExampleConfig...), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(dir); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Commands) == 0 {
		t.Error("the example config should define commands")
	}
	if cfg.Server.URL == "" {
		t.Error("the example config should name a URL")
	}
}

func TestDSNRedactsThePassword(t *testing.T) {
	db := DatabaseConfig{
		Enabled: true, Host: "db.example", Port: 5432,
		User: "app", Password: "hunter2", Database: "prod", SSLMode: "require",
	}
	dsn := db.DSN()
	if !strings.Contains(dsn, "hunter2") {
		t.Fatalf("DSN should carry the password: %s", dsn)
	}
	if strings.Contains(RedactDSN(dsn), "hunter2") {
		t.Errorf("RedactDSN left the password in: %s", RedactDSN(dsn))
	}
}

func TestDSNEmptyWhenNotConfigured(t *testing.T) {
	if dsn := (DatabaseConfig{}).DSN(); dsn != "" {
		t.Errorf("a disabled database should have no DSN, got %q", dsn)
	}
	if dsn := (DatabaseConfig{Enabled: true}).DSN(); dsn != "" {
		t.Errorf("an enabled but credential-less database should have no DSN, got %q", dsn)
	}
}

func TestReadOnlyStatementDetection(t *testing.T) {
	readable := []string{"SELECT 1", "  select * from t ", "WITH x AS (SELECT 1) SELECT * FROM x", "EXPLAIN SELECT 1"}
	writable := []string{"DELETE FROM t", "UPDATE t SET a = 1", "SELECT 1; DROP TABLE t", "insert into t values (1)"}
	for _, sql := range readable {
		if !isReadOnlyStatement(sql) {
			t.Errorf("%q should be read-only", sql)
		}
	}
	for _, sql := range writable {
		if isReadOnlyStatement(sql) {
			t.Errorf("%q should not be treated as read-only", sql)
		}
	}
}

func TestGlobToRegexp(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"**/*.go", "cmd/app/main.go", true},
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
		{"cmd/*/main.go", "cmd/app/main.go", true},
		{"cmd/*/main.go", "cmd/a/b/main.go", false},
	}
	for _, tc := range cases {
		re, err := globToRegexp(tc.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := re.MatchString(tc.path); got != tc.want {
			t.Errorf("%q vs %q = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// --- stdio ----------------------------------------------------------------

func TestStdioTransport(t *testing.T) {
	s, _ := newTestServer(t)
	in := bytes.NewReader(rpcBody(t, "server/discover", nil))
	var out bytes.Buffer
	tr := NewStdioTransport(s, in, &out, true, log.New(io.Discard, "", 0))
	if err := tr.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  *RPCError      `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("stdio wrote %q: %v", out.String(), err)
	}
	if resp.Error != nil {
		t.Fatalf("discover failed: %v", resp.Error)
	}
	if resp.Result["resultType"] != ResultComplete {
		t.Errorf("resultType = %v", resp.Result["resultType"])
	}
}

// --- prompts and resources ------------------------------------------------

func TestPromptsListAndGet(t *testing.T) {
	s, _ := newTestServer(t)
	list := call(t, s, "prompts/list", nil)
	prompts, _ := list["prompts"].([]any)
	if len(prompts) == 0 {
		t.Fatal("no prompts")
	}
	got := call(t, s, "prompts/get", map[string]any{
		"name":      "ship_change",
		"arguments": map[string]string{"message": "fix the thing"},
	})
	messages, _ := got["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	content, _ := first["content"].(map[string]any)
	text, _ := content["text"].(string)
	if !strings.Contains(text, "fix the thing") {
		t.Errorf("the prompt did not use its argument: %q", text)
	}
	if !strings.Contains(text, "github_run_watch") {
		t.Errorf("ship_change should walk through the deployment: %q", text)
	}
}

func TestResourcesListAndRead(t *testing.T) {
	s, _ := newTestServer(t)
	list := call(t, s, "resources/list", nil)
	resources, _ := list["resources"].([]any)
	if len(resources) == 0 {
		t.Fatal("no resources listed")
	}
	first, _ := resources[0].(map[string]any)
	got := call(t, s, "resources/read", map[string]any{"uri": first["uri"]})
	contents, _ := got["contents"].([]any)
	body, _ := contents[0].(map[string]any)
	if !strings.Contains(body["text"].(string), "one") {
		t.Errorf("resource body = %q", body["text"])
	}
}
