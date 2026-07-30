package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// The same tools, served as a plain REST API with an OpenAPI description, for
// clients that speak that instead of MCP - Open WebUI's tool servers being the
// one this follows. Its convention is a POST per tool at the root of the
// server, a JSON body of the tool's arguments, and the specification at
// openapi.json, with an explicit operationId on every operation because that
// is the name the model is given.
//
// Nothing here is a second implementation of anything: a request is decoded,
// handed to the same tool handler the MCP endpoint calls, and encoded back.
// The two front doors cannot drift apart because there is only one room.

// OpenAPIConfig governs the REST face of the server.
type OpenAPIConfig struct {
	Enabled bool `json:"enabled"`
	// Path is the prefix the API is mounted under, so the specification is at
	// <path>/openapi.json and a tool at <path>/<tool name>.
	Path string `json:"path,omitempty"`

	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`

	// PublicURL is what the specification names as its server. It is normally
	// left empty, in which case each request is answered with the address it
	// arrived on - which is what makes the spec correct behind a tunnel or a
	// reverse proxy without being told about either.
	PublicURL string `json:"public_url,omitempty"`

	// Include, when set, is the only tools exposed. Exclude removes tools from
	// whatever Include left. Names not registered are ignored.
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

// normalizePath fills in the default prefix and strips a trailing slash, so
// "/api/" and "/api" mean the same thing and "" means the default.
func (c *OpenAPIConfig) normalizePath() {
	c.Path = strings.TrimRight(strings.TrimSpace(c.Path), "/")
	if c.Path == "" {
		c.Path = "/api"
	}
	if !strings.HasPrefix(c.Path, "/") {
		c.Path = "/" + c.Path
	}
}

// openAPIServer answers the REST requests for one server.
type openAPIServer struct {
	server *Server
	// prefix is the mount point. It is fixed once the mux is built, so it is
	// read without the lock and never reassigned.
	prefix string

	mu  sync.RWMutex
	cfg OpenAPIConfig
	// auth is the bearer token the MCP endpoint requires, applied here too: a
	// second door into the same tools must not be an easier one.
	auth string
}

// settings returns the configuration and token under the lock, since a reload
// can replace both while a request is in flight.
func (a *openAPIServer) settings() (OpenAPIConfig, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg, a.auth
}

// newOpenAPIServer builds the REST face, or nil when it is switched off.
func newOpenAPIServer(s *Server, cfg Config) *openAPIServer {
	if !cfg.OpenAPI.Enabled {
		return nil
	}
	api := cfg.OpenAPI
	api.normalizePath()
	if api.Title == "" {
		api.Title = cfg.Server.Name
	}
	if api.Version == "" {
		api.Version = cfg.Server.Version
	}
	if api.Description == "" {
		api.Description = cfg.Server.Instructions
	}
	return &openAPIServer{server: s, prefix: api.Path, cfg: api, auth: cfg.Server.AuthToken}
}

// update applies a reloaded configuration. The routes are fixed once the mux
// is built, so the prefix is kept and everything that can change without
// moving a route - which tools are exposed, how the API describes itself -
// takes effect on the next request.
func (a *openAPIServer) update(cfg Config) {
	if a == nil {
		return
	}
	next := cfg.OpenAPI
	next.Path = a.prefix
	if next.Title == "" {
		next.Title = cfg.Server.Name
	}
	if next.Version == "" {
		next.Version = cfg.Server.Version
	}
	if next.Description == "" {
		next.Description = cfg.Server.Instructions
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg = next
	a.auth = cfg.Server.AuthToken
}

// register mounts the API on a mux. It is called once per listener, not once
// per MCP path, since a mux refuses a pattern twice.
func (a *openAPIServer) register(mux *http.ServeMux) {
	prefix := a.prefix
	mux.HandleFunc(prefix+"/openapi.json", a.serveSpec)
	// FastAPI serves its specification at both of these, and a client pointed
	// at the root of the server should find something rather than a 404.
	mux.HandleFunc(prefix+"/", a.serveTool)
	mux.HandleFunc(prefix, a.serveIndex)
}

// exposes reports whether a tool is part of the REST surface.
func (a *openAPIServer) exposes(name string) bool {
	cfg, _ := a.settings()
	if len(cfg.Include) > 0 && !slicesContains(cfg.Include, name) {
		return false
	}
	return !slicesContains(cfg.Exclude, name)
}

func slicesContains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// authorized applies the same bearer check the MCP endpoint uses.
func (a *openAPIServer) authorized(w http.ResponseWriter, r *http.Request) bool {
	_, auth := a.settings()
	if auth == "" || bearerTokenMatches(r.Header.Get("Authorization"), auth) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"detail": "authentication required: send Authorization: Bearer <token>",
	})
	return false
}

// baseURL is what the specification should name as its server: the address the
// request actually arrived on, so a tunnel or a proxy needs no configuration.
func (a *openAPIServer) baseURL(r *http.Request) string {
	cfg, _ := a.settings()
	if cfg.PublicURL != "" {
		return strings.TrimRight(cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	return fmt.Sprintf("%s://%s%s", scheme, host, a.prefix)
}

// apiToolResponse is what every tool call answers with. One shape for every
// endpoint means the specification can describe it once, and a model reading
// the result does not have to work out which kind of answer it got.
type apiToolResponse struct {
	// Result is the tool's text output, which is what a model reads.
	Result string `json:"result"`
	// Data is the structured content, when the tool produced any.
	Data any `json:"data,omitempty"`
	// IsError marks a tool that refused or failed. The HTTP status stays 200:
	// the call reached the tool and the tool answered, and a model reads a 4xx
	// as the connection failing rather than as something it can act on.
	IsError bool `json:"is_error,omitempty"`
}

// serveTool answers a call to <prefix>/<tool name>.
func (a *openAPIServer) serveTool(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, a.prefix), "/")
	switch {
	case name == "":
		a.serveIndex(w, r)
		return
	case name == "openapi.json":
		a.serveSpec(w, r)
		return
	}
	if !a.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"detail": fmt.Sprintf("%s is called with POST and a JSON body of its arguments", name),
		})
		return
	}

	tool, registered := a.server.lookupTool(name)
	if !registered || !a.exposes(name) {
		// The same answer the MCP endpoint gives, for the same reason: a name
		// that is off or misspelled should say what to do instead.
		detail := a.server.unknownToolMessage(name)
		if info, off := a.server.unavailableFor(name); off {
			detail = info.message(name)
		} else if registered {
			detail = fmt.Sprintf("the %s tool is not exposed on this API (openapi.include/openapi.exclude in "+
				"config.json decide what is). Nothing was changed", name)
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": detail})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"detail": fmt.Sprintf("could not read the request body: %v", err),
		})
		return
	}
	arguments := json.RawMessage(body)
	if len(strings.TrimSpace(string(body))) == 0 {
		// A tool with no arguments is reasonably called with no body at all.
		arguments = json.RawMessage(`{}`)
	} else if !json.Valid(body) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"detail": "the request body must be a JSON object of the tool's arguments",
		})
		return
	}

	// The modern protocol version, so results carry the same envelope the MCP
	// endpoint produces, and runTool so a panic is an answer rather than a
	// dropped connection.
	ctx := withVersion(r.Context(), ProtocolVersion)
	res, rpcErr := a.server.runTool(ctx, name, tool.handler, arguments)
	if rpcErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": rpcErr.Message})
		return
	}
	writeJSON(w, http.StatusOK, apiToolResponse{
		Result:  resultText(res),
		Data:    res.StructuredContent,
		IsError: res.IsError,
	})
}

// resultText flattens a tool result's content blocks into the text a model
// reads.
func resultText(res *CallToolResult) string {
	if res == nil {
		return ""
	}
	var parts []string
	for _, block := range res.Content {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// serveSpec renders the OpenAPI document.
func (a *openAPIServer) serveSpec(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, a.spec(a.baseURL(r)))
}

// spec builds the OpenAPI 3.1 document from the registered tools. The tool
// input schemas are JSON Schema already, and 3.1 takes JSON Schema as it is,
// so they are carried across rather than translated - there is nothing to get
// wrong in the copy.
func (a *openAPIServer) spec(base string) map[string]any {
	cfg, auth := a.settings()
	paths := map[string]any{}
	for _, name := range a.server.ToolNames() {
		if !a.exposes(name) {
			continue
		}
		tool, ok := a.server.lookupTool(name)
		if !ok {
			continue
		}
		paths["/"+name] = map[string]any{
			"post": a.operation(tool.def),
		}
	}

	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       cfg.Title,
			"description": cfg.Description,
			"version":     firstNonEmpty(cfg.Version, "0.0.0"),
		},
		"servers": []any{map[string]any{"url": base}},
		"paths":   paths,
		"components": map[string]any{
			"schemas": map[string]any{"ToolResult": toolResultSchema()},
		},
	}
	if auth != "" {
		doc["components"].(map[string]any)["securitySchemes"] = map[string]any{
			"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"},
		}
		doc["security"] = []any{map[string]any{"bearerAuth": []any{}}}
	}
	return doc
}

// operation describes one tool as an OpenAPI operation.
func (a *openAPIServer) operation(tool Tool) map[string]any {
	summary := tool.Title
	if summary == "" {
		summary = tool.Name
	}
	// A tool with no arguments still needs a schema, or a client has nothing
	// to describe the (empty) body with.
	schema := any(tool.InputSchema)
	if len(tool.InputSchema) == 0 {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	op := map[string]any{
		// Required by Open WebUI, and the name the model is given for the
		// tool, so it is the tool's own name and nothing else.
		"operationId": tool.Name,
		"summary":     summary,
		"description": tool.Description,
		"requestBody": map[string]any{
			"required": len(requiredOf(tool.InputSchema)) > 0,
			"content": map[string]any{
				"application/json": map[string]any{"schema": schema},
			},
		},
		"responses": map[string]any{
			"200": map[string]any{
				"description": "The tool ran. is_error marks a tool that refused or failed.",
				"content": map[string]any{
					"application/json": map[string]any{
						"schema": map[string]any{"$ref": "#/components/schemas/ToolResult"},
					},
				},
			},
		},
	}
	if tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
		op["x-openai-isConsequential"] = false
	} else if tool.Annotations != nil && (tool.Annotations.DestructiveHint || tool.Annotations.OpenWorldHint) {
		// Named for the clients that read it as "ask the user first".
		op["x-openai-isConsequential"] = true
	}
	return op
}

// requiredOf reads the required list out of a tool's input schema.
func requiredOf(schema map[string]any) []string {
	required, _ := schema["required"].([]string)
	if len(required) > 0 {
		return required
	}
	if list, ok := schema["required"].([]any); ok {
		out := make([]string, 0, len(list))
		for _, item := range list {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

func toolResultSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []any{"result"},
		"properties": map[string]any{
			"result": map[string]any{
				"type":        "string",
				"description": "The tool's output, as text.",
			},
			"data": map[string]any{
				"type":        "object",
				"description": "The tool's structured output, when it produced any.",
			},
			"is_error": map[string]any{
				"type":        "boolean",
				"description": "True when the tool refused or failed. The call itself still succeeded.",
			},
		},
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// serveIndex is what a person pasting the base URL into a browser gets: the
// tools, and where the specification is.
func (a *openAPIServer) serveIndex(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(w, r) {
		return
	}
	type entry struct {
		Name        string `json:"name"`
		Summary     string `json:"summary,omitempty"`
		Description string `json:"description,omitempty"`
		Path        string `json:"path"`
	}
	var tools []entry
	for _, name := range a.server.ToolNames() {
		if !a.exposes(name) {
			continue
		}
		tool, ok := a.server.lookupTool(name)
		if !ok {
			continue
		}
		tools = append(tools, entry{
			Name:        name,
			Summary:     tool.def.Title,
			Description: tool.def.Description,
			Path:        a.prefix + "/" + name,
		})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	cfg, _ := a.settings()
	writeJSON(w, http.StatusOK, map[string]any{
		"title":       cfg.Title,
		"version":     cfg.Version,
		"openapi_url": a.baseURL(r) + "/openapi.json",
		"tool_count":  len(tools),
		"tools":       tools,
	})
}
