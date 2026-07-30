package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The REST face has to satisfy a client that was never told about MCP: a
// specification it can read, one POST per tool, and answers in a shape it can
// describe. These tests are written against what Open WebUI actually needs -
// openapi.json, an operationId on every operation, CORS, bearer auth.

// apiServer builds a server with the REST face on, and the HTTP handler it is
// mounted in.
func apiServer(t *testing.T, adjust func(*Config)) (*Server, http.Handler) {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Workspace.Root = root
	cfg.Server.Transport = "http"
	cfg.Commands = []CommandConfig{{Name: "build", Description: "Build it.", Command: "echo built"}}
	if adjust != nil {
		adjust(&cfg)
	}
	if err := cfg.Normalize(root); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg.Server.Name, "test", cfg.Server.Instructions, cfg.Server.LegacyCompatibility)
	s.cfg = cfg
	s.ws = NewWorkspace(cfg.Workspace)
	s.registerAll(cfg)
	s.api = newOpenAPIServer(s, cfg)

	transport := NewHTTPTransport(s, cfg.Server, "/mcp", nil)
	write(t, root, "hello.txt", "one\ntwo\n")
	return s, transport.HandlerFor("/mcp")
}

// apiCall sends one request to the REST face and returns the status and the
// decoded JSON body.
func apiCall(t *testing.T, h http.Handler, method, path string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("%s %s: body is not JSON: %s", method, path, rec.Body.String())
		}
	}
	return rec.Code, decoded
}

func TestOpenAPISpecIsServed(t *testing.T) {
	_, h := apiServer(t, nil)

	status, doc := apiCall(t, h, http.MethodGet, "/api/openapi.json", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if version, _ := doc["openapi"].(string); !strings.HasPrefix(version, "3.1") {
		t.Errorf("openapi = %v, want 3.1.x", doc["openapi"])
	}
	info, _ := doc["info"].(map[string]any)
	if info["title"] == "" || info["version"] == nil {
		t.Errorf("info is incomplete: %v", info)
	}
	servers, _ := doc["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("servers = %v, want one entry", doc["servers"])
	}
	// The spec must name the address the request arrived on, or a client
	// behind a proxy calls the wrong host.
	if url, _ := servers[0].(map[string]any)["url"].(string); !strings.HasSuffix(url, "/api") {
		t.Errorf("servers[0].url = %q, want it to end in the API prefix", url)
	}
}

// TestOpenAPIEveryOperationIsUsable is the checklist a tool-server client
// works from: a POST, an operationId, a described request body, a response.
func TestOpenAPIEveryOperationIsUsable(t *testing.T) {
	s, h := apiServer(t, nil)

	_, doc := apiCall(t, h, http.MethodGet, "/api/openapi.json", nil, nil)
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) != len(s.ToolNames()) {
		t.Errorf("spec has %d paths for %d tools", len(paths), len(s.ToolNames()))
	}
	seen := map[string]bool{}
	for path, item := range paths {
		operations, _ := item.(map[string]any)
		post, ok := operations["post"].(map[string]any)
		if !ok {
			t.Errorf("%s has no post operation", path)
			continue
		}
		id, _ := post["operationId"].(string)
		if id == "" {
			t.Errorf("%s has no operationId, which is what names the tool", path)
		}
		if seen[id] {
			t.Errorf("operationId %q is used twice", id)
		}
		seen[id] = true
		if "/"+id != path {
			t.Errorf("path %s and operationId %q disagree", path, id)
		}
		if _, ok := post["description"].(string); !ok {
			t.Errorf("%s has no description for the model to read", path)
		}
		requestBody, _ := post["requestBody"].(map[string]any)
		content, _ := requestBody["content"].(map[string]any)
		if _, ok := content["application/json"]; !ok {
			t.Errorf("%s does not take a JSON body", path)
		}
		responses, _ := post["responses"].(map[string]any)
		if _, ok := responses["200"]; !ok {
			t.Errorf("%s describes no successful response", path)
		}
	}
	if !seen["read_file"] || !seen["build"] {
		t.Errorf("both built-in tools and project commands should be exposed: %v", seen)
	}
}

// TestOpenAPISchemasComeFromTheTools: 3.1 takes JSON Schema as it is, so the
// tool's own schema is what the client sees - no second description to drift.
func TestOpenAPISchemasComeFromTheTools(t *testing.T) {
	_, h := apiServer(t, nil)

	_, doc := apiCall(t, h, http.MethodGet, "/api/openapi.json", nil, nil)
	paths, _ := doc["paths"].(map[string]any)
	readFile, _ := paths["/read_file"].(map[string]any)
	post, _ := readFile["post"].(map[string]any)
	body, _ := post["requestBody"].(map[string]any)
	content, _ := body["content"].(map[string]any)
	appJSON, _ := content["application/json"].(map[string]any)
	schema, _ := appJSON["schema"].(map[string]any)

	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties["path"]; !ok {
		t.Errorf("read_file's schema lost its path property: %v", schema)
	}
	required, _ := schema["required"].([]any)
	if len(required) == 0 || required[0] != "path" {
		t.Errorf("read_file's schema lost its required list: %v", schema)
	}
	if body["required"] != true {
		t.Error("a tool with a required argument should have a required request body")
	}
}

func TestOpenAPICallsATool(t *testing.T) {
	_, h := apiServer(t, nil)

	status, body := apiCall(t, h, http.MethodPost, "/api/read_file",
		map[string]any{"path": "hello.txt"}, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	if result, _ := body["result"].(string); result != "one\ntwo\n" {
		t.Errorf("result = %q, want the file's contents", body["result"])
	}
	if body["is_error"] == true {
		t.Errorf("a successful read should not be an error: %v", body)
	}
}

// TestOpenAPIToolErrorIsNotATransportError: a tool that refuses answers 200
// with is_error, because a 4xx reads to a model as the connection failing.
func TestOpenAPIToolErrorIsNotATransportError(t *testing.T) {
	_, h := apiServer(t, nil)

	status, body := apiCall(t, h, http.MethodPost, "/api/read_file",
		map[string]any{"path": "nothing-here.txt"}, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 with is_error", status)
	}
	if body["is_error"] != true {
		t.Errorf("the refusal should be marked: %v", body)
	}
	if result, _ := body["result"].(string); !strings.Contains(result, "path parameter") {
		t.Errorf("the guidance should survive into the REST answer: %v", body)
	}
}

func TestOpenAPIStructuredContentIsReturned(t *testing.T) {
	_, h := apiServer(t, nil)

	status, body := apiCall(t, h, http.MethodPost, "/api/project_commands", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, body)
	}
	if body["result"] == "" {
		t.Errorf("a tool with no arguments should still answer: %v", body)
	}
}

func TestOpenAPIUnknownToolExplainsItself(t *testing.T) {
	_, h := apiServer(t, nil)

	status, body := apiCall(t, h, http.MethodPost, "/api/no_such_tool", map[string]any{}, nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "openapi.json") &&
		!strings.Contains(detail, "no no_such_tool tool") {
		t.Errorf("the answer should say what to do: %v", body)
	}
}

func TestOpenAPIDisabledToolExplainsItself(t *testing.T) {
	_, h := apiServer(t, func(c *Config) { c.Git.Enabled = false })

	status, body := apiCall(t, h, http.MethodPost, "/api/git_push", map[string]any{}, nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "git.enabled is false") {
		t.Errorf("the answer should name the setting: %v", body)
	}
}

func TestOpenAPIRequiresTheBearerToken(t *testing.T) {
	_, h := apiServer(t, func(c *Config) { c.Server.AuthToken = "s3cret" })

	if status, _ := apiCall(t, h, http.MethodGet, "/api/openapi.json", nil, nil); status != http.StatusUnauthorized {
		t.Errorf("the spec should need the token too, got %d", status)
	}
	if status, _ := apiCall(t, h, http.MethodPost, "/api/read_file",
		map[string]any{"path": "hello.txt"}, nil); status != http.StatusUnauthorized {
		t.Errorf("a call without the token should be refused, got %d", status)
	}
	authorized := map[string]string{"Authorization": "Bearer s3cret"}
	status, doc := apiCall(t, h, http.MethodGet, "/api/openapi.json", nil, authorized)
	if status != http.StatusOK {
		t.Fatalf("the token should be accepted, got %d", status)
	}
	// A client reading the spec has to be told it needs the token.
	components, _ := doc["components"].(map[string]any)
	if _, ok := components["securitySchemes"]; !ok {
		t.Errorf("the spec should declare its security scheme: %v", components)
	}
	if _, ok := doc["security"]; !ok {
		t.Errorf("the spec should require the scheme: %v", doc)
	}
	if status, body := apiCall(t, h, http.MethodPost, "/api/read_file",
		map[string]any{"path": "hello.txt"}, authorized); status != http.StatusOK {
		t.Errorf("an authorized call should work, got %d: %v", status, body)
	}
}

func TestOpenAPIWithoutATokenDeclaresNoSecurity(t *testing.T) {
	_, h := apiServer(t, nil)

	_, doc := apiCall(t, h, http.MethodGet, "/api/openapi.json", nil, nil)
	if _, ok := doc["security"]; ok {
		t.Errorf("an unauthenticated server should not claim to need a token: %v", doc["security"])
	}
}

// TestOpenAPIAnswersCORS: Open WebUI calls the tool server from the browser,
// so the preflight has to succeed.
func TestOpenAPIAnswersCORS(t *testing.T) {
	// The browser origin has to be allowed, exactly as it does for the MCP
	// endpoint: the origin check is this server's DNS-rebinding defence and
	// the REST face does not get to skip it.
	_, h := apiServer(t, func(c *Config) {
		c.Server.AllowedOrigins = append(c.Server.AllowedOrigins, "https://webui.example.com")
	})

	req := httptest.NewRequest(http.MethodOptions, "/api/read_file", nil)
	req.Header.Set("Origin", "https://webui.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type, authorization")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Error("the preflight should allow the origin")
	}
	if !strings.Contains(strings.ToLower(rec.Header().Get("Access-Control-Allow-Methods")), "post") {
		t.Errorf("POST should be allowed: %q", rec.Header().Get("Access-Control-Allow-Methods"))
	}
}

// TestOpenAPIRefusesAnUnlistedOrigin is the other half: a page nobody
// configured cannot reach the tools through a browser.
func TestOpenAPIRefusesAnUnlistedOrigin(t *testing.T) {
	_, h := apiServer(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/read_file", strings.NewReader(`{"path":"hello.txt"}`))
	req.Header.Set("Origin", "https://somewhere-else.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for an origin that is not allowed", rec.Code)
	}
}

func TestOpenAPIIndexListsTheTools(t *testing.T) {
	_, h := apiServer(t, nil)

	status, body := apiCall(t, h, http.MethodGet, "/api", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if url, _ := body["openapi_url"].(string); !strings.HasSuffix(url, "/api/openapi.json") {
		t.Errorf("the index should point at the spec: %v", body)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) == 0 {
		t.Errorf("the index should list the tools: %v", body)
	}
}

func TestOpenAPIIncludeAndExclude(t *testing.T) {
	_, h := apiServer(t, func(c *Config) {
		c.OpenAPI.Include = []string{"read_file", "grep_files", "write_file"}
		c.OpenAPI.Exclude = []string{"write_file"}
	})

	_, doc := apiCall(t, h, http.MethodGet, "/api/openapi.json", nil, nil)
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) != 2 {
		t.Errorf("paths = %v, want read_file and grep_files only", paths)
	}
	if _, ok := paths["/write_file"]; ok {
		t.Error("exclude should win over include")
	}
	// A tool left out is not reachable either, or the list would be decoration.
	if status, _ := apiCall(t, h, http.MethodPost, "/api/write_file",
		map[string]any{"path": "x.txt", "content": "x"}, nil); status != http.StatusNotFound {
		t.Errorf("an excluded tool should not be callable, got %d", status)
	}
}

func TestOpenAPIPathIsConfigurable(t *testing.T) {
	_, h := apiServer(t, func(c *Config) { c.OpenAPI.Path = "tools/" })

	if status, _ := apiCall(t, h, http.MethodGet, "/tools/openapi.json", nil, nil); status != http.StatusOK {
		t.Errorf("the spec should be under the configured prefix, got %d", status)
	}
	if status, _ := apiCall(t, h, http.MethodPost, "/tools/read_file",
		map[string]any{"path": "hello.txt"}, nil); status != http.StatusOK {
		t.Errorf("a tool should be under the configured prefix, got %d", status)
	}
}

func TestOpenAPIRejectsTheWrongMethod(t *testing.T) {
	_, h := apiServer(t, nil)

	status, body := apiCall(t, h, http.MethodGet, "/api/read_file", nil, nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "POST") {
		t.Errorf("the answer should say how to call it: %v", body)
	}
}

func TestOpenAPIRejectsABadBody(t *testing.T) {
	_, h := apiServer(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/read_file", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestOpenAPIOffServesNothing: the MCP endpoint still works, and the REST face
// is simply not there.
func TestOpenAPIOffServesNothing(t *testing.T) {
	_, h := apiServer(t, func(c *Config) { c.OpenAPI.Enabled = false })

	req := httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("the API should not answer when it is off: %s", rec.Body.String())
	}
}

func TestOpenAPIPublicURLOverridesTheHost(t *testing.T) {
	_, h := apiServer(t, func(c *Config) { c.OpenAPI.PublicURL = "https://tools.example.com/api/" })

	_, doc := apiCall(t, h, http.MethodGet, "/api/openapi.json", nil, nil)
	servers, _ := doc["servers"].([]any)
	first, _ := servers[0].(map[string]any)
	if first["url"] != "https://tools.example.com/api" {
		t.Errorf("servers[0].url = %v, want the configured public URL without its trailing slash", first["url"])
	}
}

func TestOpenAPIForwardedHeadersDecideTheServerURL(t *testing.T) {
	_, h := apiServer(t, nil)

	_, doc := apiCall(t, h, http.MethodGet, "/api/openapi.json", nil, map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "tunnel.example.com",
	})
	servers, _ := doc["servers"].([]any)
	first, _ := servers[0].(map[string]any)
	if first["url"] != "https://tunnel.example.com/api" {
		t.Errorf("servers[0].url = %v, want the forwarded address", first["url"])
	}
}

func TestOpenAPIConfigNormalizesItsPath(t *testing.T) {
	cases := map[string]string{"": "/api", "/api/": "/api", "api": "/api", "tools/v1/": "/tools/v1"}
	for given, want := range cases {
		cfg := OpenAPIConfig{Path: given}
		cfg.normalizePath()
		if cfg.Path != want {
			t.Errorf("normalizePath(%q) = %q, want %q", given, cfg.Path, want)
		}
	}
}

// TestOpenAPIReloadChangesWhatIsExposed: the routes are fixed once the mux is
// built, but which tools answer on them is re-read, so an edited config.json
// takes effect without a restart.
func TestOpenAPIReloadChangesWhatIsExposed(t *testing.T) {
	s, h := apiServer(t, nil)

	if status, _ := apiCall(t, h, http.MethodPost, "/api/read_file",
		map[string]any{"path": "hello.txt"}, nil); status != http.StatusOK {
		t.Fatalf("read_file should start out exposed, got %d", status)
	}

	next := s.cfg
	next.OpenAPI.Exclude = []string{"read_file"}
	if err := s.applyConfig(next); err != nil {
		t.Fatal(err)
	}
	if status, _ := apiCall(t, h, http.MethodPost, "/api/read_file",
		map[string]any{"path": "hello.txt"}, nil); status != http.StatusNotFound {
		t.Errorf("the reloaded exclude list should have taken effect, got %d", status)
	}
	_, doc := apiCall(t, h, http.MethodGet, "/api/openapi.json", nil, nil)
	paths, _ := doc["paths"].(map[string]any)
	if _, ok := paths["/read_file"]; ok {
		t.Error("the specification should have dropped the excluded tool")
	}
}

// TestOpenAPIPrefixSurvivesAReload: the mount point cannot move under a
// running mux, so a reload must not make the handler think it has.
func TestOpenAPIPrefixSurvivesAReload(t *testing.T) {
	s, h := apiServer(t, nil)

	next := s.cfg
	next.OpenAPI.Path = "/somewhere-else"
	if err := s.applyConfig(next); err != nil {
		t.Fatal(err)
	}
	if status, _ := apiCall(t, h, http.MethodGet, "/api/openapi.json", nil, nil); status != http.StatusOK {
		t.Errorf("the API should still answer on the prefix it was mounted at, got %d", status)
	}
}
